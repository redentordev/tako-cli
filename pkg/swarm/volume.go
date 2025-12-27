package swarm

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

// validNamePattern matches safe Docker volume/label names
// Only allows alphanumeric, underscore, hyphen, and dot
var validNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// shellQuote safely quotes a string for shell usage
func shellQuote(s string) string {
	// If the string is safe (alphanumeric with allowed chars), return as-is
	if validNamePattern.MatchString(s) {
		return s
	}
	// Otherwise, wrap in single quotes and escape any single quotes within
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// validateVolumeName checks if a volume name is safe for use
func validateVolumeName(name string) error {
	if name == "" {
		return fmt.Errorf("volume name cannot be empty")
	}
	if len(name) > 255 {
		return fmt.Errorf("volume name too long (max 255 characters)")
	}
	// Docker volume names: [a-zA-Z0-9][a-zA-Z0-9_.-]*
	if !validNamePattern.MatchString(name) {
		return fmt.Errorf("volume name contains invalid characters: %s", name)
	}
	return nil
}

// VolumeMount represents a volume mount specification
type VolumeMount struct {
	Type     string // "volume", "bind", "nfs"
	Source   string // Volume name or host path
	Target   string // Container path
	ReadOnly bool
}

// getServiceMounts retrieves current volume mounts from a running service
func (m *Manager) getServiceMounts(client *ssh.Client, fullServiceName string) (map[string]VolumeMount, error) {
	// Get mounts from service spec using docker inspect
	cmd := fmt.Sprintf("docker service inspect %s --format '{{json .Spec.TaskTemplate.ContainerSpec.Mounts}}'", fullServiceName)
	output, err := client.Execute(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect service mounts: %w", err)
	}

	output = strings.TrimSpace(output)
	if output == "" || output == "null" || output == "[]" {
		return make(map[string]VolumeMount), nil
	}

	// Parse JSON array of mounts
	var mounts []struct {
		Type     string `json:"Type"`
		Source   string `json:"Source"`
		Target   string `json:"Target"`
		ReadOnly bool   `json:"ReadOnly"`
	}
	if err := json.Unmarshal([]byte(output), &mounts); err != nil {
		return nil, fmt.Errorf("failed to parse mounts: %w", err)
	}

	// Convert to map keyed by target (container path)
	mountMap := make(map[string]VolumeMount)
	for _, mount := range mounts {
		mountMap[mount.Target] = VolumeMount{
			Type:     mount.Type,
			Source:   mount.Source,
			Target:   mount.Target,
			ReadOnly: mount.ReadOnly,
		}
	}

	return mountMap, nil
}

// buildDesiredMounts builds the desired mount specifications from service config
func (m *Manager) buildDesiredMounts(serviceName string, service *config.ServiceConfig) (map[string]VolumeMount, error) {
	mounts := make(map[string]VolumeMount)

	for _, volume := range service.Volumes {
		// Check if this is an NFS volume
		if config.IsNFSVolume(volume) {
			exportName, containerPath, readOnly, err := config.ParseNFSVolumeSpec(volume)
			if err != nil {
				return nil, fmt.Errorf("invalid NFS volume spec: %w", err)
			}

			var mountSource string
			isMultiServer := m.config.IsMultiServer()

			if isMultiServer {
				mountSource = fmt.Sprintf("/mnt/tako-nfs/%s_%s_%s", m.config.Project.Name, m.environment, exportName)
			} else {
				export, err := m.config.GetNFSExport(exportName)
				if err != nil {
					return nil, fmt.Errorf("NFS export '%s' not found: %w", exportName, err)
				}
				mountSource = export.Path
			}

			mounts[containerPath] = VolumeMount{
				Type:     "bind",
				Source:   mountSource,
				Target:   containerPath,
				ReadOnly: readOnly,
			}
			continue
		}

		source, target := m.parseVolumeSpec(volume)
		if target == "" {
			// Only target specified - create named volume
			target = source
			source = fmt.Sprintf("%s_%s_%s", m.config.Project.Name, m.environment, strings.ReplaceAll(target, "/", "_"))
			mounts[target] = VolumeMount{
				Type:   "volume",
				Source: source,
				Target: target,
			}
		} else {
			// Both source and target specified
			if strings.HasPrefix(source, "/") || strings.HasPrefix(source, "\\") || (len(source) > 1 && source[1] == ':') {
				// Absolute path = bind mount
				mounts[target] = VolumeMount{
					Type:   "bind",
					Source: source,
					Target: target,
				}
			} else {
				// Named volume - check if defined in top-level volumes section
				volumeName := m.config.GetVolumeName(source, m.environment)
				mounts[target] = VolumeMount{
					Type:   "volume",
					Source: volumeName,
					Target: target,
				}
			}
		}
	}

	return mounts, nil
}

// CreateDefinedVolumes creates all volumes defined in the top-level volumes section
func (m *Manager) CreateDefinedVolumes(client *ssh.Client) error {
	definedVolumes := m.config.GetAllDefinedVolumes()
	if len(definedVolumes) == 0 {
		return nil
	}

	if m.verbose {
		fmt.Printf("→ Creating defined volumes...\n")
	}

	for volumeKey, volumeCfg := range definedVolumes {
		// Skip external volumes - they should already exist
		if volumeCfg.External {
			volumeName := volumeKey
			if volumeCfg.Name != "" {
				volumeName = volumeCfg.Name
			}
			exists, err := m.VolumeExists(client, volumeName)
			if err != nil {
				return fmt.Errorf("failed to check external volume '%s': %w", volumeName, err)
			}
			if !exists {
				return fmt.Errorf("external volume '%s' does not exist", volumeName)
			}
			if m.verbose {
				fmt.Printf("  ✓ External volume exists: %s\n", volumeName)
			}
			continue
		}

		// Get the actual volume name (with or without prefix)
		volumeName := m.config.GetVolumeName(volumeKey, m.environment)

		// Check if volume already exists
		exists, err := m.VolumeExists(client, volumeName)
		if err != nil {
			return fmt.Errorf("failed to check volume '%s': %w", volumeName, err)
		}

		if exists {
			if m.verbose {
				fmt.Printf("  Volume already exists: %s\n", volumeName)
			}
			continue
		}

		// Validate volume name before use
		if err := validateVolumeName(volumeName); err != nil {
			return fmt.Errorf("invalid volume name '%s': %w", volumeName, err)
		}

		// Build create command with options
		cmd := fmt.Sprintf("docker volume create %s", shellQuote(volumeName))

		// Add driver
		driver := volumeCfg.Driver
		if driver == "" {
			driver = "local"
		}
		if driver != "local" {
			cmd += fmt.Sprintf(" --driver %s", shellQuote(driver))
		}

		// Add driver options
		for key, value := range volumeCfg.DriverOpts {
			cmd += fmt.Sprintf(" --opt %s=%s", shellQuote(key), shellQuote(value))
		}

		// Add labels
		labels := make(map[string]string)
		labels["tako.project"] = m.config.Project.Name
		labels["tako.environment"] = m.environment
		labels["tako.volume"] = volumeKey
		for key, value := range volumeCfg.Labels {
			labels[key] = value
		}
		for key, value := range labels {
			cmd += fmt.Sprintf(" --label %s=%s", shellQuote(key), shellQuote(value))
		}

		if m.verbose {
			fmt.Printf("  Creating volume: %s\n", volumeName)
		}

		output, err := client.Execute(cmd)
		if err != nil {
			return fmt.Errorf("failed to create volume '%s': %w, output: %s", volumeName, err, output)
		}

		if m.verbose {
			fmt.Printf("  ✓ Created volume: %s\n", volumeName)
		}
	}

	return nil
}

// buildMountSpec builds a mount specification string for docker service update
func buildMountSpec(mount VolumeMount) string {
	spec := fmt.Sprintf("type=%s,source=%s,target=%s", mount.Type, mount.Source, mount.Target)
	if mount.ReadOnly {
		spec += ",readonly"
	}
	return spec
}

// computeMountChanges computes the mount add/remove commands needed
func (m *Manager) computeMountChanges(current, desired map[string]VolumeMount) (adds []string, removes []string) {
	// Find mounts to remove (in current but not in desired, or changed)
	for target, currentMount := range current {
		desiredMount, exists := desired[target]
		if !exists {
			// Mount no longer needed
			removes = append(removes, target)
		} else if currentMount.Source != desiredMount.Source ||
			currentMount.Type != desiredMount.Type ||
			currentMount.ReadOnly != desiredMount.ReadOnly {
			// Mount changed - need to remove and re-add
			removes = append(removes, target)
			adds = append(adds, buildMountSpec(desiredMount))
		}
	}

	// Find mounts to add (in desired but not in current)
	for target, desiredMount := range desired {
		if _, exists := current[target]; !exists {
			adds = append(adds, buildMountSpec(desiredMount))
		}
	}

	return adds, removes
}

// CreateVolume creates a Docker volume with the given name and options
func (m *Manager) CreateVolume(client *ssh.Client, name string, driver string, labels map[string]string) error {
	// Validate volume name
	if err := validateVolumeName(name); err != nil {
		return fmt.Errorf("invalid volume name: %w", err)
	}

	cmd := fmt.Sprintf("docker volume create %s", shellQuote(name))

	if driver != "" && driver != "local" {
		cmd += fmt.Sprintf(" --driver %s", shellQuote(driver))
	}

	for key, value := range labels {
		cmd += fmt.Sprintf(" --label %s=%s", shellQuote(key), shellQuote(value))
	}

	if m.verbose {
		fmt.Printf("  Creating volume: %s\n", name)
	}

	output, err := client.Execute(cmd)
	if err != nil {
		return fmt.Errorf("failed to create volume: %w, output: %s", err, output)
	}

	return nil
}

// VolumeExists checks if a Docker volume exists
func (m *Manager) VolumeExists(client *ssh.Client, name string) (bool, error) {
	// Use exact match filter with proper escaping
	cmd := fmt.Sprintf("docker volume ls --filter name=^%s$ --format '{{.Name}}'", shellQuote(name))
	output, err := client.Execute(cmd)
	if err != nil {
		return false, fmt.Errorf("failed to list volumes: %w", err)
	}

	return strings.TrimSpace(output) == name, nil
}

// RemoveVolume removes a Docker volume
func (m *Manager) RemoveVolume(client *ssh.Client, name string) error {
	cmd := fmt.Sprintf("docker volume rm %s", shellQuote(name))

	if m.verbose {
		fmt.Printf("  Removing volume: %s\n", name)
	}

	output, err := client.Execute(cmd)
	if err != nil {
		return fmt.Errorf("failed to remove volume: %w, output: %s", err, output)
	}

	return nil
}

// ListProjectVolumes lists all volumes for a project/environment
func (m *Manager) ListProjectVolumes(client *ssh.Client) ([]string, error) {
	prefix := fmt.Sprintf("%s_%s_", m.config.Project.Name, m.environment)
	cmd := fmt.Sprintf("docker volume ls --filter name=%s --format '{{.Name}}'", shellQuote(prefix))

	output, err := client.Execute(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to list volumes: %w", err)
	}

	if strings.TrimSpace(output) == "" {
		return []string{}, nil
	}

	return strings.Split(strings.TrimSpace(output), "\n"), nil
}

// EnsureVolumesExist creates all required volumes for a service if they don't exist
func (m *Manager) EnsureVolumesExist(client *ssh.Client, serviceName string, service *config.ServiceConfig) error {
	desiredMounts, err := m.buildDesiredMounts(serviceName, service)
	if err != nil {
		return err
	}

	for _, mount := range desiredMounts {
		if mount.Type != "volume" {
			// Skip bind mounts - they use host paths
			continue
		}

		exists, err := m.VolumeExists(client, mount.Source)
		if err != nil {
			return err
		}

		if !exists {
			labels := map[string]string{
				"tako.project":     m.config.Project.Name,
				"tako.environment": m.environment,
				"tako.service":     serviceName,
			}
			if err := m.CreateVolume(client, mount.Source, "local", labels); err != nil {
				return err
			}
			if m.verbose {
				fmt.Printf("  ✓ Created volume: %s\n", mount.Source)
			}
		}
	}

	return nil
}
