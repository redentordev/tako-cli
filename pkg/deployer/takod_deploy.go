package deployer

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployplan"
	"github.com/redentordev/tako-cli/pkg/mesh"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/nodeidentity"
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

type takodServiceRolloutOperations struct {
	Preflight func() error
	Build     func() error
	Release   func() error
	Reconcile func() error
}

type localImageClient interface {
	CheckAvailable(context.Context) error
	Build(context.Context, takounregistry.BuildRequest) error
	Inspect(context.Context, string) (takounregistry.ImageDescriptor, error)
	Export(context.Context, string, io.Writer) error
}

// SetupTakodRuntime prepares every selected environment server for the takod runtime.
func (d *Deployer) SetupTakodRuntime() error {
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

	if err := d.verifyTakodAgents(servers); err != nil {
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

// verifyTakodAgents is deliberately read-only. Application operations never
// install, restart, or upgrade node software; setup and node lifecycle
// commands own that privileged boundary.
func (d *Deployer) verifyTakodAgents(servers []string) error {
	return runTakodNodeActions(servers, func(serverName string) error {
		if _, ok := d.config.Servers[serverName]; !ok {
			return fmt.Errorf("server %s not found", serverName)
		}
		client, err := d.getRuntimeClient(serverName)
		if err != nil {
			return fmt.Errorf("failed to connect to %s: %w", serverName, err)
		}

		output, err := takodclient.RequestJSON(client, d.takodSocket(), "GET", "/v1/status", nil)
		if err != nil {
			return fmt.Errorf("takod is unavailable on %s; run 'tako setup --server %s' or 'tako upgrade servers --server %s': %w", serverName, serverName, serverName, err)
		}
		var status takod.Status
		if err := json.Unmarshal([]byte(output), &status); err != nil {
			return fmt.Errorf("takod returned invalid status on %s: %w", serverName, err)
		}
		if err := validateTakodAgentForAppOperation(serverName, d.cliVersion, status); err != nil {
			return err
		}
		return nil
	})
}

func validateTakodAgentForAppOperation(serverName string, cliVersion string, status takod.Status) error {
	if strings.TrimSpace(status.Runtime) != "takod" {
		return fmt.Errorf("unexpected node runtime %q on %s", status.Runtime, serverName)
	}
	want := strings.TrimSpace(cliVersion)
	if want == "" || want == "dev" {
		return nil
	}
	got := strings.TrimSpace(status.Version)
	if got != want {
		return fmt.Errorf("takod version %q on %s is incompatible with CLI %q; run 'tako upgrade servers --server %s' before the application operation", got, serverName, want, serverName)
	}
	return nil
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

// DeployPreparedServiceTakod reconciles a service whose shared image was
// already built and transferred to every node selected for its placement.
func (d *Deployer) DeployPreparedServiceTakod(serviceName string, service *config.ServiceConfig, imageRef string, warmOnly bool) error {
	return d.deployServiceTakod(serviceName, service, imageRef, takodServiceDeployOptions{WarmOnly: warmOnly})
}

// EnsurePreparedServiceImage transfers an existing exact shared image to newly
// selected nodes before scale/rollback reconciliation. It never rebuilds.
func (d *Deployer) EnsurePreparedServiceImage(serviceName string, service *config.ServiceConfig, imageRef string) error {
	assignments, err := d.planTakodAssignments(service)
	if err != nil {
		return err
	}
	servers := uniqueAssignmentServers(assignments)
	if err := d.transferExistingImageToNodes(imageRef, servers); err != nil {
		return fmt.Errorf("service %s shared image %s is unavailable for exact transfer: %w", serviceName, imageRef, err)
	}
	return nil
}

// BuildSharedTakodImage builds one top-level build exactly once across the
// union of nodes used by its consumers.
func (d *Deployer) BuildSharedTakodImage(buildName string, build config.SharedBuildConfig, imageRef string, consumers map[string]config.ServiceConfig) error {
	servers, err := d.sharedBuildConsumerServers(buildName, consumers)
	if err != nil {
		return err
	}
	synthetic := &config.ServiceConfig{
		Build: build.Context, BuildArgs: build.Args, BuildTarget: build.Target, Dockerfile: build.Dockerfile,
	}
	if err := d.buildImageOnTakodNodes(buildName, synthetic, imageRef, servers); err != nil {
		return fmt.Errorf("shared build %s failed: %w", buildName, err)
	}
	return nil
}

func (d *Deployer) EnsureSharedTakodImage(buildName string, imageRef string, consumers map[string]config.ServiceConfig) error {
	servers, err := d.sharedBuildConsumerServers(buildName, consumers)
	if err != nil {
		return err
	}
	if err := d.transferExistingImageToNodes(imageRef, servers); err != nil {
		return fmt.Errorf("shared build %s image %s is unavailable: %w", buildName, imageRef, err)
	}
	return nil
}

func (d *Deployer) sharedBuildConsumerServers(buildName string, consumers map[string]config.ServiceConfig) ([]string, error) {
	serverSet := make(map[string]bool)
	for _, service := range consumers {
		assignments, err := d.planTakodAssignments(&service)
		if err != nil {
			return nil, fmt.Errorf("shared build %s placement: %w", buildName, err)
		}
		for _, assignment := range assignments {
			serverSet[assignment.ServerName] = true
		}
	}
	servers := make([]string, 0, len(serverSet))
	for serverName := range serverSet {
		servers = append(servers, serverName)
	}
	sort.Strings(servers)
	if len(servers) == 0 {
		return nil, fmt.Errorf("shared build %s has no consumer placement nodes", buildName)
	}
	return servers, nil
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
	if len(service.Files) > 0 {
		_, _, filesHash, err := d.PrepareServiceFiles(serviceName, service)
		if err != nil {
			return err
		}
		service.FilesContentHash = filesHash
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
		return runTakodJobBuildPhases(func() error {
			if len(service.Files) > 0 {
				if err := d.preflightTakodCapability(assignmentServers, takod.CapabilityServiceFilesV1, "operator file distribution"); err != nil {
					return fmt.Errorf("job %s uses files: %w", serviceName, err)
				}
			}
			if service.Entrypoint.IsSet() {
				if err := d.preflightTakodCapability(assignmentServers, takod.CapabilityContainerArgvV1, "container argv payloads"); err != nil {
					return fmt.Errorf("job %s uses an entrypoint: %w", serviceName, err)
				}
			}
			if serviceNeedsRuntimeControlsCapability(service) {
				if err := d.preflightTakodCapability(assignmentServers, takod.CapabilityContainerRuntimeControlsV1, "container runtime controls"); err != nil {
					return fmt.Errorf("job %s uses container runtime controls: %w", serviceName, err)
				}
			}
			return nil
		}, func() error {
			if options.BuildImage {
				return d.buildImageOnTakodNodes(serviceName, service, imageRef, assignmentServers)
			}
			return nil
		}, func() {
			d.recordJobImage(serviceName, imageRef)
		})
	}
	targetServers, err := d.getTakodTargetServers()
	if err != nil {
		return fmt.Errorf("failed to get takod target servers: %w", err)
	}
	grouped := groupTakodAssignments(assignments)
	needsCapabilities := serviceNeedsTakodCapabilityPreflight(service)
	return runTakodServiceRollout(needsCapabilities, takodServiceRolloutOperations{
		Preflight: func() error {
			if len(service.Files) > 0 {
				if err := d.preflightTakodCapability(targetServers, takod.CapabilityServiceFilesV1, "operator file distribution"); err != nil {
					return fmt.Errorf("service %s uses files: %w", serviceName, err)
				}
				if service.ReuseFiles {
					if err := d.preflightTakodServiceFileSet(assignmentServers, serviceName, service.FilesContentHash); err != nil {
						return fmt.Errorf("service %s historical operator files are unavailable: %w", serviceName, err)
					}
				}
			}
			if serviceNeedsContainerArgvCapability(service) {
				if err := d.preflightTakodCapability(targetServers, takod.CapabilityContainerArgvV1, "container argv payloads"); err != nil {
					return fmt.Errorf("service %s uses list-form command or entrypoint: %w", serviceName, err)
				}
			}
			if serviceNeedsRuntimeControlsCapability(service) {
				if err := d.preflightTakodCapability(targetServers, takod.CapabilityContainerRuntimeControlsV1, "container runtime controls"); err != nil {
					return fmt.Errorf("service %s uses container runtime controls: %w", serviceName, err)
				}
			}
			return nil
		},
		Build: func() error {
			if !options.BuildImage {
				return nil
			}
			return d.buildImageOnTakodNodes(serviceName, service, imageRef, assignmentServers)
		},
		// Release runs once per service after its image exists and before any
		// rollout activation in reconcile.
		Release: func() error {
			return d.runReleaseCommand(serviceName, service, imageRef, assignmentServers)
		},
		Reconcile: func() error {
			return runTakodNodeActions(targetServers, func(serverName string) error {
				slots := grouped[serverName]
				client, err := d.getRuntimeClient(serverName)
				if err != nil {
					return err
				}
				return d.deployServiceToTakodNode(client, serverName, serviceName, service, imageRef, slots, options.PullImage, options.WarmOnly)
			})
		},
	})
}

func runTakodJobBuildPhases(preflight func() error, build func() error, record func()) error {
	if err := preflight(); err != nil {
		return err
	}
	if err := build(); err != nil {
		return err
	}
	record()
	return nil
}

func runTakodServiceRollout(needsArgvCapability bool, operations takodServiceRolloutOperations) error {
	if needsArgvCapability {
		if err := operations.Preflight(); err != nil {
			return err
		}
	}
	if err := operations.Build(); err != nil {
		return err
	}
	if err := operations.Release(); err != nil {
		return err
	}
	return operations.Reconcile()
}

func serviceNeedsContainerArgvCapability(service *config.ServiceConfig) bool {
	return service != nil && (service.Command.IsList() || service.Entrypoint.IsSet())
}

func serviceNeedsRuntimeControlsCapability(service *config.ServiceConfig) bool {
	return service != nil && (service.User != "" || service.WorkingDir != "" || service.StopGracePeriod != "" || service.Init || len(service.ExtraHosts) > 0 || len(service.Ulimits) > 0 || service.ShmSize != "")
}

func serviceNeedsTakodCapabilityPreflight(service *config.ServiceConfig) bool {
	return serviceNeedsContainerArgvCapability(service) || serviceNeedsRuntimeControlsCapability(service) || (service != nil && len(service.Files) > 0)
}

func (d *Deployer) preflightTakodContainerArgv(serverNames []string) error {
	return d.preflightTakodCapability(serverNames, takod.CapabilityContainerArgvV1, "container argv payloads")
}

func (d *Deployer) preflightTakodCapability(serverNames []string, capability string, feature string) error {
	return preflightTakodContainerArgvWithCheck(serverNames, func(serverName string) error {
		client, err := d.getRuntimeClient(serverName)
		if err != nil {
			return err
		}
		return d.ensureTakodCapability(client, serverName, capability, feature)
	})
}

func (d *Deployer) preflightTakodServiceFileSet(serverNames []string, serviceName string, contentHash string) error {
	setID, err := serviceFileSetID(contentHash)
	if err != nil {
		return err
	}
	return runTakodNodeActions(serverNames, func(serverName string) error {
		client, err := d.getRuntimeClient(serverName)
		if err != nil {
			return err
		}
		_, err = takodclient.RequestJSONWithContext(d.baseContext(), client, d.takodSocket(), "POST", "/v1/service-files/check", takod.ServiceFilesCheckRequest{
			Project: d.config.Project.Name, Environment: d.environment, Service: serviceName, FileSetID: setID,
		})
		if err != nil {
			return fmt.Errorf("%s: %w", serverName, err)
		}
		return nil
	})
}

func preflightTakodContainerArgvWithCheck(serverNames []string, check func(string) error) error {
	return runTakodNodeActions(serverNames, check)
}

func (d *Deployer) ensureTakodContainerArgvCapability(client any, serverName string) error {
	return d.ensureTakodCapability(client, serverName, takod.CapabilityContainerArgvV1, "container argv payloads")
}

func (d *Deployer) ensureTakodCapability(client any, serverName string, required string, feature string) error {
	return takodclient.RequireCapability(d.baseContext(), client, d.takodSocket(), serverName, required, feature)
}

func (d *Deployer) buildImageOnTakodNodes(serviceName string, service *config.ServiceConfig, imageRef string, serverNames []string) error {
	strategy := config.BuildStrategyRemote
	if d.config != nil {
		strategy = d.config.GetBuildStrategy()
	}
	var builders []string
	resolveBuilders := func() ([]string, error) {
		if builders != nil {
			return builders, nil
		}
		var err error
		builders, err = d.remoteBuilderServers()
		return builders, err
	}
	fallbackCause, err := runTakodBuildStrategy(
		strategy,
		func() error {
			return d.buildImageLocallyAndPushToTakodNodes(serviceName, service, imageRef, serverNames)
		},
		func() error {
			resolved, err := resolveBuilders()
			if err != nil {
				return err
			}
			return d.preflightTakodBuildOptions(service, resolved)
		},
		func() error {
			resolved, err := resolveBuilders()
			if err != nil {
				return err
			}
			return d.buildImageRemotelyOnTakodNodes(serviceName, service, imageRef, serverNames, resolved)
		},
	)
	if fallbackCause != nil && d.verbose {
		d.printf("  Local build unavailable, falling back to remote takod build: %v\n", fallbackCause)
	}
	return err
}

func (d *Deployer) remoteBuilderServers() ([]string, error) {
	builders := make([]string, 0, len(d.config.Servers))
	for serverName, server := range d.config.Servers {
		if server.Schedulable() && server.HasPlatformRole(nodeidentity.RoleBuilder) {
			builders = append(builders, serverName)
		}
	}
	if len(builders) == 0 {
		return nil, fmt.Errorf("environment %s has no schedulable builder-role nodes for remote image builds", d.environment)
	}
	sort.Strings(builders)
	return builders, nil
}

func runTakodBuildStrategy(strategy string, local func() error, preflightRemote func() error, remote func() error) (error, error) {
	if strategy == config.BuildStrategyLocal {
		return nil, local()
	}
	if strategy == config.BuildStrategyAuto {
		localErr := local()
		if localErr == nil {
			return nil, nil
		}
		if err := preflightRemote(); err != nil {
			return localErr, err
		}
		return localErr, remote()
	}
	if err := preflightRemote(); err != nil {
		return nil, err
	}
	return nil, remote()
}

func (d *Deployer) preflightTakodBuildOptions(service *config.ServiceConfig, serverNames []string) error {
	if service == nil || (len(service.BuildArgs) == 0 && service.BuildTarget == "") {
		return nil
	}
	if err := d.preflightTakodCapability(serverNames, takod.CapabilityImageBuildOptionsV1, "structured image build options"); err != nil {
		return fmt.Errorf("build uses args or target: %w", err)
	}
	return nil
}

func (d *Deployer) buildImageRemotelyOnTakodNodes(serviceName string, service *config.ServiceConfig, imageRef string, serverNames, builderNames []string) error {
	if len(serverNames) == 0 {
		return fmt.Errorf("service %s has no assigned takod nodes for build", serviceName)
	}

	if d.verbose {
		d.printf("  Building image on %d assigned node(s) with remote takod builder...\n", len(serverNames))
	}

	allNames := append(append([]string(nil), serverNames...), builderNames...)
	seenNames := make(map[string]struct{}, len(allNames))
	uniqueNames := allNames[:0]
	for _, name := range allNames {
		if _, seen := seenNames[name]; seen {
			continue
		}
		seenNames[name] = struct{}{}
		uniqueNames = append(uniqueNames, name)
	}
	platformGroups, err := d.groupTakodNodesByPlatform(uniqueNames)
	if err != nil {
		return err
	}
	targetSet := make(map[string]struct{}, len(serverNames))
	for _, name := range serverNames {
		targetSet[name] = struct{}{}
	}
	builderSet := make(map[string]struct{}, len(builderNames))
	for _, name := range builderNames {
		builderSet[name] = struct{}{}
	}
	platforms := make([]string, 0, len(platformGroups))
	for platform := range platformGroups {
		platforms = append(platforms, platform)
	}
	sort.Strings(platforms)
	for _, platform := range platforms {
		var targets, sources []string
		for _, name := range platformGroups[platform] {
			if _, ok := targetSet[name]; ok {
				targets = append(targets, name)
			}
			if _, ok := builderSet[name]; ok {
				sources = append(sources, name)
			}
		}
		sort.Strings(targets)
		if len(targets) == 0 {
			continue
		}
		sort.Strings(sources)
		if len(sources) == 0 {
			return fmt.Errorf("no schedulable builder-role node supports target platform %s", platform)
		}
		source := sources[0]
		if d.verbose {
			d.printf("  [%s] Building %s for %s\n", source, imageRef, platform)
		}
		buildStart := time.Now()
		if _, err := d.buildImageOnNode(source, serviceName, service, imageRef); err != nil {
			return fmt.Errorf("failed to build image on %s: %w", source, err)
		}
		expected, err := d.inspectImageOnTakodNode(source, imageRef)
		if err != nil {
			return fmt.Errorf("inspect newly built image on %s: %w", source, err)
		}
		if !expected.Exists {
			return fmt.Errorf("inspect newly built image on %s: image is absent after build", source)
		}
		if d.verbose {
			d.printf("  [%s] Image ready: %s (%s, %s)\n", source, imageRef, expected.ImageID, formatBuildDuration(time.Since(buildStart)))
		}
		for _, target := range targets {
			if target == source {
				continue
			}
			actual, inspectErr := d.inspectImageOnTakodNode(target, imageRef)
			if inspectErr == nil && sameImageContent(expected, actual) {
				continue
			}
			if err := d.transferImageBetweenNodes(source, target, imageRef); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *Deployer) transferExistingImageToNodes(imageRef string, requiredServers []string) error {
	allServers, err := d.getTakodTargetServers()
	if err != nil {
		return err
	}
	// A dedicated builder remains a valid exact-image source even when it is
	// not a workload placement target for this environment. Export is
	// read-only; imports remain restricted to required schedulable workers.
	if builders, builderErr := d.remoteBuilderServers(); builderErr == nil {
		allServers = mergeTakodImageSourceNames(allServers, builders)
	}
	allGroups, err := d.groupTakodNodesByPlatform(allServers)
	if err != nil {
		return err
	}
	requiredGroups, err := d.groupTakodNodesByPlatform(requiredServers)
	if err != nil {
		return err
	}
	platforms := make([]string, 0, len(requiredGroups))
	for platform := range requiredGroups {
		platforms = append(platforms, platform)
	}
	sort.Strings(platforms)
	for _, platform := range platforms {
		sources := append([]string(nil), allGroups[platform]...)
		sort.Strings(sources)
		source := ""
		var expected *takod.ImageDescriptor
		for _, candidate := range sources {
			descriptor, inspectErr := d.inspectImageOnTakodNode(candidate, imageRef)
			if inspectErr != nil || !descriptor.Exists {
				continue
			}
			if expected == nil {
				source, expected = candidate, descriptor
				continue
			}
			if !sameImageContent(expected, descriptor) {
				return fmt.Errorf("image tag %s resolves to conflicting digests on %s (%s) and %s (%s); refusing ambiguous reuse", imageRef, source, expected.ImageID, candidate, descriptor.ImageID)
			}
		}
		if source == "" {
			return fmt.Errorf("no %s node retains the exact image", platform)
		}
		targets := requiredGroups[platform]
		sort.Strings(targets)
		for _, target := range targets {
			actual, inspectErr := d.inspectImageOnTakodNode(target, imageRef)
			if inspectErr == nil && sameImageContent(expected, actual) {
				continue
			}
			if err := d.transferImageBetweenNodes(source, target, imageRef); err != nil {
				return err
			}
		}
	}
	return nil
}

func mergeTakodImageSourceNames(targets, builders []string) []string {
	merged := append([]string(nil), targets...)
	seen := make(map[string]struct{}, len(targets)+len(builders))
	for _, name := range targets {
		seen[name] = struct{}{}
	}
	for _, name := range builders {
		if _, ok := seen[name]; ok {
			continue
		}
		merged = append(merged, name)
		seen[name] = struct{}{}
	}
	return merged
}

func (d *Deployer) transferImageBetweenNodes(sourceName string, targetName string, imageRef string) error {
	source, err := d.getRuntimeClient(sourceName)
	if err != nil {
		return err
	}
	target, err := d.getRuntimeClient(targetName)
	if err != nil {
		return err
	}
	if d.verbose {
		d.printf("  [%s -> %s] Transferring exact image %s\n", sourceName, targetName, imageRef)
	}
	expected, err := d.inspectImageOnTakodNode(sourceName, imageRef)
	if err != nil {
		return fmt.Errorf("inspect source image %s on %s: %w", imageRef, sourceName, err)
	}
	if !expected.Exists {
		return fmt.Errorf("source image %s is absent on %s", imageRef, sourceName)
	}
	if actual, inspectErr := d.inspectImageOnTakodNode(targetName, imageRef); inspectErr == nil && sameImageContent(expected, actual) {
		return nil
	}
	var sourceStderr strings.Builder
	var targetStderr strings.Builder
	var importOutput string
	sourceErr, targetErr := streamExactImageTransfer(d.baseContext(),
		func(ctx context.Context, output io.Writer) error {
			return source.StreamOutput(ctx, "GET", takodclient.ImageExportEndpoint(imageRef, expected.ImageID), nil, "", output, &sourceStderr)
		},
		func(ctx context.Context, input io.Reader) error {
			var importErr error
			importOutput, importErr = target.StreamRequest(ctx, "POST", takodclient.ImageImportEndpoint(imageRef, expected.ImageID), input, "application/x-tar")
			return importErr
		},
	)
	if targetErr != nil {
		return fmt.Errorf("failed to import image %s on %s: %w: %s (source %s stopped with: %v)", imageRef, targetName, targetErr, strings.TrimSpace(targetStderr.String()), sourceName, sourceErr)
	}
	if sourceErr != nil {
		return fmt.Errorf("failed to export image %s from %s: %w: %s", imageRef, sourceName, sourceErr, strings.TrimSpace(sourceStderr.String()))
	}
	var imported takod.ImageImportResponse
	if err := json.Unmarshal([]byte(importOutput), &imported); err != nil {
		return fmt.Errorf("decode image import response from %s: %w", targetName, err)
	}
	actual, err := d.inspectImageOnTakodNode(targetName, imageRef)
	if err != nil {
		return fmt.Errorf("inspect image %s on %s after transfer: %w", imageRef, targetName, err)
	}
	if !sameImageContent(expected, actual) {
		return fmt.Errorf("image %s on %s has digest/platform %s/%s/%s, want %s/%s/%s from %s", imageRef, targetName, actual.ImageID, actual.OS, actual.Architecture, expected.ImageID, expected.OS, expected.Architecture, sourceName)
	}
	return nil
}

func streamExactImageTransfer(ctx context.Context, save func(context.Context, io.Writer) error, load func(context.Context, io.Reader) error) (error, error) {
	transferCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	reader, writer := io.Pipe()
	sourceErrCh := make(chan error, 1)
	go func() {
		sourceErr := save(transferCtx, writer)
		_ = writer.CloseWithError(sourceErr)
		sourceErrCh <- sourceErr
	}()
	targetErr := load(transferCtx, reader)
	if targetErr != nil {
		cancel()
		_ = reader.CloseWithError(targetErr)
	} else {
		_ = reader.Close()
	}
	return <-sourceErrCh, targetErr
}

func (d *Deployer) buildImageLocallyAndPushToTakodNodes(serviceName string, service *config.ServiceConfig, imageRef string, serverNames []string) error {
	if len(serverNames) == 0 {
		return fmt.Errorf("service %s has no assigned takod nodes for build", serviceName)
	}

	// A local build is authoritative for this deploy. Without a descriptor
	// from the local daemon, a matching remote tag is not reuse evidence, so
	// push to every assigned node and verify the resulting node descriptors.
	missingServers := append([]string(nil), serverNames...)

	if d.verbose {
		d.printf("  Building image locally with docker buildx and importing through structured transport to %d node(s)...\n", len(missingServers))
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
			Args:       service.BuildArgs,
			Target:     service.BuildTarget,
		}); err != nil {
			return err
		}
		if d.verbose {
			d.printf("  Local build ready for %s (%s)\n", platform, formatBuildDuration(time.Since(buildStart)))
		}
		localDescriptor, err := localClient.Inspect(ctx, imageRef)
		if err != nil {
			return fmt.Errorf("inspect locally built image %s: %w", imageRef, err)
		}
		if got := localDescriptor.OS + "/" + localDescriptor.Architecture; !strings.HasPrefix(platform, got) {
			return fmt.Errorf("locally built image platform %s/%s does not match requested %s", localDescriptor.OS, localDescriptor.Architecture, platform)
		}

		var expected *takod.ImageDescriptor
		for _, serverName := range targets {
			if err := d.importLocalImageToTakodNode(ctx, localClient, localDescriptor, imageRef, serverName); err != nil {
				return err
			}
			descriptor, err := d.inspectImageOnTakodNode(serverName, imageRef)
			if err != nil {
				return fmt.Errorf("verify locally built image %s on %s: %w", imageRef, serverName, err)
			}
			if !descriptor.Exists {
				return fmt.Errorf("verify locally built image %s on %s: image is absent after push", imageRef, serverName)
			}
			if expected == nil {
				expected = descriptor
			} else if !sameImageContent(expected, descriptor) {
				return fmt.Errorf("local image push produced conflicting digests for %s/%s: %s on first target, %s on %s", imageRef, platform, expected.ImageID, descriptor.ImageID, serverName)
			}
			if d.verbose {
				d.printf("  [%s] Image ready: %s (%s)\n", serverName, imageRef, descriptor.ImageID)
			}
		}
	}

	return nil
}

func (d *Deployer) importLocalImageToTakodNode(ctx context.Context, localClient localImageClient, local takounregistry.ImageDescriptor, imageRef string, serverName string) error {
	target, err := d.getRuntimeClient(serverName)
	if err != nil {
		return err
	}
	if d.verbose {
		d.printf("  [%s] Importing exact local image %s through structured runtime transport\n", serverName, imageRef)
	}
	if actual, inspectErr := d.inspectImageOnTakodNode(serverName, imageRef); inspectErr == nil && actual.Exists && actual.ImageID == local.ImageID && actual.OS == local.OS && actual.Architecture == local.Architecture && actual.Variant == local.Variant {
		return nil
	}
	var output string
	sourceErr, targetErr := streamExactImageTransfer(ctx,
		func(ctx context.Context, output io.Writer) error {
			return localClient.Export(ctx, local.ImageID, output)
		},
		func(ctx context.Context, input io.Reader) error {
			var importErr error
			output, importErr = target.StreamRequest(ctx, "POST", takodclient.ImageImportEndpoint(imageRef, local.ImageID), input, "application/x-tar")
			return importErr
		},
	)
	if targetErr != nil {
		return fmt.Errorf("failed to import local image %s on %s: %w", imageRef, serverName, targetErr)
	}
	if sourceErr != nil {
		return fmt.Errorf("failed to export local image %s: %w", imageRef, sourceErr)
	}
	var imported takod.ImageImportResponse
	if err := json.Unmarshal([]byte(output), &imported); err != nil {
		return fmt.Errorf("decode local image import response from %s: %w", serverName, err)
	}
	if d.verbose {
		d.printf("  [%s] Structured image import complete\n", serverName)
	}
	return nil
}

func (d *Deployer) localImageTransferClient() localImageClient {
	if d.localImageClient != nil {
		return d.localImageClient
	}
	client := takounregistry.Client{}
	if d.verbose {
		client.Stdout = d.outputWriter()
		client.Stderr = d.outputWriter()
	}
	return client
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
	response, err := d.readTakodNodePlatform(serverName)
	if err != nil {
		return "", err
	}
	if response.OS != "linux" {
		return "", fmt.Errorf("unsupported node OS %q on %s", response.OS, serverName)
	}
	platform, err := normalizeLinuxBuildPlatform(response.Architecture)
	if err != nil {
		return "", fmt.Errorf("failed to normalize platform on %s: %w", serverName, err)
	}
	if strings.TrimSpace(response.Variant) != "" {
		platform += "/" + strings.TrimSpace(response.Variant)
	}
	return platform, nil
}

func (d *Deployer) readTakodNodePlatform(serverName string) (*takod.PlatformResponse, error) {
	client, err := d.getRuntimeClient(serverName)
	if err != nil {
		return nil, err
	}
	output, err := takodclient.RequestJSONWithContext(d.baseContext(), client, d.takodSocket(), "GET", "/v1/platform", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to detect platform on %s: %w", serverName, err)
	}
	var response takod.PlatformResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("failed to decode platform on %s: %w", serverName, err)
	}
	response.OS = strings.ToLower(strings.TrimSpace(response.OS))
	response.Architecture = strings.ToLower(strings.TrimSpace(response.Architecture))
	response.Variant = strings.ToLower(strings.TrimSpace(response.Variant))
	response.DaemonID = strings.TrimSpace(response.DaemonID)
	if response.OS == "" || response.Architecture == "" || response.DaemonID == "" {
		return nil, fmt.Errorf("Docker platform response on %s omitted OS, architecture, or daemon identity", serverName)
	}
	return &response, nil
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

func (d *Deployer) inspectImageOnTakodNode(serverName string, imageRef string) (*takod.ImageDescriptor, error) {
	client, err := d.getRuntimeClient(serverName)
	if err != nil {
		return nil, err
	}
	output, err := takodclient.RequestJSON(client, d.takodSocket(), "GET", takodclient.ImageInspectEndpoint(imageRef), nil)
	if err != nil {
		return nil, err
	}
	var response takod.ImageDescriptor
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("failed to parse image descriptor response: %w", err)
	}
	if response.Image != imageRef {
		return nil, fmt.Errorf("image descriptor returned identity %q, want %q", response.Image, imageRef)
	}
	if response.Exists && (response.ImageID == "" || response.OS == "" || response.Architecture == "" || response.DaemonID == "") {
		return nil, fmt.Errorf("image descriptor for %s on %s omitted immutable evidence", imageRef, serverName)
	}
	if response.Exists {
		platform, err := d.readTakodNodePlatform(serverName)
		if err != nil {
			return nil, fmt.Errorf("re-attest Docker daemon for image %s on %s: %w", imageRef, serverName, err)
		}
		nodeArch, err := normalizeLinuxBuildPlatform(platform.Architecture)
		if err != nil {
			return nil, fmt.Errorf("normalize Docker daemon platform on %s: %w", serverName, err)
		}
		imageArch, err := normalizeLinuxBuildPlatform(response.Architecture)
		if err != nil {
			return nil, fmt.Errorf("normalize image platform on %s: %w", serverName, err)
		}
		if response.DaemonID != platform.DaemonID || response.OS != platform.OS || imageArch != nodeArch || (platform.Variant != "" && strings.TrimSpace(response.Variant) != strings.TrimSpace(platform.Variant)) {
			return nil, fmt.Errorf("image descriptor for %s on %s does not match the currently attested Docker daemon/platform", imageRef, serverName)
		}
	}
	return &response, nil
}

func sameImageContent(expected *takod.ImageDescriptor, actual *takod.ImageDescriptor) bool {
	return expected != nil && actual != nil && expected.Exists && actual.Exists &&
		expected.ImageID == actual.ImageID && expected.OS == actual.OS &&
		expected.Architecture == actual.Architecture && expected.Variant == actual.Variant
}

func (d *Deployer) RemoveServiceTakod(serviceName string) error {
	targetServers, err := d.getTakodTargetServers()
	if err != nil {
		return fmt.Errorf("failed to get takod target servers: %w", err)
	}
	if len(targetServers) == 0 {
		return fmt.Errorf("environment %s has no target servers", d.environment)
	}

	return runTakodNodeActions(targetServers, func(serverName string) error {
		client, err := d.getRuntimeClient(serverName)
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
	requests := takodRevisionPruneRequests(d.config.Project.Name, d.environment, normalizedRevisions)
	targetServers, err := d.getTakodTargetServers()
	if err != nil {
		return fmt.Errorf("failed to get takod target servers: %w", err)
	}
	if len(targetServers) == 0 {
		return fmt.Errorf("environment %s has no target servers", d.environment)
	}

	return runTakodNodeActions(targetServers, func(serverName string) error {
		client, err := d.getRuntimeClient(serverName)
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

	var failures []takodNodeActionResult
	for result := range resultCh {
		if result.err != nil {
			failures = append(failures, result)
		}
	}
	if len(failures) > 0 {
		sort.Slice(failures, func(i, j int) bool { return failures[i].serverName < failures[j].serverName })
		return &takodNodeActionsError{failures: failures}
	}
	return nil
}

type takodNodeActionsError struct {
	failures []takodNodeActionResult
}

func (e *takodNodeActionsError) Error() string {
	parts := make([]string, 0, len(e.failures))
	for _, failure := range e.failures {
		parts = append(parts, fmt.Sprintf("%s: %v", failure.serverName, failure.err))
	}
	return strings.Join(parts, "; ")
}

func (e *takodNodeActionsError) Unwrap() []error {
	errs := make([]error, 0, len(e.failures))
	for _, failure := range e.failures {
		errs = append(errs, failure.err)
	}
	return errs
}

func (d *Deployer) prepareTakodNode(client any, serverName string, server config.ServerConfig, index int, peers []takodMeshPeer, publicKey string) error {
	meshAddress, err := d.meshAddressForServer(server, index)
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
	metadata := takod.MetadataRequest{Node: nodeState, Peers: meshState}
	enrolled := strings.TrimSpace(server.ClusterID) != "" && strings.TrimSpace(server.NodeID) != ""
	if enrolled {
		// Cluster topology and peer credentials come from protected platform
		// membership, never from one application's desired-state document.
		metadata.Peers = nil
	}
	if _, err := takodclient.RequestJSON(client, d.takodSocket(), "PUT", "/v1/metadata", metadata); err != nil {
		return fmt.Errorf("failed to write takod metadata: %w", err)
	}

	meshNode := mesh.Node{
		Name:      serverName,
		Host:      server.Host,
		Address:   meshAddress,
		PublicKey: publicKey,
		Labels:    server.Labels,
	}
	if enrolled {
		return nil
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
		client, err := d.getRuntimeClient(serverName)
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

func (d *Deployer) deployServiceToTakodNode(client any, serverName string, serviceName string, service *config.ServiceConfig, imageRef string, slots []int, pullImage bool, warmOnly bool) error {
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
	var fileBundles []takod.ServiceFileBundle
	fileSetID := ""
	if len(service.Files) > 0 && (!service.ReuseFiles || len(slots) > 0) {
		var fileMounts []string
		fileBundles, fileMounts, _, err = d.PrepareServiceFiles(serviceName, service)
		if err != nil {
			return err
		}
		if len(slots) > 0 {
			mounts = append(mounts, fileMounts...)
		}
		fileSetID, err = serviceFileSetID(service.FilesContentHash)
		if err != nil {
			return err
		}
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
		Entrypoint:         service.Entrypoint,
		Labels:             serviceRuntimeLabels(d.config.Project.Name, d.environment, serviceName, *service),
		ExternalVolumes:    externalVolumes,
		Files:              fileBundles,
		FileSetID:          fileSetID,
		MemoryLimit:        serviceMemoryLimit(service),
		CPULimit:           serviceCPULimit(service),
		User:               service.User,
		WorkingDir:         service.WorkingDir,
		StopTimeoutSeconds: serviceStopTimeoutSeconds(service),
		Init:               service.Init,
		ExtraHosts:         append([]string(nil), service.ExtraHosts...),
		Ulimits:            copyServiceUlimits(service.Ulimits),
		ShmSize:            service.ShmSize,
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
	for _, entry := range service.Ports {
		publish, err := config.ParsePortPublish(entry)
		if err != nil {
			return container, fmt.Errorf("service %s: %w", serviceName, err)
		}
		container.Publishes = append(container.Publishes, publish.String())
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
	labels := make(map[string]string, len(service.Labels)+3)
	for key, value := range service.Labels {
		labels[key] = value
	}
	labels[runtimeid.ServiceIdentityLabel] = runtimeid.ServiceIdentity(project, environment, serviceName)
	labels["tako.persistent"] = strconv.FormatBool(service.Persistent)
	configHash, ok := reconcile.SafeServiceConfigHash(service)
	if !ok {
		return labels
	}
	labels[reconcile.ConfigHashLabel] = configHash
	return labels
}

func (d *Deployer) buildTakodHealthSpec(service *config.ServiceConfig) *takod.HealthSpec {
	if readinessSpec := d.buildTakodReadinessHealthSpec(service); readinessSpec != nil {
		readinessSpec.Command = service.HealthCheck.Command
		return attachTakodSmokeSpec(readinessSpec, service)
	}
	if service.HealthCheck.Command == "" && service.HealthCheck.Path == "" && service.HealthCheck.TCPPort <= 0 {
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
		Command:      service.HealthCheck.Command,
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
	if attempts > 24*60*60 {
		return 24 * 60 * 60
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

func (d *Deployer) reconcileBackupScheduleViaTakod(client any, serviceName string, service *config.ServiceConfig, serviceAssignedToNode bool) error {
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

func (d *Deployer) deleteBackupScheduleViaTakod(client any, serviceName string) error {
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

func (d *Deployer) ensureTakodProxy(client any, networkName string, email string) error {
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
	content, _, err := d.buildTakodEnvFileContentAndHash(service)
	return content, err
}

func (d *Deployer) buildTakodEnvFileContentAndHash(service *config.ServiceConfig) (string, string, error) {
	hasEnvVars := len(service.Env) > 0 || len(service.Secrets) > 0 || service.EnvFile != "" || len(service.EnvFiles) > 0
	if !hasEnvVars {
		return "", runInputValuesHash(nil), nil
	}

	secretsMgr, err := secrets.NewManager(d.environment)
	if err != nil {
		return "", "", fmt.Errorf("failed to create secrets manager: %w", err)
	}
	envFile, err := secretsMgr.CreateEnvFile(service)
	if err != nil {
		return "", "", fmt.Errorf("failed to create env file: %w", err)
	}

	data, err := io.ReadAll(envFile.ToReader())
	if err != nil {
		return "", "", fmt.Errorf("failed to read env file: %w", err)
	}

	if d.verbose {
		d.printf("  ✓ Env file created with %d variables\n", envFile.Count())
	}

	return string(data), runInputValuesHash(envFile.GetAll()), nil
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
	if service.IsJob() || service.IsRun() {
		// Jobs and deploy-time runs have exactly one owning node; replicas do
		// not apply and zero must not mean scale-to-zero.
		scaleToZero = false
		replicas = 1
	}
	if replicas <= 0 {
		replicas = 1
	}

	targets, err := config.ResolveSchedulablePlacementTargets(service.Placement, d.config.Servers, servers, d.environment)
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
		client, err := d.getRuntimeClient(serverName)
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

func (d *Deployer) meshAddressForServer(server config.ServerConfig, index int) (string, error) {
	if strings.TrimSpace(server.MeshIP) != "" {
		if net.ParseIP(strings.TrimSpace(server.MeshIP)) == nil {
			return "", fmt.Errorf("invalid platform mesh IP %q", server.MeshIP)
		}
		bits := 32
		if d.config.Mesh != nil && d.config.Mesh.SubnetBits > 0 {
			bits = d.config.Mesh.SubnetBits
		}
		return fmt.Sprintf("%s/%d", strings.TrimSpace(server.MeshIP), bits), nil
	}
	return d.meshAddress(index)
}

func (d *Deployer) getTakodTargetServers() ([]string, error) {
	var targets []string
	if len(d.targetServers) > 0 {
		targets = append([]string(nil), d.targetServers...)
	} else {
		var err error
		targets, err = d.config.GetEnvironmentServers(d.environment)
		if err != nil {
			return nil, err
		}
	}
	return config.ResolveSchedulableEnvironmentTargets(d.config.Servers, targets, d.environment)
}

type directSSHClientPool struct {
	client *ssh.Client
}

func (p directSSHClientPool) GetOrCreateWithAuth(_ string, _ int, _ string, _ string, _ string) (*ssh.Client, error) {
	if p.client == nil {
		return nil, fmt.Errorf("SSH client is not initialized")
	}
	return p.client, nil
}

// getRuntimeClient returns only the structured takod surface. Transport is
// resolved once from immutable identity before any SSH attempt; callers cannot
// gain a shell through this client.
func (d *Deployer) getRuntimeClient(serverName string) (*takodclient.AgentClient, error) {
	d.runtimeFactoryMu.Lock()
	if d.runtimeFactory == nil {
		var pool nodeclient.SSHPool = d.sshPool
		if pool == nil {
			if client, ok := d.client.(*ssh.Client); ok && client != nil {
				pool = directSSHClientPool{client: client}
			}
		}
		factory, err := nodeclient.NewFactory(d.config, pool, d.takodSocket())
		if err != nil {
			d.runtimeFactoryMu.Unlock()
			return nil, err
		}
		d.runtimeFactory = factory
	}
	factory := d.runtimeFactory
	d.runtimeFactoryMu.Unlock()
	client, decision, err := factory.Client(d.baseContext(), serverName)
	if err != nil {
		return nil, err
	}
	if d.verbose {
		d.printf("  [%s] Runtime transport: %s (%s)\n", serverName, decision.Transport, decision.Evidence)
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

func serviceCPULimit(service *config.ServiceConfig) string {
	if service == nil || service.Resources == nil {
		return ""
	}
	return strings.TrimSpace(service.Resources.CPUs)
}

func serviceStopTimeoutSeconds(service *config.ServiceConfig) int {
	if service == nil || strings.TrimSpace(service.StopGracePeriod) == "" {
		return 0
	}
	duration, err := time.ParseDuration(service.StopGracePeriod)
	if err != nil || duration <= 0 {
		return 0
	}
	return int(duration / time.Second)
}

func copyServiceUlimits(source map[string]config.UlimitConfig) map[string]config.UlimitConfig {
	if len(source) == 0 {
		return nil
	}
	copy := make(map[string]config.UlimitConfig, len(source))
	for name, limit := range source {
		copy[name] = limit
	}
	return copy
}

func (d *Deployer) reconcileServiceViaTakod(client any, request takod.ReconcileServiceRequest) error {
	timeout := reconcileServiceRequestTimeout(request)
	if _, err := takodclient.RequestJSONWithTimeoutContext(d.baseContext(), client, d.takodSocket(), "POST", "/v1/reconcile-service", request, timeout); err != nil {
		return fmt.Errorf("takod service reconciliation failed: %w", err)
	}
	return nil
}

func reconcileServiceRequestTimeout(request takod.ReconcileServiceRequest) time.Duration {
	timeout := takodclient.JSONRequestTimeout
	if len(request.Files) > 0 {
		timeout = takodclient.StreamRequestTimeout
	}
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

func (d *Deployer) removeServiceViaTakod(client any, request takod.RemoveServiceRequest) error {
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
