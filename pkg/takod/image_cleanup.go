package takod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
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
	// Created and Size come from `docker image inspect` of the selected
	// candidates only; they are zero when unknown.
	Created time.Time
	Size    int64
}

func (e dockerImageEntry) ref() string { return e.Repository + ":" + e.Tag }

// Kept-image reasons. They explain a decision; they never change one.
const (
	ImageKeptInUse            = "in use"
	ImageKeptCurrent          = "current"
	ImageKeptOtherEnvironment = "other environment"
	ImageKeptRetention        = "retention"
	ImageKeptRecent           = "recent"
)

// CleanupHygiene is the additive image report of a scoped cleanup, a dry run
// or an image GC pass: what was (or would be) removed, why the rest of the
// target environment's images were kept, and the bytes freed.
type CleanupHygiene struct {
	Removed    []HygieneImage `json:"removed,omitempty"`
	Kept       map[string]int `json:"kept,omitempty"`
	BytesFreed int64          `json:"bytesFreed"`
	DryRun     bool           `json:"dryRun,omitempty"`
	// Failed lists removals Docker refused; those images were kept.
	Failed []string `json:"failed,omitempty"`
}

// HygieneImage is one removed (or, in a dry run, removable) image.
type HygieneImage struct {
	ID        string   `json:"id"`
	Refs      []string `json:"refs"`
	SizeBytes int64    `json:"sizeBytes,omitempty"`
}

// imageCleanupOptions are the additions unattended callers need. The zero
// value is exactly v0.14.2's scoped cleanup.
type imageCleanupOptions struct {
	// CreatedAfter keeps images created after this instant (reason
	// "recent"); an image whose creation time is unknown is kept too.
	CreatedAfter time.Time
	// DryRun selects and reports without removing anything.
	DryRun bool
}

type imageCleanupPlan struct {
	Removals []imageRemoval
	// Kept maps an image ID owned by the target environment, in an allowed
	// repository, to the reason it is kept.
	Kept map[string]string
}

func (p imageCleanupPlan) keptCounts() map[string]int {
	if len(p.Kept) == 0 {
		return nil
	}
	counts := make(map[string]int)
	for _, reason := range p.Kept {
		counts[reason]++
	}
	return counts
}

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
	Size int64
}

// errImageCleanupStateUnreadable marks a selection that removed nothing
// because node state or the Docker inventory could not be read.
var errImageCleanupStateUnreadable = errors.New("image cleanup state unreadable")

func cleanupOldProjectImages(ctx context.Context, dataDir string, project string, environment string, keepLatest int, imageRepositories []string) (int, error) {
	report, err := cleanupOldProjectImagesWithOptions(ctx, dataDir, project, environment, keepLatest, imageRepositories, imageCleanupOptions{})
	return len(report.Removed), err
}

// cleanupOldProjectImagesWithOptions is the single image removal path for
// scoped cleanup, dry runs and the autonomous image GC. Selection is
// planOldImageRemovalsDetailed; anything that cannot be read removes nothing.
func cleanupOldProjectImagesWithOptions(ctx context.Context, dataDir string, project string, environment string, keepLatest int, imageRepositories []string, options imageCleanupOptions) (CleanupHygiene, error) {
	report := CleanupHygiene{DryRun: options.DryRun}
	if len(imageRepositories) == 0 {
		return report, nil
	}
	if environment == "" {
		return report, fmt.Errorf("old image cleanup requires an environment; no images removed")
	}
	output, err := runDocker(ctx, "images", "--filter", "label!="+composeProjectImageLabel, "--format", "{{.ID}}\t{{.Repository}}\t{{.Tag}}")
	if err != nil {
		return report, fmt.Errorf("%w: failed to list docker images: %w", errImageCleanupStateUnreadable, err)
	}
	images := parseDockerImageList(output)

	evidence, err := readImageCleanupState(dataDir, project)
	if err != nil {
		return report, fmt.Errorf("%w: old image cleanup skipped, deployment state unreadable: %w", errImageCleanupStateUnreadable, err)
	}
	inUseIDs, inUseRefs, err := containerImageUsage(ctx)
	if err != nil {
		return report, fmt.Errorf("%w: old image cleanup skipped, container image usage unreadable: %w", errImageCleanupStateUnreadable, err)
	}
	evidence.inUseIDs = inUseIDs
	evidence.inUseRefs = inUseRefs

	// The first pass is v0.14.2's selection. Creation times and sizes are
	// read for its candidates only; the grace can only keep more.
	plan := planOldImageRemovalsDetailed(images, evidence, environment, keepLatest, imageRepositories, time.Time{})
	if len(plan.Removals) > 0 {
		ids := make([]string, 0, len(plan.Removals))
		for _, removal := range plan.Removals {
			ids = append(ids, removal.ID)
		}
		metadata, metaErr := inspectImageMetadata(ctx, ids)
		if metaErr != nil && !options.CreatedAfter.IsZero() {
			return report, fmt.Errorf("%w: old image cleanup skipped, image creation times unreadable: %w", errImageCleanupStateUnreadable, metaErr)
		}
		for index := range images {
			if meta, ok := lookupImageMetadata(metadata, images[index].ID); ok {
				images[index].Created = meta.Created
				images[index].Size = meta.Size
			}
		}
		plan = planOldImageRemovalsDetailed(images, evidence, environment, keepLatest, imageRepositories, options.CreatedAfter)
	}

	kept := plan.Kept
	var failures []string
	for _, removal := range plan.Removals {
		if options.DryRun {
			report.Removed = append(report.Removed, HygieneImage{ID: removal.ID, Refs: removal.Refs, SizeBytes: removal.Size})
			report.BytesFreed += removal.Size
			continue
		}
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
			kept[removal.ID] = ImageKeptInUse
			continue
		}
		report.Removed = append(report.Removed, HygieneImage{ID: removal.ID, Refs: removal.Refs, SizeBytes: removal.Size})
		report.BytesFreed += removal.Size
	}
	report.Kept = imageCleanupPlan{Kept: kept}.keptCounts()
	report.Failed = failures
	if len(failures) > 0 {
		return report, fmt.Errorf("failed to remove %d old project image(s): %s", len(failures), strings.Join(failures, "; "))
	}
	return report, nil
}

type imageMetadata struct {
	Created time.Time
	Size    int64
}

// inspectImageMetadata reads creation time and size of the given image IDs.
func inspectImageMetadata(ctx context.Context, ids []string) (map[string]imageMetadata, error) {
	metadata := make(map[string]imageMetadata, len(ids))
	if len(ids) == 0 {
		return metadata, nil
	}
	args := append([]string{"image", "inspect", "--format", "{{.Id}}\t{{.Created}}\t{{.Size}}"}, ids...)
	output, err := runDocker(ctx, args...)
	if err != nil {
		return metadata, fmt.Errorf("failed to inspect images: %w", err)
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) != 3 || fields[0] == "" {
			continue
		}
		id := strings.TrimPrefix(fields[0], "sha256:")
		created, err := time.Parse(time.RFC3339Nano, fields[1])
		if err != nil {
			continue
		}
		size, _ := strconv.ParseInt(fields[2], 10, 64)
		metadata[id] = imageMetadata{Created: created.UTC(), Size: size}
	}
	return metadata, nil
}

func lookupImageMetadata(metadata map[string]imageMetadata, id string) (imageMetadata, bool) {
	if meta, ok := metadata[id]; ok {
		return meta, true
	}
	// docker images prints short IDs; inspect prints full IDs.
	for full, meta := range metadata {
		if id != "" && strings.HasPrefix(full, id) {
			return meta, true
		}
	}
	return imageMetadata{}, false
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
	return planOldImageRemovalsDetailed(images, evidence, environment, keepLatest, imageRepositories, time.Time{}).Removals
}

// planOldImageRemovalsDetailed is planOldImageRemovals plus the kept reason
// of every other image the target environment owns in the allowed
// repositories. A non-zero createdAfter additionally keeps images created
// after it (or of unknown age) as "recent"; the zero value selects exactly
// what planOldImageRemovals selects.
func planOldImageRemovalsDetailed(images []dockerImageEntry, evidence imageCleanupEvidence, environment string, keepLatest int, imageRepositories []string, createdAfter time.Time) imageCleanupPlan {
	plan := imageCleanupPlan{Kept: make(map[string]string)}
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
			plan.Kept[image.ID] = ImageKeptRetention
			continue
		}
		candidate[image.ID] = true
	}

	for _, id := range ids {
		if !candidate[id] {
			continue
		}
		// v0.14.2 semantics: an image beyond N in any allowed repository is
		// a candidate; the reasons below replace "retention".
		delete(plan.Kept, id)
		if imageIDInUse(id, evidence.inUseIDs) {
			plan.Kept[id] = ImageKeptInUse
			continue
		}
		// Every ref on the image must be safe to remove; a ref outside the
		// target environment or repositories keeps the whole image.
		ok := true
		var created time.Time
		var size int64
		for _, image := range images {
			if image.ID != id {
				continue
			}
			if !image.Created.IsZero() {
				created = image.Created
			}
			if image.Size > 0 {
				size = image.Size
			}
			if !removable(image) {
				ok = false
				plan.Kept[id] = imageKeptReason(image, environment, evidence, ownedByTarget(image))
				break
			}
		}
		if !ok {
			continue
		}
		if !createdAfter.IsZero() && (created.IsZero() || created.After(createdAfter)) {
			plan.Kept[id] = ImageKeptRecent
			continue
		}
		plan.Removals = append(plan.Removals, imageRemoval{ID: id, Refs: refsByID[id], Size: size})
	}
	return plan
}

// imageKeptReason explains why a non-removable ref keeps its image.
func imageKeptReason(image dockerImageEntry, environment string, evidence imageCleanupEvidence, ownedByTarget bool) string {
	switch {
	case evidence.inUseRefs[image.ref()]:
		return ImageKeptInUse
	case evidence.protectedRefs[image.ref()]:
		return ImageKeptCurrent
	case !ownedByTarget:
		return ImageKeptOtherEnvironment
	}
	for env := range imageOwners(image, evidence) {
		if env != environment {
			return ImageKeptOtherEnvironment
		}
	}
	return ImageKeptCurrent
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

// globState expands a state path pattern under dataDir one segment at a
// time with os.ReadDir. Unlike filepath.Glob it fails closed: any directory
// read or stat error other than "does not exist" is returned, so ownership
// evidence can never silently vanish and make an image look removable.
func globState(dataDir string, parts ...string) ([]string, error) {
	pattern := filepath.Join(append([]string{dataDir}, parts...)...)
	scanErr := func(err error) error {
		return fmt.Errorf("failed to scan state %s: %w", pattern, err)
	}
	current := []string{dataDir}
	for index, part := range parts {
		last := index == len(parts)-1
		var next []string
		for _, dir := range current {
			if !strings.ContainsAny(part, "*?[") {
				path := filepath.Join(dir, part)
				info, err := os.Stat(path)
				if os.IsNotExist(err) {
					continue
				}
				if err != nil {
					return nil, scanErr(err)
				}
				if last || info.IsDir() {
					next = append(next, path)
				}
				continue
			}
			entries, err := os.ReadDir(dir)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, scanErr(err)
			}
			for _, entry := range entries {
				matched, err := filepath.Match(part, entry.Name())
				if err != nil {
					return nil, scanErr(err)
				}
				if !matched {
					continue
				}
				path := filepath.Join(dir, entry.Name())
				if !last {
					info, err := os.Stat(path)
					if os.IsNotExist(err) {
						continue
					}
					if err != nil {
						return nil, scanErr(err)
					}
					if !info.IsDir() {
						continue
					}
				}
				next = append(next, path)
			}
		}
		current = next
	}
	sort.Strings(current)
	return current, nil
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
