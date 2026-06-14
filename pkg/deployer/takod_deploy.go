package deployer

import (
	"encoding/binary"
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

	servers, err := d.getTakodTargetServers()
	if err != nil {
		return fmt.Errorf("failed to get takod target servers: %w", err)
	}
	if len(servers) == 0 {
		return fmt.Errorf("environment %s has no target servers", d.environment)
	}

	if d.verbose {
		fmt.Printf("\n-> Preparing takod mesh runtime...\n")
	}

	if err := d.ensureTakodAgents(servers); err != nil {
		return err
	}

	nodePublicKeys, err := d.ensureTakodMeshKeys(servers)
	if err != nil {
		return err
	}

	peers, err := d.buildTakodMeshPeers(servers, nodePublicKeys)
	if err != nil {
		return err
	}

	if err := d.prepareTakodNodes(servers, peers, nodePublicKeys); err != nil {
		return err
	}

	if d.verbose {
		fmt.Printf("  ✓ takod runtime prepared on %d node(s)\n", len(servers))
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

	assignments, err := d.planTakodAssignments(service)
	if err != nil {
		return err
	}

	assignmentServers := uniqueAssignmentServers(assignments)
	if service.Build != "" && len(assignmentServers) > 1 {
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

	grouped := groupTakodAssignments(assignments)
	targetServers, err := d.getTakodTargetServers()
	if err != nil {
		return fmt.Errorf("failed to get takod target servers: %w", err)
	}
	if err := runTakodNodeActions(targetServers, func(serverName string) error {
		slots := grouped[serverName]
		client, err := d.getEnvironmentClient(serverName)
		if err != nil {
			return err
		}
		if err := d.deployServiceToTakodNode(client, serverName, serviceName, service, imageRef, slots); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	return nil
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

	if service.IsPublic() {
		if service.Port <= 0 {
			return fmt.Errorf("service %s has proxy config but no port", serviceName)
		}
	}
	publishMeshUpstreams, err := d.shouldPublishMeshUpstreams()
	if err != nil {
		return err
	}

	envFileContent, err := d.buildTakodEnvFileContent(service)
	if err != nil {
		return err
	}

	mounts, err := d.buildTakodMountSpecs(serviceName, service)
	if err != nil {
		return err
	}

	request := takod.ReconcileServiceRequest{
		Project:        d.config.Project.Name,
		Environment:    d.environment,
		Service:        serviceName,
		Image:          imageRef,
		PullImage:      service.Image != "",
		Restart:        service.Restart,
		Network:        networkName,
		NetworkAlias:   serviceName,
		EnvFileContent: envFileContent,
		Mounts:         mounts,
		Health:         d.buildTakodHealthSpec(service),
		Command:        service.Command,
		Labels:         serviceRuntimeLabels(d.config.Project.Name, d.environment, serviceName, *service),
	}
	for _, slot := range slots {
		meshPort := 0
		if service.IsPublic() && publishMeshUpstreams {
			meshPort, err = d.allocateMeshUpstreamPort(client, serverName, serviceName, slot, service.Port)
			if err != nil {
				return err
			}
		}
		container, err := d.buildTakodContainerSpec(serverName, serviceName, service, slot, publishMeshUpstreams, meshPort)
		if err != nil {
			return err
		}
		request.Containers = append(request.Containers, container)
	}

	return d.reconcileServiceViaTakod(client, request)
}

func (d *Deployer) buildTakodContainerSpec(serverName string, serviceName string, service *config.ServiceConfig, slot int, publishMeshUpstreams bool, meshPort int) (takod.ContainerSpec, error) {
	container := takod.ContainerSpec{Name: d.takodContainerName(serviceName, slot)}
	if service.IsPublic() && publishMeshUpstreams {
		meshHostIP, err := d.meshHostIPForServer(serverName)
		if err != nil {
			return container, err
		}
		if meshPort <= 0 {
			return container, fmt.Errorf("service %s slot %d requires an allocated mesh upstream port", serviceName, slot)
		}
		container.Publishes = append(container.Publishes, fmt.Sprintf("%s:%d:%d", meshHostIP, meshPort, service.Port))
	}
	return container, nil
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
	if service.HealthCheck.Path == "" || service.Port <= 0 {
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
	waitAttempts := service.HealthCheck.Retries
	if waitAttempts <= 0 {
		waitAttempts = 30
	}

	return &takod.HealthSpec{
		Command:      buildTakodHealthCommand(service.Port, service.HealthCheck.Path),
		Interval:     interval,
		Timeout:      timeout,
		Retries:      retries,
		StartPeriod:  startPeriod,
		WaitAttempts: waitAttempts,
	}
}

func buildTakodHealthCommand(port int, path string) string {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	return fmt.Sprintf("curl -sf -- %s || exit 1", utils.ShellQuote(url))
}

func (d *Deployer) buildTakodMountSpecs(serviceName string, service *config.ServiceConfig) ([]string, error) {
	var mounts []string
	for _, volume := range service.Volumes {
		if config.IsNFSVolume(volume) {
			return nil, fmt.Errorf("service %s: NFS volumes are no longer supported; use node-local volumes or an external storage service", serviceName)
		}

		source, target := parseVolumeSpec(volume)
		if target == "" {
			target = source
			source = runtimeid.VolumeName(d.config.Project.Name, d.environment, target)
			mounts = append(mounts, fmt.Sprintf("type=volume,source=%s,target=%s", source, target))
			continue
		}

		if strings.HasPrefix(source, "/") {
			mounts = append(mounts, fmt.Sprintf("type=bind,source=%s,target=%s", source, target))
		} else {
			namedVolume := runtimeid.VolumeName(d.config.Project.Name, d.environment, source)
			mounts = append(mounts, fmt.Sprintf("type=volume,source=%s,target=%s", namedVolume, target))
		}
	}
	return mounts, nil
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

func (d *Deployer) buildTakodEnvFileContent(service *config.ServiceConfig) (string, error) {
	hasEnvVars := len(service.Env) > 0 || len(service.Secrets) > 0 || service.EnvFile != ""
	if !hasEnvVars {
		return "", nil
	}

	secretsMgr, err := secrets.NewManager(d.environment)
	if err != nil {
		return "", fmt.Errorf("failed to create secrets manager: %w", err)
	}
	envFile, err := secretsMgr.CreateEnvFile(service)
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

func (d *Deployer) planTakodAssignments(service *config.ServiceConfig) ([]takodAssignment, error) {
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
	if service.Placement != nil && strings.TrimSpace(service.Placement.Strategy) == "global" {
		replicas = len(targets)
		scaleToZero = false
	}
	if scaleToZero {
		return []takodAssignment{}, nil
	}

	assignments := make([]takodAssignment, 0, replicas)
	for slot := 1; slot <= replicas; slot++ {
		serverName := targets[(slot-1)%len(targets)]
		assignments = append(assignments, takodAssignment{ServerName: serverName, Slot: slot})
	}
	return assignments, nil
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

func parseVolumeSpec(volume string) (source, target string) {
	parts := strings.SplitN(volume, ":", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}
