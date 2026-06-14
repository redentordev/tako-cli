package deployer

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

func (d *Deployer) ValidateServicePlatformCompatibility(serviceName string, service *config.ServiceConfig) error {
	if service == nil {
		return fmt.Errorf("service %s config is nil", serviceName)
	}
	if strings.TrimSpace(service.Platform) == "" && strings.TrimSpace(service.Build) == "" {
		return nil
	}

	assignments, err := d.planTakodAssignments(serviceName, service)
	if err != nil {
		return err
	}
	assignedNodes := uniqueAssignmentServers(assignments)
	if len(assignedNodes) == 0 {
		return nil
	}

	targetServers, err := d.getTakodTargetServers()
	if err != nil {
		return fmt.Errorf("failed to get takod target servers: %w", err)
	}

	nodesToInspect := append([]string(nil), assignedNodes...)
	sourceNode := ""
	if len(targetServers) > 0 {
		sourceNode = targetServers[0]
		if strings.TrimSpace(service.Build) != "" {
			nodesToInspect = append(nodesToInspect, sourceNode)
		}
	}
	infos, err := d.inspectNodeInfo(uniqueSortedStrings(nodesToInspect))
	if err != nil {
		return err
	}

	platform := strings.TrimSpace(service.Platform)
	if platform != "" {
		var mismatches []string
		for _, node := range assignedNodes {
			nodePlatform := strings.TrimSpace(infos[node].Platform)
			if nodePlatform == "" {
				return fmt.Errorf("service %s platform %s cannot be verified because node %s did not report a Docker platform", serviceName, platform, node)
			}
			if nodePlatform != platform {
				mismatches = append(mismatches, fmt.Sprintf("%s=%s", node, nodePlatform))
			}
		}
		if len(mismatches) > 0 {
			return fmt.Errorf("service %s platform %s does not match assigned node platform(s): %s; use a matching platform or pin placement to compatible nodes", serviceName, platform, strings.Join(mismatches, ", "))
		}
		return nil
	}

	// A build without an explicit platform produces a single image on the source
	// node. Refuse mixed or source-mismatched targets until per-platform builds
	// or multi-arch manifests are available.
	assignedPlatforms := make(map[string][]string)
	for _, node := range assignedNodes {
		nodePlatform := strings.TrimSpace(infos[node].Platform)
		if nodePlatform == "" {
			return fmt.Errorf("service %s build platform cannot be inferred because node %s did not report a Docker platform", serviceName, node)
		}
		assignedPlatforms[nodePlatform] = append(assignedPlatforms[nodePlatform], node)
	}
	if len(assignedPlatforms) > 1 {
		return fmt.Errorf("service %s builds one image but assigned nodes have mixed platforms: %s; set service.platform and pin placement to compatible nodes", serviceName, formatPlatformNodes(assignedPlatforms))
	}

	for platform := range assignedPlatforms {
		if sourceNode != "" && infos[sourceNode] != nil {
			sourcePlatform := strings.TrimSpace(infos[sourceNode].Platform)
			if sourcePlatform != "" && sourcePlatform != platform {
				return fmt.Errorf("service %s builds on source node %s=%s but assigned nodes require %s; set service.platform or make the source node compatible with placement", serviceName, sourceNode, sourcePlatform, platform)
			}
		}
	}
	return nil
}

func (d *Deployer) inspectNodeInfo(serverNames []string) (map[string]*takod.NodeInfoResponse, error) {
	result := make(map[string]*takod.NodeInfoResponse, len(serverNames))
	var errors []string
	for _, serverName := range serverNames {
		info, err := d.inspectSingleNodeInfo(serverName)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", serverName, err))
			continue
		}
		if info == nil {
			errors = append(errors, fmt.Sprintf("%s: empty node info response", serverName))
			continue
		}
		result[serverName] = info
	}
	if len(errors) > 0 {
		sort.Strings(errors)
		return nil, fmt.Errorf("failed to inspect node platform(s): %s", strings.Join(errors, "; "))
	}
	return result, nil
}

func (d *Deployer) inspectSingleNodeInfo(serverName string) (*takod.NodeInfoResponse, error) {
	if d.nodeInfoInspector != nil {
		return d.nodeInfoInspector(serverName)
	}
	client, err := d.getEnvironmentClient(serverName)
	if err != nil {
		return nil, err
	}
	output, err := takodclient.RequestJSON(client, d.takodSocket(), "GET", takodclient.NodeInfoEndpoint(), nil)
	if err != nil {
		return nil, err
	}
	var response takod.NodeInfoResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("failed to parse node info response from %s: %w", serverName, err)
	}
	return &response, nil
}

func formatPlatformNodes(platforms map[string][]string) string {
	keys := make([]string, 0, len(platforms))
	for platform := range platforms {
		keys = append(keys, platform)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, platform := range keys {
		nodes := append([]string(nil), platforms[platform]...)
		sort.Strings(nodes)
		parts = append(parts, fmt.Sprintf("%s=[%s]", platform, strings.Join(nodes, ", ")))
	}
	return strings.Join(parts, ", ")
}
