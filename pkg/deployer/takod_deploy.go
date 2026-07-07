package deployer

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployplan"
	"github.com/redentordev/tako-cli/pkg/mesh"
	"github.com/redentordev/tako-cli/pkg/provisioner"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	"github.com/redentordev/tako-cli/pkg/secrets"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	takounregistry "github.com/redentordev/tako-cli/pkg/unregistry"
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

type takodServiceDeployOptions struct {
	BuildImage bool
	PullImage  bool
	WarmOnly   bool
}

type localImageClient interface {
	CheckAvailable(context.Context) error
	Build(context.Context, takounregistry.BuildRequest) error
	Push(context.Context, takounregistry.PushRequest) error
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
		d.printf("\n-> Preparing takod mesh runtime...\n")
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
		d.printf("  ✓ takod runtime prepared on %d node(s)\n", len(servers))
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
	return d.deployServiceTakod(serviceName, service, imageRef, takodDeployOptionsForService(service, d.skipBuild))
}

func (d *Deployer) DeployServiceTakodWarmOnly(serviceName string, service *config.ServiceConfig, imageRef string) error {
	options := takodDeployOptionsForService(service, d.skipBuild)
	options.WarmOnly = true
	return d.deployServiceTakod(serviceName, service, imageRef, options)
}

func (d *Deployer) ActivateTakodServiceRevision(serviceName string, service *config.ServiceConfig, imageRef string) error {
	return d.deployServiceTakod(serviceName, service, imageRef, takodServiceDeployOptions{})
}

func takodDeployOptionsForService(service *config.ServiceConfig, skipBuild bool) takodServiceDeployOptions {
	if service == nil {
		return takodServiceDeployOptions{}
	}
	return takodServiceDeployOptions{
		BuildImage: service.Build != "" && !skipBuild,
		PullImage:  service.Image != "",
	}
}

func takodRollbackDeployOptionsForService(service *config.ServiceConfig) takodServiceDeployOptions {
	if service == nil {
		return takodServiceDeployOptions{}
	}
	if service.Build != "" {
		return takodServiceDeployOptions{
			BuildImage: true,
			PullImage:  false,
		}
	}
	return takodServiceDeployOptions{
		BuildImage: false,
		PullImage:  service.Image != "",
	}
}

func (d *Deployer) deployServiceTakod(serviceName string, service *config.ServiceConfig, imageRef string, options takodServiceDeployOptions) error {
	if d.sshPool == nil {
		return fmt.Errorf("ssh pool not initialized")
	}

	assignments, err := d.planTakodAssignments(service)
	if err != nil {
		return err
	}

	assignmentServers := uniqueAssignmentServers(assignments)
	if service.IsJob() {
		// Jobs run no containers: distribute the image to the owning node
		// and record it; the post-deploy ApplyJobSchedules pass registers
		// the cron schedule declaratively on every node.
		if options.BuildImage {
			if err := d.buildImageOnTakodNodes(serviceName, service, imageRef, assignmentServers); err != nil {
				return err
			}
		}
		d.recordJobImage(serviceName, imageRef)
		return nil
	}
	if options.BuildImage {
		if err := d.buildImageOnTakodNodes(serviceName, service, imageRef, assignmentServers); err != nil {
			return err
		}
	}

	// Release command: once per service per deploy, from the new image,
	// after it exists on the assigned nodes and before any rollout
	// activation (warm/replace/stop-old all happen in the reconcile below).
	if err := d.runReleaseCommand(serviceName, service, imageRef, assignmentServers); err != nil {
		return err
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
		if err := d.deployServiceToTakodNode(client, serverName, serviceName, service, imageRef, slots, options.PullImage, options.WarmOnly); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	return nil
}

func (d *Deployer) buildImageOnTakodNodes(serviceName string, service *config.ServiceConfig, imageRef string, serverNames []string) error {
	strategy := config.BuildStrategyRemote
	if d.config != nil {
		strategy = d.config.GetBuildStrategy()
	}
	switch strategy {
	case config.BuildStrategyLocal:
		return d.buildImageLocallyAndPushToTakodNodes(serviceName, service, imageRef, serverNames)
	case config.BuildStrategyAuto:
		err := d.buildImageLocallyAndPushToTakodNodes(serviceName, service, imageRef, serverNames)
		if err == nil {
			return nil
		}
		if d.verbose {
			d.printf("  Local build unavailable, falling back to remote takod build: %v\n", err)
		}
		return d.buildImageRemotelyOnTakodNodes(serviceName, service, imageRef, serverNames)
	default:
		return d.buildImageRemotelyOnTakodNodes(serviceName, service, imageRef, serverNames)
	}
}

func (d *Deployer) buildImageRemotelyOnTakodNodes(serviceName string, service *config.ServiceConfig, imageRef string, serverNames []string) error {
	if len(serverNames) == 0 {
		return fmt.Errorf("service %s has no assigned takod nodes for build", serviceName)
	}

	if d.verbose {
		d.printf("  Building image on %d assigned node(s) with remote takod builder...\n", len(serverNames))
	}

	return runTakodNodeActions(serverNames, func(serverName string) error {
		if d.verbose {
			d.printf("  [%s] Building %s\n", serverName, imageRef)
		}
		existsStart := time.Now()
		exists, existsErr := d.imageExistsOnTakodNode(serverName, imageRef)
		if existsErr == nil && exists {
			if d.verbose {
				d.printf("  [%s] Image already exists: %s (checked in %s)\n", serverName, imageRef, formatBuildDuration(time.Since(existsStart)))
			}
			return nil
		}
		if existsErr != nil && d.verbose {
			d.printf("  [%s] Could not check existing image, rebuilding: %v\n", serverName, existsErr)
		}
		buildStart := time.Now()
		if _, err := d.buildImageOnNode(serverName, serviceName, service, imageRef); err != nil {
			return fmt.Errorf("failed to build image on %s: %w", serverName, err)
		}
		if d.verbose {
			d.printf("  [%s] Image ready: %s (%s)\n", serverName, imageRef, formatBuildDuration(time.Since(buildStart)))
		}
		return nil
	})
}

func (d *Deployer) buildImageLocallyAndPushToTakodNodes(serviceName string, service *config.ServiceConfig, imageRef string, serverNames []string) error {
	if len(serverNames) == 0 {
		return fmt.Errorf("service %s has no assigned takod nodes for build", serviceName)
	}

	missingServers := d.takodNodesMissingImage(serverNames, imageRef)
	if len(missingServers) == 0 {
		if d.verbose {
			d.printf("  Image already exists on all assigned node(s): %s\n", imageRef)
		}
		return nil
	}

	if d.verbose {
		d.printf("  Building image locally with docker buildx and pushing via unregistry to %d node(s)...\n", len(missingServers))
	}

	ctx := d.baseContext()
	localClient := d.localImageTransferClient()
	if err := localClient.CheckAvailable(ctx); err != nil {
		return err
	}

	prepared, err := d.prepareBuildContext(service)
	if err != nil {
		return err
	}

	platformGroups, err := d.groupTakodNodesByPlatform(missingServers)
	if err != nil {
		return err
	}
	platforms := make([]string, 0, len(platformGroups))
	for platform := range platformGroups {
		platforms = append(platforms, platform)
	}
	sort.Strings(platforms)

	for _, platform := range platforms {
		targets := platformGroups[platform]
		sort.Strings(targets)
		if d.verbose {
			d.printf("  Building %s for %s\n", imageRef, platform)
		}
		buildStart := time.Now()
		if err := localClient.Build(ctx, takounregistry.BuildRequest{
			Image:      imageRef,
			ContextDir: prepared.AbsContextPath,
			Dockerfile: prepared.Dockerfile,
			Platform:   platform,
		}); err != nil {
			return err
		}
		if d.verbose {
			d.printf("  Local build ready for %s (%s)\n", platform, formatBuildDuration(time.Since(buildStart)))
		}

		for _, serverName := range targets {
			if err := d.pushLocalImageToTakodNode(ctx, localClient, imageRef, platform, serverName); err != nil {
				return err
			}
			if d.verbose {
				d.printf("  [%s] Image ready: %s\n", serverName, imageRef)
			}
		}
	}

	return nil
}

func (d *Deployer) pushLocalImageToTakodNode(ctx context.Context, localClient localImageClient, imageRef string, platform string, serverName string) error {
	server, ok := d.config.Servers[serverName]
	if !ok {
		return fmt.Errorf("server %s not found", serverName)
	}
	target, sshKey, err := unregistryPushTarget(serverName, server)
	if err != nil {
		return err
	}
	if d.verbose {
		d.printf("  [%s] Pushing %s with docker pussh\n", serverName, imageRef)
	}
	pushStart := time.Now()
	if err := localClient.Push(ctx, takounregistry.PushRequest{
		Image:    imageRef,
		Target:   target,
		SSHKey:   sshKey,
		Platform: platform,
	}); err != nil {
		return fmt.Errorf("failed to push image to %s: %w", serverName, err)
	}
	if d.verbose {
		d.printf("  [%s] Push complete: %s\n", serverName, formatBuildDuration(time.Since(pushStart)))
	}
	return nil
}

func (d *Deployer) localImageTransferClient() localImageClient {
	if d.localImageClient != nil {
		return d.localImageClient
	}
	client := takounregistry.Client{}
	if d.verbose {
		client.Stdout = os.Stdout
		client.Stderr = os.Stderr
	}
	return client
}

func (d *Deployer) takodNodesMissingImage(serverNames []string, imageRef string) []string {
	missing := make([]string, 0, len(serverNames))
	for _, serverName := range serverNames {
		existsStart := time.Now()
		exists, existsErr := d.imageExistsOnTakodNode(serverName, imageRef)
		if existsErr == nil && exists {
			if d.verbose {
				d.printf("  [%s] Image already exists: %s (checked in %s)\n", serverName, imageRef, formatBuildDuration(time.Since(existsStart)))
			}
			continue
		}
		if existsErr != nil && d.verbose {
			d.printf("  [%s] Could not check existing image, will push local image: %v\n", serverName, existsErr)
		}
		missing = append(missing, serverName)
	}
	return missing
}

func (d *Deployer) groupTakodNodesByPlatform(serverNames []string) (map[string][]string, error) {
	groups := make(map[string][]string)
	for _, serverName := range serverNames {
		platform, err := d.detectTakodNodePlatform(serverName)
		if err != nil {
			return nil, err
		}
		groups[platform] = append(groups[platform], serverName)
	}
	return groups, nil
}

func (d *Deployer) detectTakodNodePlatform(serverName string) (string, error) {
	client, err := d.getEnvironmentClient(serverName)
	if err != nil {
		return "", err
	}
	output, err := client.Execute("uname -m")
	if err != nil {
		return "", fmt.Errorf("failed to detect architecture on %s: %w", serverName, err)
	}
	platform, err := normalizeLinuxBuildPlatform(output)
	if err != nil {
		return "", fmt.Errorf("failed to detect architecture on %s: %w", serverName, err)
	}
	return platform, nil
}

func normalizeLinuxBuildPlatform(machine string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(machine)) {
	case "x86_64", "amd64":
		return "linux/amd64", nil
	case "aarch64", "arm64":
		return "linux/arm64", nil
	default:
		return "", fmt.Errorf("unsupported Linux architecture %q", strings.TrimSpace(machine))
	}
}

func unregistryPushTarget(serverName string, server config.ServerConfig) (string, string, error) {
	host := strings.TrimSpace(server.Host)
	user := strings.TrimSpace(server.User)
	if host == "" {
		return "", "", fmt.Errorf("server %s host is required for local unregistry push", serverName)
	}
	if user == "" {
		return "", "", fmt.Errorf("server %s user is required for local unregistry push", serverName)
	}
	if strings.TrimSpace(server.Password) != "" && strings.TrimSpace(server.SSHKey) == "" {
		return "", "", fmt.Errorf("server %s uses password-only SSH auth; deployment.build.strategy=local requires SSH key or agent auth because docker pussh uses the system ssh client", serverName)
	}
	if strings.Contains(host, ":") && net.ParseIP(host) != nil {
		return "", "", fmt.Errorf("server %s uses IPv6 host %s; docker pussh currently expects [user@]host[:port] without IPv6 literals", serverName, host)
	}

	target := user + "@" + host
	if server.Port != 0 && server.Port != 22 {
		target += ":" + strconv.Itoa(server.Port)
	}
	return target, strings.TrimSpace(server.SSHKey), nil
}

func (d *Deployer) imageExistsOnTakodNode(serverName string, imageRef string) (bool, error) {
	client, err := d.getEnvironmentClient(serverName)
	if err != nil {
		return false, err
	}
	output, err := takodclient.RequestJSON(client, d.takodSocket(), "GET", takodclient.ImageExistsEndpoint(imageRef), nil)
	if err != nil {
		return false, err
	}
	var response takod.ImageExistsResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return false, fmt.Errorf("failed to parse image exists response: %w", err)
	}
	return response.Exists, nil
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
		if err := d.deleteBackupScheduleViaTakod(client, serviceName); err != nil {
			return err
		}
		return d.removeServiceViaTakod(client, takod.RemoveServiceRequest{
			Project:     d.config.Project.Name,
			Environment: d.environment,
			Service:     serviceName,
		})
	})
}

func (d *Deployer) PruneTakodServiceRevisions(services map[string]config.ServiceConfig, keepRevisions map[string]string) error {
	normalizedRevisions, err := normalizeTakodProxyActiveRevisions(services, keepRevisions)
	if err != nil {
		return err
	}
	if len(normalizedRevisions) == 0 {
		return nil
	}
	if d.sshPool == nil {
		return fmt.Errorf("ssh pool not initialized")
	}

	requests := takodRevisionPruneRequests(d.config.Project.Name, d.environment, normalizedRevisions)
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
		for _, request := range requests {
			if err := d.removeServiceViaTakod(client, request); err != nil {
				return fmt.Errorf("failed to prune stale revisions for %s: %w", request.Service, err)
			}
		}
		return nil
	})
}

func takodRevisionPruneRequests(project string, environment string, keepRevisions map[string]string) []takod.RemoveServiceRequest {
	serviceNames := make([]string, 0, len(keepRevisions))
	for serviceName := range keepRevisions {
		serviceNames = append(serviceNames, serviceName)
	}
	sort.Strings(serviceNames)

	requests := make([]takod.RemoveServiceRequest, 0, len(serviceNames))
	for _, serviceName := range serviceNames {
		requests = append(requests, takod.RemoveServiceRequest{
			Project:      project,
			Environment:  environment,
			Service:      serviceName,
			KeepRevision: keepRevisions[serviceName],
		})
	}
	return requests
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
			d.printf("  -> %s (%s)\n", serverName, server.Host)
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

func (d *Deployer) deployServiceToTakodNode(client *ssh.Client, serverName string, serviceName string, service *config.ServiceConfig, imageRef string, slots []int, pullImage bool, warmOnly bool) error {
	sort.Ints(slots)
	if d.verbose {
		d.printf("  -> %s slots %v\n", serverName, slots)
	}

	networkName := takodNetworkName(d.config.Project.Name, d.environment)

	if service.IsProxied() {
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

	mounts, externalVolumes, err := d.buildTakodMountSpecs(serviceName, service)
	if err != nil {
		return err
	}

	request := takod.ReconcileServiceRequest{
		Project:            d.config.Project.Name,
		Environment:        d.environment,
		Service:            serviceName,
		Image:              imageRef,
		PullImage:          pullImage,
		Restart:            service.Restart,
		Network:            networkName,
		NetworkAlias:       serviceName,
		NetworkAttachments: d.buildTakodNetworkAttachments(serviceName, service),
		EnvFileContent:     envFileContent,
		Mounts:             mounts,
		Health:             d.buildTakodHealthSpec(service),
		Command:            service.Command,
		Labels:             serviceRuntimeLabels(d.config.Project.Name, d.environment, serviceName, *service),
		ExternalVolumes:    externalVolumes,
		MemoryLimit:        serviceMemoryLimit(service),
	}
	if pullImage {
		request.RegistryAuths = d.registryAuths()
	}
	serviceRevision := takodServiceRevisionID(d.config.Project.Name, d.environment, serviceName, imageRef, *service)
	serviceStrategy := effectiveDeployStrategy(service)
	request.Revision = serviceRevision
	request.DeployStrategy = serviceStrategy
	meshRevision := meshUpstreamRevisionForStrategy(serviceRevision, serviceStrategy)
	for _, slot := range slots {
		meshPort := 0
		if service.IsProxied() && publishMeshUpstreams {
			meshPort, err = d.allocateMeshUpstreamPort(client, serverName, serviceName, meshRevision, slot, service.Port)
			if err != nil {
				return err
			}
		}
		container, err := d.buildTakodContainerSpec(serverName, serviceName, service, slot, serviceRevision, publishMeshUpstreams, meshPort, warmOnly)
		if err != nil {
			return err
		}
		request.Containers = append(request.Containers, container)
	}

	if err := d.wrapRegistryAuthError(serverName, d.reconcileServiceViaTakod(client, request)); err != nil {
		return err
	}
	if warmOnly {
		return nil
	}
	return d.reconcileBackupScheduleViaTakod(client, serviceName, service, len(slots) > 0)
}

func (d *Deployer) buildTakodNetworkAttachments(serviceName string, service *config.ServiceConfig) []takod.NetworkAttachmentSpec {
	attachments := make([]takod.NetworkAttachmentSpec, 0, 1+len(service.Imports))
	if service.Export {
		exportAlias := runtimeid.ExportAlias(d.config.Project.Name, d.environment, serviceName)
		attachments = append(attachments, takod.NetworkAttachmentSpec{
			Network: runtimeid.ExportNetworkName(d.config.Project.Name, d.environment, serviceName),
			Aliases: []string{
				exportAlias,
			},
			Create: true,
			Labels: map[string]string{
				"tako.runtime":      "takod",
				"tako.discovery":    "export",
				"tako.project":      d.config.Project.Name,
				"tako.environment":  d.environment,
				"tako.service":      serviceName,
				"tako.export.alias": exportAlias,
			},
		})
	}
	for _, importSpec := range service.Imports {
		project, importedService, ok := strings.Cut(importSpec, ".")
		if !ok {
			continue
		}
		project = strings.TrimSpace(project)
		importedService = strings.TrimSpace(importedService)
		if project == "" || importedService == "" {
			continue
		}
		attachments = append(attachments, takod.NetworkAttachmentSpec{
			Network: runtimeid.ExportNetworkName(project, d.environment, importedService),
		})
	}
	return attachments
}

func (d *Deployer) buildTakodContainerSpec(serverName string, serviceName string, service *config.ServiceConfig, slot int, revision string, publishMeshUpstreams bool, meshPort int, warmOnly bool) (takod.ContainerSpec, error) {
	strategy := effectiveDeployStrategy(service)
	containerName, err := d.takodContainerNameForStrategy(serviceName, slot, revision, strategy)
	if err != nil {
		return takod.ContainerSpec{}, err
	}
	containerAlias, err := d.takodContainerAliasForStrategy(serviceName, slot, revision, strategy)
	if err != nil {
		return takod.ContainerSpec{}, err
	}
	container := takod.ContainerSpec{
		Name:           containerName,
		NetworkAliases: []string{containerAlias},
		Labels: map[string]string{
			reconcile.RevisionLabel:       revision,
			reconcile.DeployStrategyLabel: strategy,
			reconcile.SlotLabel:           strconv.Itoa(slot),
			reconcile.ActiveLabel:         strconv.FormatBool(!warmOnly),
		},
	}
	if service.IsProxied() && publishMeshUpstreams {
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

func takodServiceRevisionID(project string, environment string, serviceName string, imageRef string, service config.ServiceConfig) string {
	return deployplan.ServiceRevisionID(project, environment, serviceName, imageRef, service)
}

func ServiceRevisionID(project string, environment string, serviceName string, imageRef string, service config.ServiceConfig) string {
	return deployplan.ServiceRevisionID(project, environment, serviceName, imageRef, service)
}

func effectiveDeployStrategy(service *config.ServiceConfig) string {
	return deployplan.EffectiveDeployStrategy(service)
}

func deployStrategyUsesRevisionScopedContainers(strategy string) bool {
	return strategy == config.DeployStrategyRolling || strategy == config.DeployStrategyBlueGreen
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
		"tako.persistent":              strconv.FormatBool(service.Persistent),
	}
	configHash, ok := reconcile.SafeServiceConfigHash(service)
	if !ok {
		return labels
	}
	labels[reconcile.ConfigHashLabel] = configHash
	return labels
}

func (d *Deployer) buildTakodHealthSpec(service *config.ServiceConfig) *takod.HealthSpec {
	if readinessSpec := d.buildTakodReadinessHealthSpec(service); readinessSpec != nil {
		return attachTakodSmokeSpec(readinessSpec, service)
	}
	if service.HealthCheck.Path == "" && service.HealthCheck.TCPPort <= 0 {
		if smokeSpec := buildTakodSmokeOnlyHealthSpec(service); smokeSpec != nil {
			return smokeSpec
		}
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

	port := service.Port
	scheme := ""
	if service.HealthCheck.Path != "" {
		scheme = "http"
	}
	if service.HealthCheck.TCPPort > 0 {
		port = service.HealthCheck.TCPPort
	}

	return attachTakodSmokeSpec(&takod.HealthSpec{
		Path:         service.HealthCheck.Path,
		Port:         port,
		Scheme:       scheme,
		Interval:     interval,
		Timeout:      timeout,
		Retries:      retries,
		StartPeriod:  startPeriod,
		WaitAttempts: deploymentHealthWaitAttempts(interval, startPeriod, retries),
	}, service)
}

func (d *Deployer) buildTakodReadinessHealthSpec(service *config.ServiceConfig) *takod.HealthSpec {
	readiness := service.Deploy.Readiness
	if readiness.Path == "" && readiness.TCPPort <= 0 {
		return nil
	}

	interval := readiness.Interval
	if interval == "" {
		interval = "10s"
	}
	timeout := readiness.Timeout
	if timeout == "" {
		timeout = "5s"
	}
	retries := readiness.Retries
	if retries <= 0 {
		retries = 3
	}

	port := service.Port
	scheme := ""
	if readiness.Path != "" {
		scheme = "http"
	}
	if readiness.TCPPort > 0 {
		port = readiness.TCPPort
	}

	return attachTakodSmokeSpec(&takod.HealthSpec{
		Path:         readiness.Path,
		Port:         port,
		Scheme:       scheme,
		Interval:     interval,
		Timeout:      timeout,
		Retries:      retries,
		WaitAttempts: deploymentHealthWaitAttempts(interval, "0s", retries),
	}, service)
}

func buildTakodSmokeOnlyHealthSpec(service *config.ServiceConfig) *takod.HealthSpec {
	if service == nil || service.Deploy.SmokeTest.Path == "" {
		return nil
	}
	return attachTakodSmokeSpec(&takod.HealthSpec{
		Scheme:       "http",
		Interval:     "10s",
		Timeout:      "5s",
		Retries:      3,
		WaitAttempts: deploymentHealthWaitAttempts("10s", "0s", 3),
	}, service)
}

func attachTakodSmokeSpec(spec *takod.HealthSpec, service *config.ServiceConfig) *takod.HealthSpec {
	if spec == nil || service == nil || service.Deploy.SmokeTest.Path == "" {
		return spec
	}
	spec.SmokePath = service.Deploy.SmokeTest.Path
	spec.SmokePort = service.Port
	spec.SmokeExpectedStatus = service.Deploy.SmokeTest.ExpectedStatus
	if spec.SmokeExpectedStatus == 0 {
		spec.SmokeExpectedStatus = 200
	}
	if spec.Scheme == "" {
		spec.Scheme = "http"
	}
	return spec
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

func (d *Deployer) buildTakodMountSpecs(serviceName string, service *config.ServiceConfig) ([]string, []string, error) {
	var mounts []string
	var externalVolumes []string
	externalSeen := make(map[string]bool)
	for _, volume := range service.Volumes {
		if config.IsNFSVolume(volume) {
			return nil, nil, fmt.Errorf("service %s: NFS volumes are no longer supported; use node-local volumes or an external storage service", serviceName)
		}

		source, target := parseVolumeSpec(volume)
		if target == "" {
			target = source
			source = d.config.GetVolumeName(target, d.environment)
			mounts = append(mounts, fmt.Sprintf("type=volume,source=%s,target=%s", source, target))
			if d.config.IsVolumeExternal(target) && !externalSeen[source] {
				externalVolumes = append(externalVolumes, source)
				externalSeen[source] = true
			}
			continue
		}

		if strings.HasPrefix(source, "/") {
			mounts = append(mounts, fmt.Sprintf("type=bind,source=%s,target=%s", source, target))
		} else {
			namedVolume := d.config.GetVolumeName(source, d.environment)
			mounts = append(mounts, fmt.Sprintf("type=volume,source=%s,target=%s", namedVolume, target))
			if d.config.IsVolumeExternal(source) && !externalSeen[namedVolume] {
				externalVolumes = append(externalVolumes, namedVolume)
				externalSeen[namedVolume] = true
			}
		}
	}
	sort.Strings(externalVolumes)
	return mounts, externalVolumes, nil
}

func (d *Deployer) reconcileBackupScheduleViaTakod(client *ssh.Client, serviceName string, service *config.ServiceConfig, serviceAssignedToNode bool) error {
	if service.Backup == nil || !serviceAssignedToNode {
		return d.deleteBackupScheduleViaTakod(client, serviceName)
	}
	request, err := d.buildTakodBackupScheduleRequest(serviceName, service)
	if err != nil {
		return err
	}
	if _, err := takodclient.RequestJSON(client, d.takodSocket(), "PUT", "/v1/backup-schedule", request); err != nil {
		return fmt.Errorf("takod backup schedule reconciliation failed: %w", err)
	}
	return nil
}

func (d *Deployer) deleteBackupScheduleViaTakod(client *ssh.Client, serviceName string) error {
	endpoint := fmt.Sprintf(
		"/v1/backup-schedule?project=%s&environment=%s&service=%s",
		url.QueryEscape(d.config.Project.Name),
		url.QueryEscape(d.environment),
		url.QueryEscape(serviceName),
	)
	if _, err := takodclient.RequestJSON(client, d.takodSocket(), "DELETE", endpoint, nil); err != nil {
		return fmt.Errorf("takod backup schedule cleanup failed: %w", err)
	}
	return nil
}

func (d *Deployer) buildTakodBackupScheduleRequest(serviceName string, service *config.ServiceConfig) (takod.BackupScheduleRequest, error) {
	volumes := serviceBackupVolumeSources(service)
	if len(service.Backup.Volumes) > 0 {
		volumes = append([]string(nil), service.Backup.Volumes...)
	}
	if len(volumes) == 0 {
		return takod.BackupScheduleRequest{}, fmt.Errorf("service %s has backup configured but no named volumes to back up", serviceName)
	}

	request := takod.BackupScheduleRequest{
		Project:       d.config.Project.Name,
		Environment:   d.environment,
		Service:       serviceName,
		Schedule:      service.Backup.Schedule,
		RetentionDays: service.Backup.Retain,
		Storage:       takodBackupStorageConfig(service.Backup.Storage),
	}
	for _, volume := range volumes {
		request.Volumes = append(request.Volumes, takod.BackupScheduleVolume{
			Volume:         runtimeid.BackupArchiveVolumeName(volume),
			DockerVolume:   d.config.GetVolumeName(volume, d.environment),
			ExternalVolume: d.config.IsVolumeExternal(volume),
		})
	}
	return request, nil
}

func serviceBackupVolumeSources(service *config.ServiceConfig) []string {
	seen := make(map[string]bool)
	var volumes []string
	for _, volume := range service.Volumes {
		source, target, hasTarget := strings.Cut(volume, ":")
		source = strings.TrimSpace(source)
		target = strings.TrimSpace(target)
		if source == "" {
			continue
		}
		if !hasTarget {
			if !seen[source] {
				volumes = append(volumes, source)
				seen[source] = true
			}
			continue
		}
		if target == "" || strings.HasPrefix(source, "/") || config.IsNFSVolume(volume) {
			continue
		}
		if !seen[source] {
			volumes = append(volumes, source)
			seen[source] = true
		}
	}
	sort.Strings(volumes)
	return volumes
}

func takodBackupStorageConfig(storage *config.BackupStorageConfig) *takod.BackupStorageConfig {
	if storage == nil {
		return nil
	}
	return &takod.BackupStorageConfig{
		Provider:        storage.Provider,
		Bucket:          storage.Bucket,
		Region:          storage.Region,
		Endpoint:        storage.Endpoint,
		Prefix:          storage.Prefix,
		AccessKeyID:     storage.AccessKeyID,
		SecretAccessKey: storage.SecretAccessKey,
		SessionToken:    storage.SessionToken,
		ForcePathStyle:  storage.ForcePathStyle,
	}
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
		d.printf("  ✓ Env file created with %d variables\n", envFile.Count())
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
	if service.IsJob() {
		// A job occupies exactly one slot on its owning node; replicas do
		// not apply and zero must not mean scale-to-zero.
		scaleToZero = false
	}
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

func (d *Deployer) takodContainerAlias(serviceName string, slot int) string {
	return runtimeid.ContainerAlias(d.config.Project.Name, d.environment, serviceName, slot)
}

func (d *Deployer) takodContainerNameForStrategy(serviceName string, slot int, revision string, strategy string) (string, error) {
	if !deployStrategyUsesRevisionScopedContainers(strategy) {
		return d.takodContainerName(serviceName, slot), nil
	}
	revision = strings.TrimSpace(revision)
	if revision == "" {
		return "", fmt.Errorf("service %s strategy %s requires a revision-scoped container name", serviceName, strategy)
	}
	return runtimeid.RevisionContainerName(d.config.Project.Name, d.environment, serviceName, revision, slot), nil
}

func (d *Deployer) takodContainerAliasForStrategy(serviceName string, slot int, revision string, strategy string) (string, error) {
	if !deployStrategyUsesRevisionScopedContainers(strategy) {
		return d.takodContainerAlias(serviceName, slot), nil
	}
	revision = strings.TrimSpace(revision)
	if revision == "" {
		return "", fmt.Errorf("service %s strategy %s requires a revision-scoped container alias", serviceName, strategy)
	}
	return runtimeid.RevisionContainerAlias(d.config.Project.Name, d.environment, serviceName, revision, slot), nil
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

func serviceMemoryLimit(service *config.ServiceConfig) string {
	if service == nil || service.Resources == nil {
		return ""
	}
	return strings.TrimSpace(service.Resources.Memory)
}

func (d *Deployer) reconcileServiceViaTakod(client *ssh.Client, request takod.ReconcileServiceRequest) error {
	timeout := reconcileServiceRequestTimeout(request)
	if _, err := takodclient.RequestJSONWithTimeoutContext(d.baseContext(), client, d.takodSocket(), "POST", "/v1/reconcile-service", request, timeout); err != nil {
		return fmt.Errorf("takod service reconciliation failed: %w", err)
	}
	return nil
}

func reconcileServiceRequestTimeout(request takod.ReconcileServiceRequest) time.Duration {
	timeout := takodclient.JSONRequestTimeout
	if request.Health == nil || request.Health.WaitAttempts <= 0 {
		return timeout
	}
	containers := len(request.Containers)
	if containers < 1 {
		containers = 1
	}
	healthWindow := time.Duration(request.Health.WaitAttempts*containers) * time.Second
	calculated := healthWindow + 2*time.Minute
	if calculated < timeout {
		return timeout
	}
	return calculated
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
