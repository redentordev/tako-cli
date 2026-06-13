package deployer

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/hooks"
	"github.com/redentordev/tako-cli/pkg/network"
	"github.com/redentordev/tako-cli/pkg/secrets"
	"github.com/redentordev/tako-cli/pkg/ssh"
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
}

type takodMeshPeer struct {
	Name    string            `json:"name"`
	Host    string            `json:"host"`
	Address string            `json:"address"`
	Labels  map[string]string `json:"labels,omitempty"`
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

	peers, err := d.buildTakodMeshPeers(servers)
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

		if err := d.prepareTakodNode(client, serverName, server, i, peers); err != nil {
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

	if service.Hooks != nil {
		if err := hooks.ValidateHooks(service.Hooks); err != nil {
			return fmt.Errorf("hook validation failed: %w", err)
		}
	}

	var hookExecutor *hooks.Executor
	if len(assignments) > 0 {
		firstTarget := assignments[0].ServerName
		firstClient, err := d.getEnvironmentClient(firstTarget)
		if err != nil {
			return err
		}
		hookExecutor = hooks.NewExecutor(firstClient, d.config.Project.Name, d.environment, serviceName, d.verbose)
		if service.Hooks != nil && len(service.Hooks.PreDeploy) > 0 {
			if err := hookExecutor.ExecutePreDeploy(service.Hooks.PreDeploy, service.Env); err != nil {
				return fmt.Errorf("pre-deploy hooks failed: %w", err)
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

	if hookExecutor != nil && service.Hooks != nil && len(service.Hooks.PostDeploy) > 0 {
		if err := hookExecutor.ExecutePostDeploy(service.Hooks.PostDeploy, service.Env); err != nil {
			return fmt.Errorf("post-deploy hooks failed: %w", err)
		}
	}

	if hookExecutor != nil && service.Hooks != nil && len(service.Hooks.PostStart) > 0 {
		containerName := d.takodContainerName(serviceName, assignments[0].Slot)
		if err := hookExecutor.ExecutePostStart(service.Hooks.PostStart, containerName, service.Env); err != nil {
			return fmt.Errorf("post-start hooks failed: %w", err)
		}
	}

	return nil
}

func (d *Deployer) prepareTakodNode(client *ssh.Client, serverName string, server config.ServerConfig, index int, peers []takodMeshPeer) error {
	if output, err := client.Execute("docker version --format '{{.Server.Version}}' 2>/dev/null"); err != nil || strings.TrimSpace(output) == "" {
		return fmt.Errorf("docker engine is not available")
	}

	dataDir := d.takodDataDir()
	dirs := []string{
		dataDir,
		dataDir + "/desired",
		dataDir + "/actual",
		dataDir + "/events",
		dataDir + "/mesh",
		"/etc/tako",
		"/run/tako",
	}

	var mkdirParts []string
	for _, dir := range dirs {
		mkdirParts = append(mkdirParts, shellQuote(dir))
	}
	if _, err := client.Execute("mkdir -p " + strings.Join(mkdirParts, " ")); err != nil {
		return fmt.Errorf("failed to create takod directories: %w", err)
	}

	meshAddress, err := d.meshAddress(index)
	if err != nil {
		return err
	}

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
		},
		Labels:    server.Labels,
		UpdatedAt: time.Now().UTC(),
	}

	if err := uploadJSON(client, dataDir+"/node.json", nodeState, 0600); err != nil {
		return fmt.Errorf("failed to write node state: %w", err)
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
	if err := uploadJSON(client, dataDir+"/mesh/peers.json", meshState, 0600); err != nil {
		return fmt.Errorf("failed to write mesh peer state: %w", err)
	}

	return nil
}

func (d *Deployer) deployServiceToTakodNode(client *ssh.Client, serverName string, serviceName string, service *config.ServiceConfig, imageRef string, slots []int) error {
	sort.Ints(slots)
	if d.verbose {
		fmt.Printf("  -> %s slots %v\n", serverName, slots)
	}

	if err := d.removeTakodServiceContainers(client, serviceName); err != nil {
		return err
	}

	if len(slots) == 0 {
		return nil
	}

	networkMgr := network.NewManager(client, d.config.Project.Name, d.environment, d.verbose)
	if err := networkMgr.EnsureNetwork(); err != nil {
		return fmt.Errorf("failed to ensure project network: %w", err)
	}
	networkName := networkMgr.GetNetworkName()

	if service.Image != "" {
		if output, err := client.Execute("docker pull " + shellQuote(imageRef) + " 2>&1"); err != nil {
			return fmt.Errorf("failed to pull image %s: %w, output: %s", imageRef, err, output)
		}
	}

	if service.IsPublic() {
		if service.Port <= 0 {
			return fmt.Errorf("service %s has proxy config but no port", serviceName)
		}
		if err := d.ensureTakodProxy(client, networkName, proxyEmail(service)); err != nil {
			return err
		}
	}

	if len(service.Init) > 0 {
		for _, initCmd := range service.Init {
			if output, err := client.Execute(initCmd); err != nil {
				return fmt.Errorf("init command failed: %w, output: %s", err, output)
			}
		}
	}

	envFilePath, cleanupEnv, err := d.uploadTakodEnvFile(client, serviceName, service)
	if err != nil {
		return err
	}
	if cleanupEnv != nil {
		defer cleanupEnv()
	}

	healthChecker := NewHealthChecker(client, d.verbose)
	for _, slot := range slots {
		containerName := d.takodContainerName(serviceName, slot)
		runCmd, err := d.buildTakodRunCommand(containerName, serviceName, service, imageRef, networkName, envFilePath, len(slots))
		if err != nil {
			return err
		}
		output, err := client.Execute(runCmd)
		if err != nil {
			return fmt.Errorf("failed to start container %s: %w, output: %s", containerName, err, output)
		}
		if err := healthChecker.WaitForHealthy(containerName, service.HealthCheck.Retries); err != nil {
			return fmt.Errorf("container %s failed health verification: %w", containerName, err)
		}
	}

	return nil
}

func (d *Deployer) removeTakodServiceContainers(client *ssh.Client, serviceName string) error {
	removeCmd := fmt.Sprintf(
		"docker ps -aq --filter label=tako.project=%s --filter label=tako.environment=%s --filter label=tako.service=%s | xargs -r docker rm -f",
		shellQuote(d.config.Project.Name),
		shellQuote(d.environment),
		shellQuote(serviceName),
	)
	if _, err := client.Execute(removeCmd); err != nil {
		return fmt.Errorf("failed to remove old containers: %w", err)
	}
	return nil
}

func (d *Deployer) buildTakodRunCommand(containerName string, serviceName string, service *config.ServiceConfig, imageRef string, networkName string, envFilePath string, nodeSlotCount int) (string, error) {
	restart := service.Restart
	if restart == "" {
		restart = "unless-stopped"
	}

	args := []string{
		"docker run -d",
		"--name " + shellQuote(containerName),
		"--restart " + shellQuote(restart),
		"--network " + shellQuote(networkName),
		"--network-alias " + shellQuote(serviceName),
		"--label " + shellQuote("tako.project="+d.config.Project.Name),
		"--label " + shellQuote("tako.environment="+d.environment),
		"--label " + shellQuote("tako.service="+serviceName),
		"--label " + shellQuote("tako.runtime="+config.RuntimeModeTakod),
	}

	if envFilePath != "" {
		args = append(args, "--env-file "+shellQuote(envFilePath))
	}

	volumeArgs, err := d.buildTakodVolumeArgs(serviceName, service)
	if err != nil {
		return "", err
	}
	args = append(args, volumeArgs...)

	if service.IsPublic() {
		args = append(args, d.buildTakodProxyLabels(serviceName, service, networkName)...)
	} else if service.Port > 0 {
		if nodeSlotCount > 1 {
			return "", fmt.Errorf("service %s publishes port %d without proxy but has multiple replicas on the same node", serviceName, service.Port)
		}
		args = append(args, fmt.Sprintf("--publish %d:%d", service.Port, service.Port))
	}

	args = append(args, d.buildTakodHealthFlags(service)...)
	args = append(args, shellQuote(imageRef))
	if service.Command != "" {
		args = append(args, service.Command)
	}
	args = append(args, "2>&1")

	return strings.Join(args, " "), nil
}

func (d *Deployer) buildTakodProxyLabels(serviceName string, service *config.ServiceConfig, networkName string) []string {
	routerName := sanitizeRouterName(d.config.Project.Name + "-" + d.environment + "-" + serviceName)
	domains := service.Proxy.GetAllDomains()
	var hostRules []string
	for _, domain := range domains {
		hostRules = append(hostRules, "Host(`"+domain+"`)")
	}
	rule := strings.Join(hostRules, " || ")
	if rule == "" {
		rule = "Host(`" + service.Proxy.GetPrimaryDomain() + "`)"
	}

	labels := []string{
		"traefik.enable=true",
		"traefik.docker.network=" + networkName,
		fmt.Sprintf("traefik.http.services.%s.loadbalancer.server.port=%d", routerName, service.Port),
		fmt.Sprintf("traefik.http.routers.%s.rule=%s", routerName, rule),
		fmt.Sprintf("traefik.http.routers.%s.entrypoints=websecure", routerName),
		fmt.Sprintf("traefik.http.routers.%s.tls=true", routerName),
		fmt.Sprintf("traefik.http.routers.%s.tls.certresolver=letsencrypt", routerName),
		fmt.Sprintf("traefik.http.routers.%s.service=%s", routerName, routerName),
		fmt.Sprintf("traefik.http.routers.%s-http.rule=%s", routerName, rule),
		fmt.Sprintf("traefik.http.routers.%s-http.entrypoints=web", routerName),
		fmt.Sprintf("traefik.http.routers.%s-http.middlewares=%s-redirect", routerName, routerName),
		fmt.Sprintf("traefik.http.middlewares.%s-redirect.redirectscheme.scheme=https", routerName),
	}

	args := make([]string, 0, len(labels))
	for _, label := range labels {
		args = append(args, "--label "+shellQuote(label))
	}
	return args
}

func (d *Deployer) buildTakodHealthFlags(service *config.ServiceConfig) []string {
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

	healthCmd := fmt.Sprintf("curl -sf http://localhost:%d%s || exit 1", service.Port, service.HealthCheck.Path)
	return []string{
		"--health-cmd " + shellQuote(healthCmd),
		"--health-interval " + shellQuote(interval),
		"--health-timeout " + shellQuote(timeout),
		fmt.Sprintf("--health-retries %d", retries),
		"--health-start-period " + shellQuote(startPeriod),
	}
}

func (d *Deployer) buildTakodVolumeArgs(serviceName string, service *config.ServiceConfig) ([]string, error) {
	var args []string
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
			args = append(args, "--mount "+shellQuote(mountOpts))
			continue
		}

		source, target := parseVolumeSpec(volume)
		if target == "" {
			target = source
			source = fmt.Sprintf("%s_%s_%s", d.config.Project.Name, d.environment, sanitizeVolumeName(target))
			args = append(args, "--mount "+shellQuote(fmt.Sprintf("type=volume,source=%s,target=%s", source, target)))
			continue
		}

		if strings.HasPrefix(source, "/") {
			args = append(args, "--mount "+shellQuote(fmt.Sprintf("type=bind,source=%s,target=%s", source, target)))
		} else {
			namedVolume := fmt.Sprintf("%s_%s_%s", d.config.Project.Name, d.environment, source)
			args = append(args, "--mount "+shellQuote(fmt.Sprintf("type=volume,source=%s,target=%s", namedVolume, target)))
		}
	}
	return args, nil
}

func (d *Deployer) ensureTakodProxy(client *ssh.Client, networkName string, email string) error {
	if email == "" {
		email = "tako@redentor.dev"
	}

	setupCmd := "mkdir -p /etc/tako/proxy/acme /var/log/tako/proxy && touch /etc/tako/proxy/acme/acme.json && chmod 600 /etc/tako/proxy/acme/acme.json"
	if _, err := client.Execute(setupCmd); err != nil {
		return fmt.Errorf("failed to prepare proxy directories: %w", err)
	}

	running, _ := client.Execute("docker ps --filter name=^tako-proxy$ --format '{{.Names}}'")
	if strings.TrimSpace(running) == "tako-proxy" {
		_, _ = client.Execute("docker network connect " + shellQuote(networkName) + " tako-proxy 2>/dev/null || true")
		return nil
	}

	_, _ = client.Execute("docker rm -f tako-proxy 2>/dev/null || true")
	cmd := strings.Join([]string{
		"docker run -d",
		"--name tako-proxy",
		"--restart unless-stopped",
		"--network " + shellQuote(networkName),
		"--publish 80:80",
		"--publish 443:443",
		"--volume /var/run/docker.sock:/var/run/docker.sock:ro",
		"--volume /etc/tako/proxy/acme:/acme",
		"--volume /var/log/tako/proxy:/var/log/traefik",
		"traefik:v3.6.1",
		"--api.dashboard=false",
		"--providers.docker=true",
		"--providers.docker.exposedByDefault=false",
		"--providers.docker.watch=true",
		"--entryPoints.web.address=:80",
		"--entryPoints.web.forwardedHeaders.insecure=true",
		"--entryPoints.websecure.address=:443",
		"--entryPoints.websecure.forwardedHeaders.insecure=true",
		"--certificatesResolvers.letsencrypt.acme.email=" + shellQuote(email),
		"--certificatesResolvers.letsencrypt.acme.storage=/acme/acme.json",
		"--certificatesResolvers.letsencrypt.acme.httpChallenge.entryPoint=web",
		"--log.level=INFO",
		"--accessLog.filePath=/var/log/traefik/access.log",
		"--accessLog.format=json",
		"2>&1",
	}, " ")

	output, err := client.Execute(cmd)
	if err != nil {
		return fmt.Errorf("failed to start takod proxy: %w, output: %s", err, output)
	}
	return nil
}

func (d *Deployer) uploadTakodEnvFile(client *ssh.Client, serviceName string, service *config.ServiceConfig) (string, func(), error) {
	hasEnvVars := len(service.Env) > 0 || len(service.Secrets) > 0 || service.EnvFile != ""
	if !hasEnvVars {
		return "", nil, nil
	}

	secretsMgr, err := secrets.NewManager(d.environment)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create secrets manager: %w", err)
	}
	envFile, err := secretsMgr.CreateEnvFile(service)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create env file: %w", err)
	}

	envFilePath := envFile.GetPath(d.config.Project.Name, serviceName)
	if err := client.UploadReader(envFile.ToReader(), envFilePath, 0600); err != nil {
		return "", nil, fmt.Errorf("failed to upload env file: %w", err)
	}

	if d.verbose {
		fmt.Printf("  ✓ Env file created with %d variables\n", envFile.Count())
	}

	cleanup := func() {
		if _, err := client.Execute("rm -f " + shellQuote(envFilePath)); err != nil && d.verbose {
			fmt.Printf("  Warning: failed to cleanup env file: %v\n", err)
		}
	}
	return envFilePath, cleanup, nil
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

func (d *Deployer) buildTakodMeshPeers(servers []string) ([]takodMeshPeer, error) {
	peers := make([]takodMeshPeer, 0, len(servers))
	for i, serverName := range servers {
		server := d.config.Servers[serverName]
		address, err := d.meshAddress(i)
		if err != nil {
			return nil, err
		}
		peers = append(peers, takodMeshPeer{
			Name:    serverName,
			Host:    server.Host,
			Address: address,
			Labels:  server.Labels,
		})
	}
	return peers, nil
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

func uploadJSON(client *ssh.Client, remotePath string, value any, mode os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return client.UploadReader(strings.NewReader(string(data)), remotePath, mode)
}

func parseVolumeSpec(volume string) (source, target string) {
	parts := strings.SplitN(volume, ":", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

func proxyEmail(service *config.ServiceConfig) string {
	if service.Proxy == nil {
		return ""
	}
	if service.Proxy.Email != "" {
		return service.Proxy.Email
	}
	return ""
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

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
