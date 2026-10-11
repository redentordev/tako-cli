package takod

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Old-image cleanup keeps the newest N images per (repository, environment).
// Image repositories are <project>/<service> and are shared by every
// environment of a project, so environment ownership comes from the node's
// replicated desired revisions and deployment history (plus the legacy
// "<version>-<environment>" tag suffix). An image is only removed when it is
// owned by the target environment alone, is not referenced by any current
// desired or actual state on the node, and is not used by any container.
// Any failure to read that evidence removes nothing.

type dockerImageEntry struct {
	ID         string
	Repository string
	Tag        string
}

func (e dockerImageEntry) ref() string { return e.Repository + ":" + e.Tag }

type imageCleanupEvidence struct {
	// owners maps an image ref to the project environments whose desired
	// revisions or deployment history reference it.
	owners map[string]map[string]bool
	// environments lists every environment of the project known on this node.
	environments map[string]bool
	// protectedRefs are referenced by the current desired or actual state of
	// any project/environment on this node.
	protectedRefs map[string]bool
	// inUseIDs are full image IDs (without the sha256: prefix) used by any
	// container in any state; inUseRefs are the refs those containers name.
	inUseIDs  []string
	inUseRefs map[string]bool
}

type imageRemoval struct {
	ID   string
	Refs []string
}

func cleanupOldProjectImages(ctx context.Context, dataDir string, project string, environment string, keepLatest int, imageRepositories []string) (int, error) {
	if len(imageRepositories) == 0 {
		return 0, nil
	}
	if environment == "" {
		return 0, fmt.Errorf("old image cleanup requires an environment; no images removed")
	}
	output, err := runDocker(ctx, "images", "--filter", "label!="+composeProjectImageLabel, "--format", "{{.ID}}\t{{.Repository}}\t{{.Tag}}")
	if err != nil {
		return 0, fmt.Errorf("failed to list docker images: %w", err)
	}
	images := parseDockerImageList(output)

	evidence, err := readImageCleanupState(dataDir, project)
	if err != nil {
		return 0, fmt.Errorf("old image cleanup skipped, deployment state unreadable: %w", err)
	}
	inUseIDs, inUseRefs, err := containerImageUsage(ctx)
	if err != nil {
		return 0, fmt.Errorf("old image cleanup skipped, container image usage unreadable: %w", err)
	}
	evidence.inUseIDs = inUseIDs
	evidence.inUseRefs = inUseRefs

	removals := planOldImageRemovals(images, evidence, environment, keepLatest, imageRepositories)
	removed := 0
	var failures []string
	for _, removal := range removals {
		// Never force: Docker refuses to delete an image a container still
		// uses, which backs up the in-use check above.
		args := []string{"rmi"}
		if len(removal.Refs) == 1 {
			args = append(args, removal.ID)
		} else {
			args = append(args, removal.Refs...)
		}
		if out, err := runDocker(ctx, args...); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v %s", strings.Join(removal.Refs, ","), err, strings.TrimSpace(out)))
			continue
		}
		removed++
	}
	if len(failures) > 0 {
		return removed, fmt.Errorf("failed to remove %d old project image(s): %s", len(failures), strings.Join(failures, "; "))
	}
	return removed, nil
}

func parseDockerImageList(output string) []dockerImageEntry {
	var images []dockerImageEntry
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) != 3 {
			continue
		}
		entry := dockerImageEntry{ID: strings.TrimPrefix(fields[0], "sha256:"), Repository: fields[1], Tag: fields[2]}
		if entry.ID == "" || entry.Repository == "" || entry.Tag == "" || entry.Repository == "<none>" || entry.Tag == "<none>" {
			continue
		}
		images = append(images, entry)
	}
	return images
}

// planOldImageRemovals selects images to remove. images must be in Docker's
// listing order (newest first).
func planOldImageRemovals(images []dockerImageEntry, evidence imageCleanupEvidence, environment string, keepLatest int, imageRepositories []string) []imageRemoval {
	if keepLatest < 0 {
		keepLatest = 0
	}
	allowed := make(map[string]bool, len(imageRepositories))
	for _, repository := range imageRepositories {
		allowed[repository] = true
	}

	var ids []string
	refsByID := make(map[string][]string)
	for _, image := range images {
		if _, ok := refsByID[image.ID]; !ok {
			ids = append(ids, image.ID)
		}
		refsByID[image.ID] = appendUnique(refsByID[image.ID], image.ref())
	}

	ownedByTarget := func(image dockerImageEntry) bool {
		return allowed[image.Repository] && imageOwnedBy(image, environment, evidence)
	}
	removable := func(image dockerImageEntry) bool {
		if !ownedByTarget(image) || evidence.protectedRefs[image.ref()] || evidence.inUseRefs[image.ref()] {
			return false
		}
		for env := range imageOwners(image, evidence) {
			if env != environment {
				return false
			}
		}
		return true
	}

	// Rank image IDs per repository among images owned by the target
	// environment, then keep the newest N of each.
	seenInRepository := make(map[string]map[string]bool)
	kept := make(map[string]int)
	candidate := make(map[string]bool)
	for _, image := range images {
		if !ownedByTarget(image) {
			continue
		}
		seen := seenInRepository[image.Repository]
		if seen == nil {
			seen = make(map[string]bool)
			seenInRepository[image.Repository] = seen
		}
		if seen[image.ID] {
			continue
		}
		seen[image.ID] = true
		if kept[image.Repository] < keepLatest {
			kept[image.Repository]++
			continue
		}
		candidate[image.ID] = true
	}

	var removals []imageRemoval
	for _, id := range ids {
		if !candidate[id] || imageIDInUse(id, evidence.inUseIDs) {
			continue
		}
		// Every ref on the image must be safe to remove; a ref outside the
		// target environment or repositories keeps the whole image.
		ok := true
		for _, image := range images {
			if image.ID == id && !removable(image) {
				ok = false
				break
			}
		}
		if ok {
			removals = append(removals, imageRemoval{ID: id, Refs: refsByID[id]})
		}
	}
	return removals
}

func imageOwners(image dockerImageEntry, evidence imageCleanupEvidence) map[string]bool {
	owners := make(map[string]bool)
	for env := range evidence.owners[image.ref()] {
		owners[env] = true
	}
	for env := range evidence.environments {
		if legacyTagEnvironmentMatch(image.Tag, env) {
			owners[env] = true
		}
	}
	return owners
}

func imageOwnedBy(image dockerImageEntry, environment string, evidence imageCleanupEvidence) bool {
	return evidence.owners[image.ref()][environment] || legacyTagEnvironmentMatch(image.Tag, environment)
}

// legacyTagEnvironmentMatch reports whether tag uses the legacy
// "<version>-<environment>" form, optionally with a shared-build suffix.
func legacyTagEnvironmentMatch(tag string, environment string) bool {
	if environment == "" {
		return false
	}
	if index := strings.LastIndex(tag, "-sb-"); index > 0 {
		tag = tag[:index]
	}
	return strings.HasSuffix(tag, "-"+environment)
}

func imageIDInUse(id string, inUse []string) bool {
	if id == "" {
		return false
	}
	for _, used := range inUse {
		if strings.HasPrefix(used, id) || strings.HasPrefix(id, used) {
			return true
		}
	}
	return false
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// containerImageUsage returns the image IDs and refs of every container on the
// node, in any state.
func containerImageUsage(ctx context.Context) ([]string, map[string]bool, error) {
	output, err := runDocker(ctx, "ps", "-aq", "--no-trunc")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list containers: %w", err)
	}
	containers := uniqueFields(output)
	refs := make(map[string]bool)
	if len(containers) == 0 {
		return nil, refs, nil
	}
	args := append([]string{"inspect", "--format", "{{.Image}}\t{{.Config.Image}}"}, containers...)
	inspectOutput, err := runDocker(ctx, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to inspect container images: %w", err)
	}
	var ids []string
	for _, line := range strings.Split(inspectOutput, "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) == 0 || fields[0] == "" {
			continue
		}
		ids = append(ids, strings.TrimPrefix(fields[0], "sha256:"))
		if len(fields) > 1 && fields[1] != "" {
			refs[fields[1]] = true
		}
	}
	return ids, refs, nil
}

type imageStateServices struct {
	Services map[string]imageStateService `json:"services"`
}

type imageStateService struct {
	Image          string            `json:"image"`
	RevisionImages map[string]string `json:"revisionImages"`
	Run            *struct {
		Image string `json:"image"`
	} `json:"run"`
}

func (s imageStateServices) images() []string {
	var images []string
	for _, service := range s.Services {
		images = append(images, service.Image)
		for _, image := range service.RevisionImages {
			images = append(images, image)
		}
		if service.Run != nil {
			images = append(images, service.Run.Image)
		}
	}
	return images
}

type imageStateActual struct {
	imageStateServices
	Nodes map[string]imageStateServices `json:"nodes"`
}

type imageStateHistory struct {
	Deployments []*imageStateServices `json:"deployments"`
}

// readImageCleanupState reads ownership and protection evidence from the
// takod data directory. Missing documents are fine; unreadable or corrupt
// ones fail the whole read so cleanup removes nothing.
func readImageCleanupState(dataDir string, project string) (imageCleanupEvidence, error) {
	evidence := imageCleanupEvidence{
		owners:        make(map[string]map[string]bool),
		environments:  make(map[string]bool),
		protectedRefs: make(map[string]bool),
	}
	if dataDir == "" {
		return evidence, fmt.Errorf("takod data directory is unknown")
	}
	info, err := os.Stat(dataDir)
	if err != nil {
		return evidence, fmt.Errorf("takod data directory: %w", err)
	}
	if !info.IsDir() {
		return evidence, fmt.Errorf("takod data directory %s is not a directory", dataDir)
	}

	protect := func(images []string) {
		for _, image := range images {
			if image = strings.TrimSpace(image); image != "" {
				evidence.protectedRefs[image] = true
			}
		}
	}

	// Current desired and actual state of every project/environment.
	desiredCurrent, err := globState(dataDir, "desired", "*", "*", "revision.json")
	if err != nil {
		return evidence, err
	}
	for _, path := range desiredCurrent {
		var doc imageStateServices
		if err := readImageStateJSON(path, &doc); err != nil {
			return evidence, err
		}
		protect(doc.images())
	}
	actualCurrent, err := globState(dataDir, "actual", "*", "*", "containers.json")
	if err != nil {
		return evidence, err
	}
	actualNodes, err := globState(dataDir, "actual", "*", "*", "nodes", "*.json")
	if err != nil {
		return evidence, err
	}
	for _, path := range append(actualCurrent, actualNodes...) {
		var doc imageStateActual
		if err := readImageStateJSON(path, &doc); err != nil {
			return evidence, err
		}
		protect(doc.images())
		for _, node := range doc.Nodes {
			protect(node.images())
		}
	}

	// Ownership: every desired revision and deployment record of the project.
	for _, kind := range []string{"desired", "history", "actual"} {
		entries, err := os.ReadDir(filepath.Join(dataDir, kind, project))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return evidence, fmt.Errorf("failed to read %s state for %s: %w", kind, project, err)
		}
		for _, entry := range entries {
			if entry.IsDir() && isSafeRuntimeName(entry.Name()) {
				evidence.environments[entry.Name()] = true
			}
		}
	}
	envs := make([]string, 0, len(evidence.environments))
	for env := range evidence.environments {
		envs = append(envs, env)
	}
	sort.Strings(envs)
	for _, env := range envs {
		own := func(images []string) {
			for _, image := range images {
				if image = strings.TrimSpace(image); image == "" {
					continue
				}
				if evidence.owners[image] == nil {
					evidence.owners[image] = make(map[string]bool)
				}
				evidence.owners[image][env] = true
			}
		}
		var serviceDocs []string
		for _, pattern := range [][]string{
			{"desired", project, env, "revision.json"},
			{"desired", project, env, "revisions", "*.json"},
			{"history", project, env, "deployments", "*.json"},
		} {
			matches, err := globState(dataDir, pattern...)
			if err != nil {
				return evidence, err
			}
			serviceDocs = append(serviceDocs, matches...)
		}
		for _, path := range serviceDocs {
			var doc imageStateServices
			if err := readImageStateJSON(path, &doc); err != nil {
				return evidence, err
			}
			own(doc.images())
		}
		historyPath := filepath.Join(dataDir, "history", project, env, "history.json")
		var history imageStateHistory
		if err := readImageStateJSON(historyPath, &history); err != nil && !os.IsNotExist(err) {
			return evidence, err
		}
		for _, deployment := range history.Deployments {
			if deployment != nil {
				own(deployment.images())
			}
		}
	}
	return evidence, nil
}

func globState(dataDir string, parts ...string) ([]string, error) {
	pattern := filepath.Join(append([]string{dataDir}, parts...)...)
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to scan state %s: %w", pattern, err)
	}
	sort.Strings(matches)
	return matches, nil
}

func readImageStateJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return err
		}
		return fmt.Errorf("failed to read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("failed to parse %s: %w", path, err)
	}
	return nil
}
