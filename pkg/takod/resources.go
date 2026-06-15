package takod

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

const maxResourceRemoveItems = 128

type ImageListRequest struct {
	Project     string `json:"project"`
	Environment string `json:"environment,omitempty"`
}

type ImageSummary struct {
	ID           string `json:"id"`
	Repository   string `json:"repository"`
	Tag          string `json:"tag"`
	Reference    string `json:"reference"`
	Size         string `json:"size,omitempty"`
	CreatedSince string `json:"createdSince,omitempty"`
}

type ImageListResponse struct {
	Project     string         `json:"project"`
	Environment string         `json:"environment,omitempty"`
	Images      []ImageSummary `json:"images"`
}

type ImageRemoveRequest struct {
	Project     string   `json:"project"`
	Environment string   `json:"environment,omitempty"`
	References  []string `json:"references"`
	Force       bool     `json:"force,omitempty"`
}

type ImageRemoveResponse struct {
	Project     string   `json:"project"`
	Environment string   `json:"environment,omitempty"`
	Removed     []string `json:"removed"`
}

type ImagePruneRequest struct {
	Project     string `json:"project"`
	Environment string `json:"environment,omitempty"`
	Force       bool   `json:"force,omitempty"`
}

type ImagePruneResponse struct {
	Project     string   `json:"project"`
	Environment string   `json:"environment,omitempty"`
	Removed     []string `json:"removed"`
	Skipped     []string `json:"skipped,omitempty"`
}

type VolumeListRequest struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
}

type VolumeSummary struct {
	Name   string `json:"name"`
	Driver string `json:"driver,omitempty"`
	Scope  string `json:"scope,omitempty"`
}

type VolumeListResponse struct {
	Project     string          `json:"project"`
	Environment string          `json:"environment"`
	Volumes     []VolumeSummary `json:"volumes"`
}

type VolumeRemoveRequest struct {
	Project     string   `json:"project"`
	Environment string   `json:"environment"`
	Names       []string `json:"names"`
	Force       bool     `json:"force,omitempty"`
}

type VolumeRemoveResponse struct {
	Project     string   `json:"project"`
	Environment string   `json:"environment"`
	Removed     []string `json:"removed"`
	Skipped     []string `json:"skipped,omitempty"`
}

func ListImages(ctx context.Context, req ImageListRequest) (*ImageListResponse, error) {
	if err := validateImageListRequest(req); err != nil {
		return nil, err
	}
	output, err := runDocker(ctx, "images", "--format", "{{.ID}}\t{{.Repository}}\t{{.Tag}}\t{{.Size}}\t{{.CreatedSince}}")
	if err != nil {
		return nil, fmt.Errorf("failed to list docker images: %w", err)
	}
	var images []ImageSummary
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) < 3 {
			continue
		}
		id, repo, tag := fields[0], fields[1], fields[2]
		if id == "" || repo == "<none>" || tag == "<none>" || !imageRepositoryMatchesProject(repo, req.Project, nil) {
			continue
		}
		image := ImageSummary{
			ID:         id,
			Repository: repo,
			Tag:        tag,
			Reference:  repo + ":" + tag,
		}
		if len(fields) > 3 {
			image.Size = fields[3]
		}
		if len(fields) > 4 {
			image.CreatedSince = fields[4]
		}
		images = append(images, image)
	}
	sort.SliceStable(images, func(i, j int) bool {
		return images[i].Reference < images[j].Reference
	})
	return &ImageListResponse{Project: req.Project, Environment: req.Environment, Images: images}, nil
}

func RemoveImages(ctx context.Context, req ImageRemoveRequest) (*ImageRemoveResponse, error) {
	if err := validateImageRemoveRequest(req); err != nil {
		return nil, err
	}
	args := []string{"rmi"}
	if req.Force {
		args = append(args, "-f")
	}
	args = append(args, req.References...)
	if _, err := runDocker(ctx, args...); err != nil {
		return nil, fmt.Errorf("failed to remove docker images: %w", err)
	}
	return &ImageRemoveResponse{Project: req.Project, Environment: req.Environment, Removed: append([]string(nil), req.References...)}, nil
}

func PruneImages(ctx context.Context, req ImagePruneRequest) (*ImagePruneResponse, error) {
	if err := validateImagePruneRequest(req); err != nil {
		return nil, err
	}
	images, err := ListImages(ctx, ImageListRequest{Project: req.Project, Environment: req.Environment})
	if err != nil {
		return nil, err
	}
	usedImages, err := projectContainerImages(ctx, req.Project, req.Environment)
	if err != nil {
		return nil, err
	}

	response := &ImagePruneResponse{Project: req.Project, Environment: req.Environment}
	for _, image := range images.Images {
		if imageInUseByProjectContainer(image, usedImages) {
			response.Skipped = append(response.Skipped, image.Reference)
			continue
		}
		output, err := runDocker(ctx, "rmi", image.Reference)
		if err != nil {
			if dockerImageInUse(output) {
				response.Skipped = append(response.Skipped, image.Reference)
				continue
			}
			return response, fmt.Errorf("failed to remove docker image %s: %w", image.Reference, err)
		}
		response.Removed = append(response.Removed, image.Reference)
	}
	return response, nil
}

func ListVolumes(ctx context.Context, req VolumeListRequest) (*VolumeListResponse, error) {
	if err := validateVolumeListRequest(req); err != nil {
		return nil, err
	}
	output, err := runDocker(
		ctx,
		"volume", "ls",
		"--filter", "label=tako.project="+req.Project,
		"--filter", "label=tako.environment="+req.Environment,
		"--format", "{{.Name}}\t{{.Driver}}\t{{.Scope}}",
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list docker volumes: %w", err)
	}
	var volumes []VolumeSummary
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) == 0 || fields[0] == "" {
			continue
		}
		volume := VolumeSummary{Name: fields[0]}
		if len(fields) > 1 {
			volume.Driver = fields[1]
		}
		if len(fields) > 2 {
			volume.Scope = fields[2]
		}
		volumes = append(volumes, volume)
	}
	sort.SliceStable(volumes, func(i, j int) bool {
		return volumes[i].Name < volumes[j].Name
	})
	return &VolumeListResponse{Project: req.Project, Environment: req.Environment, Volumes: volumes}, nil
}

func RemoveVolumes(ctx context.Context, req VolumeRemoveRequest) (*VolumeRemoveResponse, error) {
	if err := validateVolumeRemoveRequest(req); err != nil {
		return nil, err
	}
	response := &VolumeRemoveResponse{Project: req.Project, Environment: req.Environment}
	for _, name := range req.Names {
		owned, err := volumeBelongsToProjectEnvironment(ctx, name, req.Project, req.Environment)
		if err != nil {
			return response, err
		}
		if !owned {
			return response, fmt.Errorf("volume %s is not owned by %s/%s", name, req.Project, req.Environment)
		}
		args := []string{"volume", "rm"}
		if req.Force {
			args = append(args, "-f")
		}
		args = append(args, name)
		output, err := runDocker(ctx, args...)
		if err != nil {
			if dockerVolumeInUse(output) {
				response.Skipped = append(response.Skipped, name)
				continue
			}
			return response, fmt.Errorf("failed to remove docker volume %s: %w", name, err)
		}
		response.Removed = append(response.Removed, name)
	}
	return response, nil
}

func validateImageListRequest(req ImageListRequest) error {
	if !isSafeProjectName(req.Project) {
		return fmt.Errorf("invalid project name")
	}
	if req.Environment != "" && !isSafeRuntimeName(req.Environment) {
		return fmt.Errorf("invalid environment name")
	}
	return nil
}

func validateImageRemoveRequest(req ImageRemoveRequest) error {
	if err := validateImageListRequest(ImageListRequest{Project: req.Project, Environment: req.Environment}); err != nil {
		return err
	}
	if len(req.References) == 0 {
		return fmt.Errorf("references are required")
	}
	if len(req.References) > maxResourceRemoveItems {
		return fmt.Errorf("too many image references")
	}
	seen := make(map[string]bool, len(req.References))
	for _, ref := range req.References {
		if err := validateImageName(ref); err != nil {
			return err
		}
		if seen[ref] {
			return fmt.Errorf("duplicate image reference %s", ref)
		}
		seen[ref] = true
		repo := imageRepositoryFromReference(ref)
		if !imageRepositoryMatchesProject(repo, req.Project, nil) {
			return fmt.Errorf("image %s is not owned by project %s", ref, req.Project)
		}
	}
	return nil
}

func validateImagePruneRequest(req ImagePruneRequest) error {
	if err := validateImageListRequest(ImageListRequest{Project: req.Project, Environment: req.Environment}); err != nil {
		return err
	}
	if !req.Force {
		return fmt.Errorf("force is required to prune images")
	}
	return nil
}

func validateVolumeListRequest(req VolumeListRequest) error {
	if !isSafeProjectName(req.Project) {
		return fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(req.Environment) {
		return fmt.Errorf("invalid environment name")
	}
	return nil
}

func validateVolumeRemoveRequest(req VolumeRemoveRequest) error {
	if err := validateVolumeListRequest(VolumeListRequest{Project: req.Project, Environment: req.Environment}); err != nil {
		return err
	}
	if len(req.Names) == 0 {
		return fmt.Errorf("volume names are required")
	}
	if len(req.Names) > maxResourceRemoveItems {
		return fmt.Errorf("too many volumes")
	}
	seen := make(map[string]bool, len(req.Names))
	for _, name := range req.Names {
		if !isSafeDockerVolumeName(name) {
			return fmt.Errorf("invalid volume name")
		}
		if seen[name] {
			return fmt.Errorf("duplicate volume name %s", name)
		}
		seen[name] = true
	}
	return nil
}

func imageRepositoryFromReference(ref string) string {
	if repo, _, ok := strings.Cut(ref, "@"); ok {
		return repo
	}
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon > slash {
		return ref[:colon]
	}
	return ref
}

func projectContainerImages(ctx context.Context, project string, environment string) (map[string]bool, error) {
	args := []string{
		"ps", "-a",
		"--filter", "label=tako.project=" + project,
	}
	if environment != "" {
		args = append(args, "--filter", "label=tako.environment="+environment)
	}
	args = append(args, "--format", "{{.Names}}")
	output, err := runDocker(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list project containers: %w", err)
	}
	names := uniqueFields(output)
	used := make(map[string]bool)
	if len(names) == 0 {
		return used, nil
	}

	inspectArgs := append([]string{"inspect", "--format", "{{.Config.Image}}\t{{.Image}}"}, names...)
	inspectOutput, err := runDocker(ctx, inspectArgs...)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect project container images: %w", err)
	}
	for _, line := range strings.Split(inspectOutput, "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		for _, field := range fields {
			field = strings.TrimSpace(field)
			if field != "" && field != "<no value>" {
				used[field] = true
			}
		}
	}
	return used, nil
}

func imageInUseByProjectContainer(image ImageSummary, used map[string]bool) bool {
	if used[image.Reference] || used[image.ID] || used["sha256:"+image.ID] {
		return true
	}
	return false
}

func dockerImageInUse(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "image is being used") ||
		strings.Contains(lower, "is being used by") ||
		strings.Contains(lower, "conflict")
}

func volumeBelongsToProjectEnvironment(ctx context.Context, name string, project string, environment string) (bool, error) {
	if volumeNameMatchesProjectEnvironment(name, project, environment) {
		return true, nil
	}
	output, err := runDocker(ctx, "volume", "inspect", "--format", "{{ index .Labels \"tako.project\" }}\t{{ index .Labels \"tako.environment\" }}", name)
	if err != nil {
		return false, fmt.Errorf("failed to inspect docker volume %s: %w", name, err)
	}
	fields := strings.Split(strings.TrimSpace(output), "\t")
	if len(fields) < 2 {
		return false, nil
	}
	return fields[0] == project && fields[1] == environment, nil
}
