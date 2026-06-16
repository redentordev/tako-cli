package deployer

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/mesh"
	"github.com/redentordev/tako-cli/pkg/provisioner"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	"github.com/redentordev/tako-cli/pkg/secrets"
	"github.com/redentordev/tako-cli/pkg/serviceimport"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/redentordev/tako-cli/pkg/unregistry"
	"github.com/redentordev/tako-cli/pkg/utils"
)

type takodAssignment struct {
	ServerName string
	Slot       int
}

type takodNodeState struct {
	Runtime     string            `json:"runtime"`
	Project     string            `json:"project"`
	Environment string            `json:"environment"`
	Node        string            `json:"node"`
	Host        string            `json:"host"`
	Mesh        takodMeshState    `json:"mesh"`
	Labels      map[string]string `json:"labels,omitempty"`
	UpdatedAt   time.Time         `json:"updatedAt"`
}

type takodMeshState struct {
	Enabled      bool   `json:"enabled"`
	NetworkCIDR  string `json:"networkCIDR"`
	Address      string `json:"address"`
	Interface    string `json:"interface"`
	ListenPort   int    `json:"listenPort"`
	SubnetBits   int    `json:"subnetBits"`
	NATTraversal bool   `json:"natTraversal"`
	PublicKey    string `json:"publicKey,omitempty"`
	Endpoint     string `json:"endpoint,omitempty"`
}

type takodMeshPeer struct {
	Name      string            `json:"name"`
	Host      string            `json:"host"`
	Address   string            `json:"address"`
	PublicKey string            `json:"publicKey,omitempty"`
	Endpoint  string            `json:"endpoint,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type takodMeshDesiredState struct {
	Project     string          `json:"project"`
	Environment string          `json:"environment"`
	NetworkCIDR string          `json:"networkCIDR"`
	Interface   string          `json:"interface"`
	ListenPort  int             `json:"listenPort"`
	Peers       []takodMeshPeer `json:"peers"`
	UpdatedAt   time.Time       `json:"updatedAt"`
}

// SetupTakodRuntime prepares every selected environment server for the takod runtime.
func (d *Deployer) SetupTakodRuntime() error {
	if d.sshPool == nil {
		return fmt.Errorf("ssh pool not initialized")
	}

	serviceServers, err := d.getTakodTargetServers()
	if err != nil {
		return fmt.Errorf("failed to get takod target servers: %w", err)
	}
	if len(serviceServers) == 0 {
		return fmt.Errorf("environment %s has no target servers", d.environment)
	}
	meshServers, err := d.getTakodMeshServers()
	if err != nil {
		return fmt.Errorf("failed to get takod mesh servers: %w", err)
	}

	if d.verbose {
		fmt.Printf("\n-> Preparing takod mesh runtime...\n")
	}

	if err := d.ensureTakodAgents(meshServers); err != nil {
		return err
	}

	nodePublicKeys, err := d.ensureTakodMeshKeys(meshServers)
	if err != nil {
		return err
	}

	peers, err := d.buildTakodMeshPeers(meshServers, nodePublicKeys)
	if err != nil {
		return err
	}

	if err := d.prepareTakodNodes(meshServers, peers, nodePublicKeys); err != nil {
		return err
	}

	if d.verbose {
		fmt.Printf("  ✓ takod runtime prepared on %d node(s)\n", len(meshServers))
	}

	return nil
}

func (d *Deployer) ensureTakodAgents(servers []string) error {
	return runTakodNodeActions(servers, func(serverName string) error {
		server, ok := d.config.Servers[serverName]
		if !ok {
			return fmt.Errorf("server %s not found", serverName)
		}
		client, err := d.sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return fmt.Errorf("failed to connect to %s: %w", serverName, err)
		}

		prov := provisioner.NewProvisioner(client, d.verbose)
		if localBinary := strings.TrimSpace(os.Getenv("TAKO_TAKOD_BINARY")); localBinary != "" {
			if err := prov.InstallTakodBinaryFromFile(localBinary); err != nil {
				return fmt.Errorf("failed to install local takod binary on %s: %w", serverName, err)
			}
		} else if d.shouldInstallTakodRelease(client) {
			if err := prov.InstallTakodBinary(d.cliVersion); err != nil {
				return fmt.Errorf("failed to install takod binary on %s: %w", serverName, err)
			}
		}

		socket := ""
		dataDir := ""
		if d.config.Runtime != nil && d.config.Runtime.Agent != nil {
			socket = d.config.Runtime.Agent.Socket
			dataDir = d.config.Runtime.Agent.DataDir
		}
		if err := prov.InstallTakodService(socket, dataDir, serverName); err != nil {
			return fmt.Errorf("failed to install takod service on %s: %w", serverName, err)
		}
		return nil
	})
}

func (d *Deployer) shouldInstallTakodRelease(client takodclient.RequestExecutor) bool {
	version := strings.TrimSpace(d.cliVersion)
	if version == "" || version == "dev" {
		return false
	}

	output, err := takodclient.RequestJSON(client, d.takodSocket(), "GET", "/v1/status", nil)
	if err != nil {
		return true
	}
	var status takod.Status
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		return true
	}
	return strings.TrimSpace(status.Version) != version
}

// DeployServiceTakod reconciles one service through standalone Docker containers
// on the nodes chosen by takod placement.
func (d *Deployer) DeployServiceTakod(serviceName string, service *config.ServiceConfig, imageRef string) error {
	if d.sshPool == nil {
		return fmt.Errorf("ssh pool not initialized")
	}

	assignments, err := d.planTakodAssignments(serviceName, service)
	if err != nil {
		return err
	}

	assignmentServers := uniqueAssignmentServers(assignments)
	targetServers, err := d.getTakodTargetServers()
	if err != nil {
		return fmt.Errorf("failed to get takod target servers: %w", err)
	}
	sourceServerName := ""
	if len(targetServers) > 0 {
		sourceServerName = targetServers[0]
	}
	if service.Build != "" && shouldDistributeBuiltImage(assignmentServers, sourceServerName) {
		if d.distributedImages != nil && d.distributedImages[imageRef] {
			if d.verbose {
				fmt.Printf("  Image already distributed in this run, skipping...\n")
			}
		} else {
			sourceClient, err := d.getSourceEnvironmentClient()
			if err != nil {
				return err
			}
			unreg := unregistry.NewManager(d.config, d.sshPool, d.environment, d.verbose)
			if err := unreg.DistributeImageParallel(sourceClient, imageRef); err != nil {
				return fmt.Errorf("failed to distribute image across takod nodes: %w", err)
			}
			if d.distributedImages != nil {
				d.distributedImages[imageRef] = true
			}
		}
	}

	hookServerName := hookTargetServer(assignments, targetServers)
	if d.deployHooks {
		if err := d.runServiceHookTakod(hookServerName, serviceName, service, imageRef, takod.HookPreDeploy); err != nil {
			return err
		}
	}

	grouped := groupTakodAssignments(assignments)
	deployNode := func(serverName string) error {
		slots := grouped[serverName]
		client, err := d.getEnvironmentClient(serverName)
		if err != nil {
			return err
		}
		if err := d.deployServiceToTakodNode(client, serverName, serviceName, service, imageRef, slots); err != nil {
			return err
		}
		return nil
	}
	if service.Deploy.Strategy == "rolling" {
		for _, serverName := range rollingNodeOrder(targetServers, assignmentServers) {
			if err := deployNode(serverName); err != nil {
				return fmt.Errorf("%s: %w", serverName, err)
			}
		}
	} else {
		if err := runTakodNodeActions(targetServers, deployNode); err != nil {
			return err
		}
	}

	if d.deployHooks {
		if err := d.runServiceHookTakod(hookServerName, serviceName, service, imageRef, takod.HookPostDeploy); err != nil {
			return err
		}
	}

	return nil
}

func rollingNodeOrder(targetServers []string, assignmentServers []string) []string {
	seen := make(map[string]bool, len(targetServers))
	ordered := make([]string, 0, len(targetServers))
	for _, serverName := range assignmentServers {
		if seen[serverName] {
			continue
		}
		seen[serverName] = true
		ordered = append(ordered, serverName)
	}
	for _, serverName := range targetServers {
		if seen[serverName] {
			continue
		}
		seen[serverName] = true
		ordered = append(ordered, serverName)
	}
	return ordered
}

func shouldDistributeBuiltImage(assignmentServers []string, sourceServerName string) bool {
	for _, serverName := range assignmentServers {
		if serverName != sourceServerName {
			return true
		}
	}
	return false
}

func (d *Deployer) RemoveServiceTakod(serviceName string) error {
	if d.sshPool == nil {
		return fmt.Errorf("ssh pool not initialized")
	}

	targetServers, err := d.getTakodTargetServers()
	if err != nil {
		return fmt.Errorf("failed to get takod target servers: %w", err)
	}
	if len(targetServers) == 0 {
		return fmt.Errorf("environment %s has no target servers", d.environment)
	}

	return runTakodNodeActions(targetServers, func(serverName string) error {
		client, err := d.getEnvironmentClient(serverName)
		if err != nil {
			return err
		}
		return d.removeServiceViaTakod(client, takod.RemoveServiceRequest{
			Project:     d.config.Project.Name,
			Environment: d.environment,
			Service:     serviceName,
		})
	})
}

type takodNodeActionResult struct {
	serverName string
	err        error
}

func runTakodNodeActions(targetServers []string, action func(serverName string) error) error {
	resultCh := make(chan takodNodeActionResult, len(targetServers))
	var wg sync.WaitGroup

	for _, serverName := range targetServers {
		wg.Add(1)
		go func(serverName string) {
			defer wg.Done()
			resultCh <- takodNodeActionResult{
				serverName: serverName,
				err:        action(serverName),
			}
		}(serverName)
	}

	wg.Wait()
	close(resultCh)

	var errors []string
	for result := range resultCh {
		if result.err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", result.serverName, result.err))
		}
	}
	if len(errors) > 0 {
		sort.Strings(errors)
		return fmt.Errorf("%s", strings.Join(errors, "; "))
	}
	return nil
}

func (d *Deployer) prepareTakodNode(client *ssh.Client, serverName string, server config.ServerConfig, index int, peers []takodMeshPeer, publicKey string) error {
	meshAddress, err := d.meshAddress(index)
	if err != nil {
		return err
	}
	endpoint := net.JoinHostPort(server.Host, strconv.Itoa(d.config.Mesh.ListenPort))

	nodeState := takodNodeState{
		Runtime:     config.RuntimeModeTakod,
		Project:     d.config.Project.Name,
		Environment: d.environment,
		Node:        serverName,
		Host:        server.Host,
		Mesh: takodMeshState{
			Enabled:      d.config.IsMeshEnabled(),
			NetworkCIDR:  d.config.Mesh.NetworkCIDR,
			Address:      meshAddress,
			Interface:    d.config.Mesh.Interface,
			ListenPort:   d.config.Mesh.ListenPort,
			SubnetBits:   d.config.Mesh.SubnetBits,
			NATTraversal: d.config.Mesh.NATTraversal,
			PublicKey:    publicKey,
			Endpoint:     endpoint,
		},
		Labels:    server.Labels,
		UpdatedAt: time.Now().UTC(),
	}

	meshState := takodMeshDesiredState{
		Project:     d.config.Project.Name,
		Environment: d.environment,
		NetworkCIDR: d.config.Mesh.NetworkCIDR,
		Interface:   d.config.Mesh.Interface,
		ListenPort:  d.config.Mesh.ListenPort,
		Peers:       peers,
		UpdatedAt:   time.Now().UTC(),
	}
	if _, err := takodclient.RequestJSON(client, d.takodSocket(), "PUT", "/v1/metadata", takod.MetadataRequest{
		Node:  nodeState,
		Peers: meshState,
	}); err != nil {
		return fmt.Errorf("failed to write takod metadata: %w", err)
	}

	meshNode := mesh.Node{
		Name:      serverName,
		Host:      server.Host,
		Address:   meshAddress,
		PublicKey: publicKey,
		Labels:    server.Labels,
	}
	if _, err := takodclient.RequestJSON(client, d.takodSocket(), "POST", "/v1/mesh/apply", takod.MeshApplyRequest{
		Config: d.wireGuardConfig(),
		Node:   meshNode,
		Peers:  takodPeersToWireGuardNodes(peers),
	}); err != nil {
		return fmt.Errorf("failed to reconcile WireGuard mesh: %w", err)
	}

	return nil
}

type prepareTakodNodeFunc func(index int, serverName string, server config.ServerConfig) error

type prepareTakodNodeResult struct {
	serverName string
	err        error
}

func (d *Deployer) prepareTakodNodes(servers []string, peers []takodMeshPeer, publicKeys map[string]string) error {
	return d.prepareTakodNodesWith(servers, func(index int, serverName string, server config.ServerConfig) error {
		client, err := d.sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return fmt.Errorf("failed to connect to %s: %w", serverName, err)
		}

		if d.verbose {
			fmt.Printf("  -> %s (%s)\n", serverName, server.Host)
		}

		if err := d.prepareTakodNode(client, serverName, server, index, peers, publicKeys[serverName]); err != nil {
			return fmt.Errorf("failed to prepare takod node %s: %w", serverName, err)
		}
		return nil
	})
}

func (d *Deployer) prepareTakodNodesWith(servers []string, prepare prepareTakodNodeFunc) error {
	resultCh := make(chan prepareTakodNodeResult, len(servers))
	var wg sync.WaitGroup

	for index, serverName := range servers {
		server, ok := d.config.Servers[serverName]
		if !ok {
			return fmt.Errorf("server %s not found", serverName)
		}

		wg.Add(1)
		go func(index int, serverName string, server config.ServerConfig) {
			defer wg.Done()
			resultCh <- prepareTakodNodeResult{
				serverName: serverName,
				err:        prepare(index, serverName, server),
			}
		}(index, serverName, server)
	}

	wg.Wait()
	close(resultCh)

	var errors []string
	for result := range resultCh {
		if result.err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", result.serverName, result.err))
		}
	}
	if len(errors) > 0 {
		sort.Strings(errors)
		return fmt.Errorf("failed to prepare takod runtime: %s", strings.Join(errors, "; "))
	}
	return nil
}

func (d *Deployer) deployServiceToTakodNode(client *ssh.Client, serverName string, serviceName string, service *config.ServiceConfig, imageRef string, slots []int) error {
	sort.Ints(slots)
	if d.verbose {
		fmt.Printf("  -> %s slots %v\n", serverName, slots)
	}

	networkName := takodNetworkName(d.config.Project.Name, d.environment)

	if service.IsPublic() && len(proxyPortsForService(*service)) == 0 {
		return fmt.Errorf("service %s has proxy config but no proxy ports", serviceName)
	}
	publishMeshUpstreams, err := d.shouldPublishMeshUpstreams()
	if err != nil {
		return err
	}

	envFileContent, err := d.buildTakodEnvFileContent(serviceName, service)
	if err != nil {
		return err
	}

	mounts, err := d.buildTakodMountSpecs(serviceName, service)
	if err != nil {
		return err
	}

	configFiles, err := d.buildTakodConfigFiles(serviceName, service)
	if err != nil {
		return err
	}

	request := takod.ReconcileServiceRequest{
		Project:        d.config.Project.Name,
		Environment:    d.environment,
		Service:        serviceName,
		Image:          imageRef,
		PullImage:      service.Image != "",
		RegistryAuth:   takodRegistryAuth(d.config, imageRef),
		Restart:        service.Restart,
		Network:        networkName,
		NetworkAlias:   serviceName,
		NetworkAliases: serviceNetworkAliases(d.config.Project.Name, d.environment, serviceName),
		EnvFileContent: envFileContent,
		Mounts:         mounts,
		ConfigFiles:    configFiles,
		Health:         d.buildTakodHealthSpec(service),
		Command:        service.Command,
		Labels:         serviceRuntimeLabels(d.config.Project.Name, d.environment, serviceName, *service),
		Strategy:       service.Deploy.Strategy,
		Order:          service.Deploy.Order,
		MaxUnavailable: service.Deploy.MaxUnavailable,
		Monitor:        service.Deploy.Monitor,
	}
	for _, slot := range slots {
		container, err := d.buildTakodContainerSpec(client, serverName, serviceName, service, slot, publishMeshUpstreams)
		if err != nil {
			return err
		}
		container.Slot = slot
		request.Containers = append(request.Containers, container)
	}

	return d.reconcileServiceViaTakod(client, request)
}

func serviceNetworkAliases(project string, environment string, serviceName string) []string {
	aliases := []string{
		serviceName,
		serviceName + ".tako.internal",
		serviceName + "." + environment + "." + project + ".tako.internal",
	}
	seen := make(map[string]bool, len(aliases))
	out := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		alias = strings.ToLower(strings.TrimSpace(alias))
		if alias == "" || seen[alias] {
			continue
		}
		seen[alias] = true
		out = append(out, alias)
	}
	return out
}

func (d *Deployer) runServiceHookTakod(serverName string, serviceName string, service *config.ServiceConfig, imageRef string, hookName string) error {
	hook := hookConfigForName(service, hookName)
	if hook == nil {
		return nil
	}
	if strings.TrimSpace(serverName) == "" {
		return fmt.Errorf("%s hook for %s has no target node", hookName, serviceName)
	}
	client, err := d.getEnvironmentClient(serverName)
	if err != nil {
		return err
	}
	request, err := d.buildHookRequest(serviceName, service, imageRef, hookName, hook)
	if err != nil {
		return err
	}
	if d.verbose {
		fmt.Printf("  -> %s hook on %s\n", hookName, serverName)
	}
	response, err := d.runHookViaTakod(client, request)
	if err != nil {
		return err
	}
	if response.ExitCode != 0 {
		return fmt.Errorf("%s hook failed for %s with exit code %d; inspect logs with: docker logs %s", hookName, serviceName, response.ExitCode, response.Container)
	}
	if d.verbose {
		fmt.Printf("  ✓ %s hook completed on %s\n", hookName, serverName)
	}
	return nil
}

func hookConfigForName(service *config.ServiceConfig, hookName string) *config.HookConfig {
	if service == nil {
		return nil
	}
	switch hookName {
	case takod.HookPreDeploy:
		return service.Hooks.PreDeploy
	case takod.HookPostDeploy:
		return service.Hooks.PostDeploy
	default:
		return nil
	}
}

func hookTargetServer(assignments []takodAssignment, targetServers []string) string {
	assignmentServers := uniqueAssignmentServers(assignments)
	if len(assignmentServers) > 0 {
		return assignmentServers[0]
	}
	if len(targetServers) > 0 {
		return targetServers[0]
	}
	return ""
}

func (d *Deployer) buildHookRequest(serviceName string, service *config.ServiceConfig, imageRef string, hookName string, hook *config.HookConfig) (takod.RunHookRequest, error) {
	if hook == nil {
		return takod.RunHookRequest{}, fmt.Errorf("%s hook is nil", hookName)
	}
	hookService := serviceConfigForHook(service, hook)
	envFileContent, err := d.buildTakodEnvFileContent(serviceName, &hookService)
	if err != nil {
		return takod.RunHookRequest{}, err
	}
	mounts, err := d.buildTakodMountSpecs(serviceName, service)
	if err != nil {
		return takod.RunHookRequest{}, err
	}
	return takod.RunHookRequest{
		Project:        d.config.Project.Name,
		Environment:    d.environment,
		Service:        serviceName,
		Hook:           hookName,
		Image:          imageRef,
		PullImage:      service.Image != "",
		RegistryAuth:   takodRegistryAuth(d.config, imageRef),
		Network:        takodNetworkName(d.config.Project.Name, d.environment),
		EnvFileContent: envFileContent,
		Mounts:         mounts,
		Command:        []string{"sh", "-c", hook.Command},
		User:           hook.User,
		WorkingDir:     hook.WorkingDir,
		Timeout:        hook.Timeout,
	}, nil
}

func takodRegistryAuth(cfg *config.Config, image string) *takod.RegistryAuth {
	auth := cfg.RegistryAuthForImage(image)
	if auth == nil {
		return nil
	}
	return &takod.RegistryAuth{
		Server:        auth.Server,
		Username:      auth.Username,
		Password:      auth.Password,
		IdentityToken: auth.IdentityToken,
	}
}

func serviceConfigForHook(service *config.ServiceConfig, hook *config.HookConfig) config.ServiceConfig {
	hookService := *service
	hookService.Env = mergeHookEnv(service.Env, hook.Env)
	hookService.Secrets = append(append([]string(nil), service.Secrets...), hook.Secrets...)
	return hookService
}

func mergeHookEnv(serviceEnv map[string]config.EnvValue, hookEnv map[string]string) map[string]config.EnvValue {
	if len(serviceEnv) == 0 && len(hookEnv) == 0 {
		return nil
	}
	merged := make(map[string]config.EnvValue, len(serviceEnv)+len(hookEnv))
	for key, value := range serviceEnv {
		merged[key] = value
	}
	for key, value := range hookEnv {
		merged[key] = config.PlainEnvValue(value)
	}
	return merged
}

func (d *Deployer) buildTakodContainerSpec(client takodclient.RequestExecutor, serverName string, serviceName string, service *config.ServiceConfig, slot int, publishMeshUpstreams bool) (takod.ContainerSpec, error) {
	container := takod.ContainerSpec{
		Name: d.takodContainerName(serviceName, slot),
		NetworkAliases: []string{
			runtimeid.ContainerNetworkAlias(d.config.Project.Name, d.environment, serviceName, slot),
		},
	}
	for _, port := range service.EffectivePorts() {
		switch port.Mode {
		case "proxy":
			if !publishMeshUpstreams && !serviceExportsTarget(service, port.Target) {
				continue
			}
			meshHostIP, err := d.meshHostIPForServer(serverName)
			if err != nil {
				return container, err
			}
			meshPort, err := d.allocateMeshUpstreamPort(client, serverName, serviceName, slot, port.Target)
			if err != nil {
				return container, err
			}
			container.Publishes = append(container.Publishes, dockerPublishSpec(meshHostIP, meshPort, port.Target, "tcp"))
		case "internal":
			if !serviceExportsTarget(service, port.Target) {
				continue
			}
			meshHostIP, err := d.meshHostIPForServer(serverName)
			if err != nil {
				return container, err
			}
			meshPort, err := d.allocateMeshUpstreamPort(client, serverName, serviceName, slot, port.Target)
			if err != nil {
				return container, err
			}
			container.Publishes = append(container.Publishes, dockerPublishSpec(meshHostIP, meshPort, port.Target, "tcp"))
		case "host":
			hostIP, err := d.resolveHostBindIP(serverName, port.HostIP)
			if err != nil {
				return container, fmt.Errorf("service %s port %s: %w", serviceName, port.Name, err)
			}
			published, err := d.allocateHostBindPort(client, serverName, serviceName, slot, port, hostIP)
			if err != nil {
				return container, err
			}
			container.Publishes = append(container.Publishes, dockerPublishSpec(hostIP, published, port.Target, dockerPublishProtocol(port.Protocol)))
		}
	}
	return container, nil
}

func serviceExportsTarget(service *config.ServiceConfig, target int) bool {
	if service == nil || service.Export == nil || target <= 0 {
		return false
	}
	for _, exportedTarget := range service.Export.Ports {
		if exportedTarget == target {
			return true
		}
	}
	return false
}

func dockerPublishSpec(hostIP string, hostPort int, targetPort int, protocol string) string {
	target := strconv.Itoa(targetPort)
	if protocol == "udp" {
		target += "/udp"
	}
	if hostIP == "" {
		return fmt.Sprintf("%d:%s", hostPort, target)
	}
	return fmt.Sprintf("%s:%d:%s", hostIP, hostPort, target)
}

func dockerPublishProtocol(protocol string) string {
	if protocol == "udp" {
		return "udp"
	}
	return "tcp"
}

func (d *Deployer) allocateHostBindPort(client takodclient.RequestExecutor, serverName string, serviceName string, slot int, port config.PortConfig, hostIP string) (int, error) {
	published := port.Published
	if published <= 0 {
		published = port.Target
	}
	allocationHostIP := hostIP
	if allocationHostIP == "" {
		allocationHostIP = "0.0.0.0"
	}
	output, err := takodclient.RequestJSON(client, d.takodSocket(), "POST", "/v1/ports/allocate", takod.PortAllocationRequest{
		Kind:          takod.PortAllocationKindHostBind,
		Project:       d.config.Project.Name,
		Environment:   d.environment,
		Service:       serviceName,
		Slot:          slot,
		HostIP:        allocationHostIP,
		ContainerPort: port.Target,
		PreferredPort: published,
		MinPort:       published,
		MaxPort:       published,
	})
	if err != nil {
		return 0, fmt.Errorf("failed to reserve host port %d for %s port %s on %s: %w", published, serviceName, port.Name, serverName, err)
	}
	var response takod.PortAllocationResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return 0, fmt.Errorf("failed to parse host port allocation for %s port %s on %s: %w", serviceName, port.Name, serverName, err)
	}
	if response.HostPort != published {
		return 0, fmt.Errorf("takod allocated host port %d for %s port %s, want %d", response.HostPort, serviceName, port.Name, published)
	}
	return response.HostPort, nil
}

func (d *Deployer) resolveHostBindIP(serverName string, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if ip := net.ParseIP(value); ip != nil {
		return ip.String(), nil
	}
	_, cidr, err := net.ParseCIDR(value)
	if err != nil {
		return "", fmt.Errorf("invalid hostIP %q", value)
	}
	if server, ok := d.config.Servers[serverName]; ok {
		if ip := net.ParseIP(server.Host); ip != nil && cidr.Contains(ip) {
			return ip.String(), nil
		}
	}
	if meshIP, err := d.meshHostIPForServer(serverName); err == nil {
		if ip := net.ParseIP(meshIP); ip != nil && cidr.Contains(ip) {
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("no node IP matches hostIP CIDR %s", value)
}

func (d *Deployer) shouldPublishMeshUpstreams() (bool, error) {
	servers, err := d.config.GetEnvironmentServers(d.environment)
	if err != nil {
		return false, err
	}
	return len(servers) > 1, nil
}

func serviceRuntimeLabels(project string, environment string, serviceName string, service config.ServiceConfig) map[string]string {
	labels := map[string]string{
		runtimeid.ServiceIdentityLabel: runtimeid.ServiceIdentity(project, environment, serviceName),
	}
	configHash, ok := reconcile.SafeServiceConfigHash(service)
	if !ok {
		return labels
	}
	labels[reconcile.ConfigHashLabel] = configHash
	return labels
}

func (d *Deployer) buildTakodHealthSpec(service *config.ServiceConfig) *takod.HealthSpec {
	healthPort := service.PrimaryTargetPort()
	if service.HealthCheck.Path == "" || healthPort <= 0 {
		return nil
	}

	interval := service.HealthCheck.Interval
	if interval == "" {
		interval = "10s"
	}
	timeout := service.HealthCheck.Timeout
	if timeout == "" {
		timeout = "5s"
	}
	retries := service.HealthCheck.Retries
	if retries <= 0 {
		retries = 3
	}
	startPeriod := service.HealthCheck.StartPeriod
	if startPeriod == "" {
		startPeriod = "10s"
	}

	return &takod.HealthSpec{
		Command:      buildTakodHealthCommand(healthPort, service.HealthCheck.Path),
		Interval:     interval,
		Timeout:      timeout,
		Retries:      retries,
		StartPeriod:  startPeriod,
		WaitAttempts: deploymentHealthWaitAttempts(interval, startPeriod, retries),
	}
}

func deploymentHealthWaitAttempts(interval string, startPeriod string, retries int) int {
	if retries <= 0 {
		retries = 3
	}

	intervalDuration, err := time.ParseDuration(interval)
	if err != nil || intervalDuration <= 0 {
		intervalDuration = 10 * time.Second
	}
	startPeriodDuration, err := time.ParseDuration(startPeriod)
	if err != nil || startPeriodDuration < 0 {
		startPeriodDuration = 10 * time.Second
	}

	wait := startPeriodDuration + (time.Duration(retries+1) * intervalDuration) + (5 * time.Second)
	attempts := int(wait.Round(time.Second) / time.Second)
	if attempts < 30 {
		return 30
	}
	if attempts > 600 {
		return 600
	}
	return attempts
}

func buildTakodHealthCommand(port int, path string) string {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	nodeProbe := `fetch(process.argv[1]).then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))`
	script := fmt.Sprintf("url=%s; if command -v curl >/dev/null 2>&1; then curl -sf -- \"$url\" >/dev/null; elif command -v wget >/dev/null 2>&1; then wget -q -O /dev/null -- \"$url\"; elif command -v node >/dev/null 2>&1; then node -e %s \"$url\"; else echo 'curl, wget, or node is required for health checks' >&2; exit 127; fi", utils.ShellQuote(url), utils.ShellQuote(nodeProbe))
	return "sh -c " + utils.ShellQuote(script)
}

func (d *Deployer) buildTakodMountSpecs(serviceName string, service *config.ServiceConfig) ([]string, error) {
	var mounts []string
	for _, volume := range service.Volumes {
		if config.IsNFSVolume(volume) {
			return nil, fmt.Errorf("service %s: NFS volumes are no longer supported; use node-local volumes or an external storage service", serviceName)
		}
		if err := config.ValidateVolumeMountSpec(volume); err != nil {
			return nil, fmt.Errorf("service %s: %w", serviceName, err)
		}

		spec, err := config.ParseVolumeMountSpec(volume)
		if err != nil {
			return nil, fmt.Errorf("service %s: %w", serviceName, err)
		}
		readOnly := dockerMountReadOnlyOption(spec.Mode)
		if !spec.HasTarget {
			target := spec.Source
			source := d.dockerVolumeName(target)
			mounts = append(mounts, fmt.Sprintf("type=volume,source=%s,target=%s%s", source, target, readOnly))
			continue
		}

		if strings.HasPrefix(spec.Source, "/") {
			mounts = append(mounts, fmt.Sprintf("type=bind,source=%s,target=%s%s", spec.Source, spec.Target, readOnly))
		} else {
			namedVolume := d.dockerVolumeName(spec.Source)
			mounts = append(mounts, fmt.Sprintf("type=volume,source=%s,target=%s%s", namedVolume, spec.Target, readOnly))
		}
	}
	return mounts, nil
}

func (d *Deployer) buildTakodConfigFiles(serviceName string, service *config.ServiceConfig) ([]takod.ConfigFileMount, error) {
	if len(service.Configs) == 0 {
		return nil, nil
	}
	if len(d.config.Configs) == 0 {
		return nil, fmt.Errorf("service %s: service config files require top-level configs", serviceName)
	}
	files := make([]takod.ConfigFileMount, 0, len(service.Configs))
	for _, mount := range service.Configs {
		configFile, ok := d.config.Configs[mount.Source]
		if !ok {
			return nil, fmt.Errorf("service %s: config source %q is not defined", serviceName, mount.Source)
		}
		data, err := d.configFileContent(mount.Source, configFile)
		if err != nil {
			return nil, fmt.Errorf("service %s: failed to read config %s: %w", serviceName, mount.Source, err)
		}
		if len(data) > 1<<20 {
			return nil, fmt.Errorf("service %s: config %s exceeds 1 MiB", serviceName, mount.Source)
		}
		sum := sha256.Sum256(data)
		hash := hex.EncodeToString(sum[:])
		files = append(files, takod.ConfigFileMount{
			Source:        mount.Source,
			Target:        mount.Target,
			Mode:          mount.Mode,
			ContentBase64: base64.StdEncoding.EncodeToString(data),
			ContentHash:   hash,
		})
	}
	return files, nil
}

func (d *Deployer) configFileContent(name string, configFile config.ConfigFileConfig) ([]byte, error) {
	if configFile.Source != "" {
		return os.ReadFile(configFile.Source)
	}
	if configFile.Generate != nil {
		if d.generatedConfigs == nil {
			return nil, fmt.Errorf("generated config was not prepared")
		}
		data, ok := d.generatedConfigs[name]
		if !ok {
			return nil, fmt.Errorf("generated config was not prepared")
		}
		return data, nil
	}
	return nil, fmt.Errorf("source or generate is required")
}

func dockerMountReadOnlyOption(mode string) string {
	if mode == "ro" {
		return ",readonly"
	}
	return ""
}

func (d *Deployer) ensureTakodProxy(client *ssh.Client, networkName string, email string) error {
	if email == "" {
		email = "tako@redentor.dev"
	}
	_, err := takodclient.RequestJSON(client, d.takodSocket(), "POST", "/v1/proxy", takod.ReconcileProxyRequest{
		Network: networkName,
		Email:   email,
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile takod proxy: %w", err)
	}
	return nil
}

// BuildServiceEnvFileContent renders the runtime env file for a service after
// resolving Tako service links. It is shared by deploy and one-off run paths.
func (d *Deployer) BuildServiceEnvFileContent(serviceName string, service *config.ServiceConfig) (string, error) {
	return d.buildTakodEnvFileContent(serviceName, service)
}

func (d *Deployer) buildTakodEnvFileContent(serviceName string, service *config.ServiceConfig) (string, error) {
	hasEnvVars := len(service.Env) > 0 || len(service.Secrets) > 0 || service.EnvFile != ""
	if !hasEnvVars {
		return "", nil
	}
	resolvedEnv, err := d.resolveServiceEnv(serviceName, service)
	if err != nil {
		return "", err
	}
	envService := *service
	envService.Env = resolvedEnv

	secretsMgr, err := secrets.NewManager(d.environment)
	if err != nil {
		return "", fmt.Errorf("failed to create secrets manager: %w", err)
	}
	envFile, err := secretsMgr.CreateEnvFile(&envService)
	if err != nil {
		return "", fmt.Errorf("failed to create env file: %w", err)
	}

	data, err := io.ReadAll(envFile.ToReader())
	if err != nil {
		return "", fmt.Errorf("failed to read env file: %w", err)
	}

	if d.verbose {
		fmt.Printf("  ✓ Env file created with %d variables\n", envFile.Count())
	}

	return string(data), nil
}

func (d *Deployer) planTakodAssignments(serviceName string, service *config.ServiceConfig) ([]takodAssignment, error) {
	servers, err := d.getTakodTargetServers()
	if err != nil {
		return nil, fmt.Errorf("failed to get takod target servers: %w", err)
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("environment %s has no target servers", d.environment)
	}

	replicas := service.Replicas
	if replicas < 0 {
		return nil, fmt.Errorf("replicas cannot be negative")
	}
	scaleToZero := replicas == 0
	if replicas <= 0 {
		replicas = 1
	}

	targets, err := config.ResolvePlacementTargets(service.Placement, d.config.Servers, servers, d.environment)
	if err != nil {
		return nil, err
	}
	global := service.Placement != nil && strings.TrimSpace(service.Placement.Strategy) == "global"
	if global {
		replicas = len(targets)
		scaleToZero = false
	}
	if scaleToZero {
		return []takodAssignment{}, nil
	}
	targets, err = d.applyVolumePlacementGuardrails(serviceName, service, targets, replicas, global, explicitPlacement(service.Placement))
	if err != nil {
		return nil, err
	}

	assignments := make([]takodAssignment, 0, replicas)
	for slot := 1; slot <= replicas; slot++ {
		serverName := targets[(slot-1)%len(targets)]
		assignments = append(assignments, takodAssignment{ServerName: serverName, Slot: slot})
	}
	if err := d.validateVolumeAssignments(serviceName, service, assignments, global); err != nil {
		return nil, err
	}
	return assignments, nil
}

func explicitPlacement(placement *config.PlacementConfig) bool {
	if placement == nil {
		return false
	}
	return strings.TrimSpace(placement.Strategy) != "" ||
		len(placement.Servers) > 0 ||
		len(placement.Constraints) > 0 ||
		len(placement.Preferences) > 0
}

func (d *Deployer) ensureTakodMeshKeys(servers []string) (map[string]string, error) {
	return d.ensureTakodMeshKeysWith(servers, func(serverName string) (string, error) {
		client, err := d.getEnvironmentClient(serverName)
		if err != nil {
			return "", err
		}
		output, err := takodclient.RequestJSON(client, d.takodSocket(), "POST", "/v1/mesh/key", nil)
		if err != nil {
			return "", fmt.Errorf("failed to ensure WireGuard key on %s: %w", serverName, err)
		}
		var response takod.MeshKeyResponse
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			return "", fmt.Errorf("failed to parse WireGuard key response from %s: %w", serverName, err)
		}
		publicKey := strings.TrimSpace(response.PublicKey)
		if publicKey == "" {
			return "", fmt.Errorf("WireGuard public key from %s is empty", serverName)
		}
		return publicKey, nil
	})
}

type ensureTakodMeshKeyFunc func(serverName string) (string, error)

type ensureTakodMeshKeyResult struct {
	serverName string
	publicKey  string
	err        error
}

func (d *Deployer) ensureTakodMeshKeysWith(servers []string, ensure ensureTakodMeshKeyFunc) (map[string]string, error) {
	publicKeys := make(map[string]string, len(servers))
	resultCh := make(chan ensureTakodMeshKeyResult, len(servers))
	var wg sync.WaitGroup

	for _, serverName := range servers {
		if _, ok := d.config.Servers[serverName]; !ok {
			return nil, fmt.Errorf("server %s not found", serverName)
		}

		wg.Add(1)
		go func(serverName string) {
			defer wg.Done()
			publicKey, err := ensure(serverName)
			resultCh <- ensureTakodMeshKeyResult{
				serverName: serverName,
				publicKey:  strings.TrimSpace(publicKey),
				err:        err,
			}
		}(serverName)
	}

	wg.Wait()
	close(resultCh)

	var errors []string
	for result := range resultCh {
		if result.err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", result.serverName, result.err))
			continue
		}
		if result.publicKey == "" {
			errors = append(errors, fmt.Sprintf("%s: WireGuard public key is empty", result.serverName))
			continue
		}
		publicKeys[result.serverName] = result.publicKey
	}
	if len(errors) > 0 {
		sort.Strings(errors)
		return nil, fmt.Errorf("failed to ensure WireGuard keys: %s", strings.Join(errors, "; "))
	}
	return publicKeys, nil
}

func (d *Deployer) buildTakodMeshPeers(servers []string, publicKeys map[string]string) ([]takodMeshPeer, error) {
	peers := make([]takodMeshPeer, 0, len(servers))
	for i, serverName := range servers {
		server := d.config.Servers[serverName]
		address, err := d.meshAddress(i)
		if err != nil {
			return nil, err
		}
		publicKey := publicKeys[serverName]
		if publicKey == "" {
			return nil, fmt.Errorf("missing WireGuard public key for %s", serverName)
		}
		peers = append(peers, takodMeshPeer{
			Name:      serverName,
			Host:      server.Host,
			Address:   address,
			PublicKey: publicKey,
			Endpoint:  net.JoinHostPort(server.Host, strconv.Itoa(d.config.Mesh.ListenPort)),
			Labels:    server.Labels,
		})
	}
	return peers, nil
}

func (d *Deployer) wireGuardConfig() mesh.WireGuardConfig {
	return mesh.WireGuardConfig{
		Enabled:      d.config.IsMeshEnabled(),
		Interface:    d.config.Mesh.Interface,
		ListenPort:   d.config.Mesh.ListenPort,
		NATTraversal: d.config.Mesh.NATTraversal,
	}
}

func takodPeersToWireGuardNodes(peers []takodMeshPeer) []mesh.Node {
	nodes := make([]mesh.Node, 0, len(peers))
	for _, peer := range peers {
		nodes = append(nodes, mesh.Node{
			Name:      peer.Name,
			Host:      peer.Host,
			Address:   peer.Address,
			PublicKey: peer.PublicKey,
			Labels:    peer.Labels,
		})
	}
	return nodes
}

func (d *Deployer) meshAddress(index int) (string, error) {
	_, ipNet, err := net.ParseCIDR(d.config.Mesh.NetworkCIDR)
	if err != nil {
		return "", fmt.Errorf("invalid mesh CIDR: %w", err)
	}
	ip := ipNet.IP.To4()
	if ip == nil {
		return "", fmt.Errorf("mesh.networkCIDR must be IPv4")
	}

	base := binary.BigEndian.Uint32(ip)
	nodeIP := make(net.IP, 4)
	binary.BigEndian.PutUint32(nodeIP, base+uint32(index+1))
	return fmt.Sprintf("%s/%d", nodeIP.String(), d.config.Mesh.SubnetBits), nil
}

func (d *Deployer) getSourceEnvironmentClient() (*ssh.Client, error) {
	servers, err := d.getTakodTargetServers()
	if err != nil {
		return nil, err
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("environment %s has no target servers", d.environment)
	}
	return d.getEnvironmentClient(servers[0])
}

func (d *Deployer) getTakodTargetServers() ([]string, error) {
	if len(d.targetServers) > 0 {
		return append([]string(nil), d.targetServers...), nil
	}
	return d.config.GetEnvironmentServers(d.environment)
}

func (d *Deployer) getTakodMeshServers() ([]string, error) {
	targetServers, err := d.getTakodTargetServers()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(targetServers)+len(d.config.Imports))
	meshServers := make([]string, 0, len(targetServers)+len(d.config.Imports))
	appendServer := func(serverName string) error {
		serverName = strings.TrimSpace(serverName)
		if serverName == "" || seen[serverName] {
			return nil
		}
		if _, ok := d.config.Servers[serverName]; !ok {
			return fmt.Errorf("server %s not found", serverName)
		}
		seen[serverName] = true
		meshServers = append(meshServers, serverName)
		return nil
	}

	importAliases := make([]string, 0, len(d.config.Imports))
	for alias := range d.config.Imports {
		importAliases = append(importAliases, alias)
	}
	sort.Strings(importAliases)
	for _, alias := range importAliases {
		importServers, err := serviceimport.ServerNames(d.config, d.environment, d.config.Imports[alias], "")
		if err != nil {
			return nil, fmt.Errorf("import %s: %w", alias, err)
		}
		for _, serverName := range importServers {
			if err := appendServer(serverName); err != nil {
				return nil, err
			}
		}
	}
	for _, serverName := range targetServers {
		if err := appendServer(serverName); err != nil {
			return nil, err
		}
	}
	return meshServers, nil
}

func (d *Deployer) getEnvironmentClient(serverName string) (*ssh.Client, error) {
	server, ok := d.config.Servers[serverName]
	if !ok {
		return nil, fmt.Errorf("server %s not found", serverName)
	}
	client, err := d.sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %w", serverName, err)
	}
	return client, nil
}

func (d *Deployer) takodContainerName(serviceName string, slot int) string {
	return runtimeid.ContainerName(d.config.Project.Name, d.environment, serviceName, slot)
}

func (d *Deployer) takodDataDir() string {
	if d.config.Runtime != nil && d.config.Runtime.Agent != nil && d.config.Runtime.Agent.DataDir != "" {
		return d.config.Runtime.Agent.DataDir
	}
	return "/var/lib/tako"
}

func groupTakodAssignments(assignments []takodAssignment) map[string][]int {
	grouped := make(map[string][]int)
	for _, assignment := range assignments {
		grouped[assignment.ServerName] = append(grouped[assignment.ServerName], assignment.Slot)
	}
	return grouped
}

func sortedAssignmentServers(grouped map[string][]int) []string {
	servers := make([]string, 0, len(grouped))
	for serverName := range grouped {
		servers = append(servers, serverName)
	}
	sort.Strings(servers)
	return servers
}

func uniqueAssignmentServers(assignments []takodAssignment) []string {
	seen := make(map[string]bool)
	var servers []string
	for _, assignment := range assignments {
		if !seen[assignment.ServerName] {
			seen[assignment.ServerName] = true
			servers = append(servers, assignment.ServerName)
		}
	}
	sort.Strings(servers)
	return servers
}

func (d *Deployer) reconcileServiceViaTakod(client *ssh.Client, request takod.ReconcileServiceRequest) error {
	if _, err := takodclient.RequestJSON(client, d.takodSocket(), "POST", "/v1/reconcile-service", request); err != nil {
		return fmt.Errorf("takod service reconciliation failed: %w", err)
	}
	return nil
}

func (d *Deployer) runHookViaTakod(client *ssh.Client, request takod.RunHookRequest) (*takod.RunHookResponse, error) {
	output, err := takodclient.RequestJSON(client, d.takodSocket(), "POST", "/v1/hooks/run", request)
	if err != nil {
		return nil, fmt.Errorf("takod hook execution failed: %w", err)
	}
	var response takod.RunHookResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("failed to parse takod hook response: %w", err)
	}
	return &response, nil
}

func (d *Deployer) removeServiceViaTakod(client *ssh.Client, request takod.RemoveServiceRequest) error {
	if _, err := takodclient.RequestJSON(client, d.takodSocket(), "POST", "/v1/remove-service", request); err != nil {
		return fmt.Errorf("takod service removal failed: %w", err)
	}
	return nil
}

func (d *Deployer) takodSocket() string {
	if d.config.Runtime != nil && d.config.Runtime.Agent != nil && d.config.Runtime.Agent.Socket != "" {
		return d.config.Runtime.Agent.Socket
	}
	return "/run/tako/takod.sock"
}

func takodNetworkName(project string, environment string) string {
	return runtimeid.NetworkName(project, environment)
}
