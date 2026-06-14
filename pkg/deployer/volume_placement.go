package deployer

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

type serviceVolumeRef struct {
	LogicalName string
	DockerName  string
	Local       bool
}

func (d *Deployer) applyVolumePlacementGuardrails(serviceName string, service *config.ServiceConfig, targets []string, replicas int, global bool, explicitPlacement bool) ([]string, error) {
	refs, err := d.serviceVolumeRefs(serviceName, service)
	if err != nil {
		return nil, err
	}
	localRefs := localVolumeRefs(refs)
	if len(localRefs) == 0 || replicas <= 0 {
		return targets, nil
	}

	pins, err := d.localVolumePins(localRefs, targets)
	if err != nil {
		return nil, err
	}
	if global {
		return targets, nil
	}
	if !explicitPlacement {
		pinnedNode, ok, err := singleVolumePinnedNode(serviceName, pins, targets)
		if err != nil {
			return nil, err
		}
		if ok {
			return []string{pinnedNode}, nil
		}
	}
	return targets, nil
}

func (d *Deployer) validateVolumeAssignments(serviceName string, service *config.ServiceConfig, assignments []takodAssignment, global bool) error {
	refs, err := d.serviceVolumeRefs(serviceName, service)
	if err != nil {
		return err
	}
	localRefs := localVolumeRefs(refs)
	if len(localRefs) == 0 || len(assignments) == 0 {
		return nil
	}

	assignedNodes := uniqueAssignedServers(assignments)
	if global {
		d.rememberVolumePlacements(localRefs, assignedNodes)
		return nil
	}
	if len(assignedNodes) > 1 {
		return fmt.Errorf("service %s uses local volume(s) %s but would be scheduled on multiple nodes %s; pin placement to one node or mark the top-level volume external: true or replicated: true", serviceName, volumeRefNames(localRefs), strings.Join(assignedNodes, ", "))
	}
	d.rememberVolumePlacements(localRefs, assignedNodes)
	return nil
}

func (d *Deployer) rememberVolumePlacements(refs []serviceVolumeRef, nodes []string) {
	if len(nodes) == 0 {
		return
	}
	if d.volumePlacements == nil {
		d.volumePlacements = make(map[string][]string)
	}
	for _, ref := range refs {
		d.volumePlacements[ref.DockerName] = append([]string(nil), nodes...)
	}
}

func (d *Deployer) serviceVolumeRefs(serviceName string, service *config.ServiceConfig) ([]serviceVolumeRef, error) {
	refs := make([]serviceVolumeRef, 0, len(service.Volumes))
	for _, volume := range service.Volumes {
		if config.IsNFSVolume(volume) {
			return nil, fmt.Errorf("service %s: NFS volumes are no longer supported; use node-local volumes or an external storage service", serviceName)
		}
		spec, err := config.ParseVolumeMountSpec(volume)
		if err != nil {
			return nil, fmt.Errorf("service %s: %w", serviceName, err)
		}
		if spec.HasTarget && strings.HasPrefix(spec.Source, "/") {
			continue
		}

		logicalName := spec.Source
		dockerName := d.dockerVolumeName(logicalName)
		refs = append(refs, serviceVolumeRef{
			LogicalName: logicalName,
			DockerName:  dockerName,
			Local:       d.isLocalVolume(logicalName),
		})
	}
	return refs, nil
}

func (d *Deployer) dockerVolumeName(logicalName string) string {
	if !strings.HasPrefix(logicalName, "/") {
		return d.config.GetVolumeName(logicalName, d.environment)
	}
	return runtimeid.VolumeName(d.config.Project.Name, d.environment, logicalName)
}

func (d *Deployer) isLocalVolume(logicalName string) bool {
	if strings.HasPrefix(logicalName, "/") {
		return true
	}
	volume, ok := d.config.GetVolume(logicalName)
	if !ok {
		return true
	}
	return !volume.External && !volume.Replicated
}

func localVolumeRefs(refs []serviceVolumeRef) []serviceVolumeRef {
	out := make([]serviceVolumeRef, 0, len(refs))
	seen := make(map[string]bool)
	for _, ref := range refs {
		if !ref.Local || seen[ref.DockerName] {
			continue
		}
		seen[ref.DockerName] = true
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].DockerName < out[j].DockerName
	})
	return out
}

func (d *Deployer) localVolumePins(refs []serviceVolumeRef, targets []string) (map[string][]string, error) {
	volumeNames := volumeRefDockerNames(refs)
	if len(volumeNames) == 0 {
		return nil, nil
	}
	pins := make(map[string][]string, len(volumeNames))
	for _, volumeName := range volumeNames {
		if remembered := intersectNodes(d.volumePlacements[volumeName], targets); len(remembered) > 0 {
			pins[volumeName] = remembered
		}
	}

	for _, serverName := range targets {
		found, err := d.inspectNodeVolumes(serverName, volumeNames)
		if err != nil {
			return nil, err
		}
		for _, volumeName := range volumeNames {
			if found[volumeName] {
				pins[volumeName] = appendUniqueSorted(pins[volumeName], serverName)
			}
		}
	}
	return pins, nil
}

func (d *Deployer) inspectNodeVolumes(serverName string, volumeNames []string) (map[string]bool, error) {
	if d.volumeInspector != nil {
		return d.volumeInspector(serverName, volumeNames)
	}
	client, err := d.getEnvironmentClient(serverName)
	if err != nil {
		return nil, err
	}
	output, err := takodclient.RequestJSON(client, d.takodSocket(), "POST", "/v1/volumes/inspect", takod.VolumeInspectRequest{
		Project:     d.config.Project.Name,
		Environment: d.environment,
		Volumes:     volumeNames,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to inspect volumes on %s: %w", serverName, err)
	}
	var response takod.VolumeInspectResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("failed to parse volume inspection response from %s: %w", serverName, err)
	}
	if response.Project != d.config.Project.Name || response.Environment != d.environment {
		return nil, fmt.Errorf("volume inspection response from %s has project/environment mismatch", serverName)
	}
	return response.Volumes, nil
}

func singleVolumePinnedNode(serviceName string, pins map[string][]string, targets []string) (string, bool, error) {
	required := ""
	for volumeName, nodes := range pins {
		nodes = uniqueSortedStrings(nodes)
		if len(nodes) == 0 {
			continue
		}
		if len(nodes) > 1 {
			return "", false, fmt.Errorf("service %s local volume %s already exists on multiple eligible nodes %s; pin placement to one node or mark the top-level volume external: true or replicated: true", serviceName, volumeName, strings.Join(nodes, ", "))
		}
		node := nodes[0]
		if !containsString(targets, node) {
			return "", false, fmt.Errorf("service %s local volume %s exists on node %s, outside eligible nodes %s", serviceName, volumeName, node, strings.Join(targets, ", "))
		}
		if required != "" && required != node {
			return "", false, fmt.Errorf("service %s local volumes require different nodes (%s and %s); use explicit placement or mark volumes external: true or replicated: true", serviceName, required, node)
		}
		required = node
	}
	if required == "" {
		return "", false, nil
	}
	return required, true, nil
}

func volumeRefDockerNames(refs []serviceVolumeRef) []string {
	names := make([]string, 0, len(refs))
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if seen[ref.DockerName] {
			continue
		}
		seen[ref.DockerName] = true
		names = append(names, ref.DockerName)
	}
	sort.Strings(names)
	return names
}

func volumeRefNames(refs []serviceVolumeRef) string {
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.LogicalName)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func uniqueAssignedServers(assignments []takodAssignment) []string {
	nodes := make([]string, 0, len(assignments))
	seen := make(map[string]bool, len(assignments))
	for _, assignment := range assignments {
		if seen[assignment.ServerName] {
			continue
		}
		seen[assignment.ServerName] = true
		nodes = append(nodes, assignment.ServerName)
	}
	sort.Strings(nodes)
	return nodes
}

func intersectNodes(nodes []string, targets []string) []string {
	targetSet := make(map[string]bool, len(targets))
	for _, target := range targets {
		targetSet[target] = true
	}
	out := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if targetSet[node] {
			out = append(out, node)
		}
	}
	return uniqueSortedStrings(out)
}

func appendUniqueSorted(values []string, value string) []string {
	if containsString(values, value) {
		return values
	}
	out := append(append([]string(nil), values...), value)
	sort.Strings(out)
	return out
}

func uniqueSortedStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func containsString(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}
