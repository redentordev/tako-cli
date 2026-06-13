package deployer

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/mesh"
	"github.com/redentordev/tako-cli/pkg/secrets"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/redentordev/tako-cli/pkg/unregistry"
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

	nodePublicKeys, err := d.ensureTakodMeshKeys(servers)
	if err != nil {
		return err
	}

	peers, err := d.buildTakodMeshPeers(servers, nodePublicKeys)
	if err != nil {
		return err
	}

	for i, serverName := range servers {
		server, ok := d.config.Servers[serverName]
		if !ok {
			return fmt.Errorf("server %s not found", serverName)
		}

		client, err := d.sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return fmt.Errorf("failed to connect to %s: %w", serverName, err)
		}

		if d.verbose {
			fmt.Printf("  -> %s (%s)\n", serverName, server.Host)
		}

		if err := d.prepareTakodNode(client, serverName, server, i, peers, nodePublicKeys[serverName]); err != nil {
			return fmt.Errorf("failed to prepare takod node %s: %w", serverName, err)
		}
	}

	if d.verbose {
		fmt.Printf("  ✓ takod runtime prepared on %d node(s)\n", len(servers))
	}

	return nil
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
			managerClient, err := d.getFirstEnvironmentClient()
			if err != nil {
				return err
			}
			unreg := unregistry.NewManager(d.config, d.sshPool, d.environment, d.verbose)
			if err := unreg.DistributeImageParallel(managerClient, imageRef); err != nil {
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
	for _, serverName := range targetServers {
		slots := grouped[serverName]
		client, err := d.getEnvironmentClient(serverName)
		if err != nil {
			return err
		}
		if err := d.deployServiceToTakodNode(client, serverName, serviceName, service, imageRef, slots); err != nil {
			return fmt.Errorf("%s: %w", serverName, err)
		}
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
	}
	for _, slot := range slots {
		containerName := d.takodContainerName(serviceName, slot)
		container := takod.ContainerSpec{Name: containerName}
		if service.IsPublic() {
			meshHostIP, err := d.meshHostIPForServer(serverName)
			if err != nil {
				return err
			}
			meshPort, err := d.meshUpstreamPort(serviceName, slot)
			if err != nil {
				return err
			}
			container.Publishes = append(container.Publishes, fmt.Sprintf("%s:%d:%d", meshHostIP, meshPort, service.Port))
		} else if service.Port > 0 {
			if len(slots) > 1 {
				return fmt.Errorf("service %s publishes port %d without proxy but has multiple replicas on the same node", serviceName, service.Port)
			}
			container.Publishes = append(container.Publishes, fmt.Sprintf("%d:%d", service.Port, service.Port))
		}
		request.Containers = append(request.Containers, container)
	}

	return d.reconcileServiceViaTakod(client, request)
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

	healthCmd := fmt.Sprintf("curl -sf http://localhost:%d%s || exit 1", service.Port, service.HealthCheck.Path)
	return &takod.HealthSpec{
		Command:      healthCmd,
		Interval:     interval,
		Timeout:      timeout,
		Retries:      retries,
		StartPeriod:  startPeriod,
		WaitAttempts: waitAttempts,
	}
}

func (d *Deployer) buildTakodMountSpecs(serviceName string, service *config.ServiceConfig) ([]string, error) {
	var mounts []string
	for _, volume := range service.Volumes {
		if config.IsNFSVolume(volume) {
			exportName, containerPath, readOnly, err := config.ParseNFSVolumeSpec(volume)
			if err != nil {
				return nil, fmt.Errorf("invalid NFS volume spec for service %s: %w", serviceName, err)
			}

			mountSource := fmt.Sprintf("/mnt/tako-nfs/%s_%s_%s", d.config.Project.Name, d.environment, exportName)
			if !d.config.IsMultiServer() {
				export, err := d.config.GetNFSExport(exportName)
				if err != nil {
					return nil, fmt.Errorf("NFS export '%s' not found in config for service %s: %w", exportName, serviceName, err)
				}
				mountSource = export.Path
			}

			mountOpts := fmt.Sprintf("type=bind,source=%s,target=%s", mountSource, containerPath)
			if readOnly {
				mountOpts += ",readonly"
			}
			mounts = append(mounts, mountOpts)
			continue
		}

		source, target := parseVolumeSpec(volume)
		if target == "" {
			target = source
			source = fmt.Sprintf("%s_%s_%s", d.config.Project.Name, d.environment, sanitizeVolumeName(target))
			mounts = append(mounts, fmt.Sprintf("type=volume,source=%s,target=%s", source, target))
			continue
		}

		if strings.HasPrefix(source, "/") {
			mounts = append(mounts, fmt.Sprintf("type=bind,source=%s,target=%s", source, target))
		} else {
			namedVolume := fmt.Sprintf("%s_%s_%s", d.config.Project.Name, d.environment, source)
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

	targets := append([]string(nil), servers...)
	replicas := service.Replicas
	if replicas < 0 {
		return nil, fmt.Errorf("replicas cannot be negative")
	}
	if replicas == 0 {
		return []takodAssignment{}, nil
	}
	if replicas <= 0 {
		replicas = 1
	}

	if service.Placement != nil {
		switch service.Placement.Strategy {
		case "global":
			replicas = len(targets)
		case "pinned":
			if len(service.Placement.Servers) == 0 {
				return nil, fmt.Errorf("pinned placement requires servers")
			}
			targets = append([]string(nil), service.Placement.Servers...)
			if replicas <= 0 {
				replicas = 1
			}
		case "spread", "any", "":
		default:
			return nil, fmt.Errorf("unknown placement strategy: %s", service.Placement.Strategy)
		}
	}

	if err := d.validateTakodTargets(targets, servers); err != nil {
		return nil, err
	}

	assignments := make([]takodAssignment, 0, replicas)
	for slot := 1; slot <= replicas; slot++ {
		serverName := targets[(slot-1)%len(targets)]
		assignments = append(assignments, takodAssignment{ServerName: serverName, Slot: slot})
	}
	return assignments, nil
}

func (d *Deployer) validateTakodTargets(targets []string, environmentServers []string) error {
	allowed := make(map[string]bool, len(environmentServers))
	for _, serverName := range environmentServers {
		allowed[serverName] = true
	}
	for _, target := range targets {
		if !allowed[target] {
			return fmt.Errorf("placement target %s is outside the selected takod node set for environment %s", target, d.environment)
		}
		if _, ok := d.config.Servers[target]; !ok {
			return fmt.Errorf("placement target %s is not defined in servers", target)
		}
	}
	return nil
}

func (d *Deployer) ensureTakodMeshKeys(servers []string) (map[string]string, error) {
	publicKeys := make(map[string]string, len(servers))
	for _, serverName := range servers {
		client, err := d.getEnvironmentClient(serverName)
		if err != nil {
			return nil, err
		}
		output, err := takodclient.RequestJSON(client, d.takodSocket(), "POST", "/v1/mesh/key", nil)
		if err != nil {
			return nil, fmt.Errorf("failed to ensure WireGuard key on %s: %w", serverName, err)
		}
		var response takod.MeshKeyResponse
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			return nil, fmt.Errorf("failed to parse WireGuard key response from %s: %w", serverName, err)
		}
		publicKey := strings.TrimSpace(response.PublicKey)
		if publicKey == "" {
			return nil, fmt.Errorf("WireGuard public key from %s is empty", serverName)
		}
		publicKeys[serverName] = publicKey
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

func (d *Deployer) getFirstEnvironmentClient() (*ssh.Client, error) {
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
	return fmt.Sprintf("%s_%s_%s_%d", d.config.Project.Name, d.environment, serviceName, slot)
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

func (d *Deployer) takodSocket() string {
	if d.config.Runtime != nil && d.config.Runtime.Agent != nil && d.config.Runtime.Agent.Socket != "" {
		return d.config.Runtime.Agent.Socket
	}
	return "/run/tako/takod.sock"
}

func takodNetworkName(project string, environment string) string {
	return fmt.Sprintf("tako_%s_%s", project, environment)
}

func parseVolumeSpec(volume string) (source, target string) {
	parts := strings.SplitN(volume, ":", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

func sanitizeRouterName(value string) string {
	var out strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			out.WriteRune(r)
		} else {
			out.WriteRune('-')
		}
	}
	return strings.Trim(out.String(), "-")
}

func sanitizeVolumeName(value string) string {
	value = strings.Trim(value, "/")
	value = strings.ReplaceAll(value, "/", "_")
	value = strings.ReplaceAll(value, ":", "_")
	if value == "" {
		return "data"
	}
	return value
}
