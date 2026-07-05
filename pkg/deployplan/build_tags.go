package deployplan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

const maxDockerTagLength = 128

// ValidateBuildTag validates a Docker image tag used for build outputs.
// It follows Docker tag constraints: non-empty, at most 128 characters,
// first character [A-Za-z0-9_], and remaining characters [A-Za-z0-9_.-].
func ValidateBuildTag(tag string) error {
	if tag == "" {
		return fmt.Errorf("build tag must not be empty")
	}
	if len(tag) > maxDockerTagLength {
		return fmt.Errorf("build tag %q is too long: maximum length is %d characters", tag, maxDockerTagLength)
	}
	if !isDockerTagFirstChar(tag[0]) {
		return fmt.Errorf("build tag %q has invalid first character %q: must be [A-Za-z0-9_]", tag, tag[0])
	}
	for i := 1; i < len(tag); i++ {
		if !isDockerTagChar(tag[i]) {
			return fmt.Errorf("build tag %q has invalid character %q at position %d: must be [A-Za-z0-9_.-]", tag, tag[i], i)
		}
	}
	return nil
}

// SourceBuildTag returns the build tag for a source deploy.
// An explicit revision is validated and returned unchanged. If no explicit
// revision is provided, a deterministic UTC timestamp tag is generated.
func SourceBuildTag(explicitRevision string, now time.Time) (string, error) {
	if explicitRevision != "" {
		if err := ValidateBuildTag(explicitRevision); err != nil {
			return "", err
		}
		return explicitRevision, nil
	}
	return "source-" + now.UTC().Format("20060102T150405Z"), nil
}

// ImageBuildTag returns the build tag for a deploy from an existing image.
// An explicit revision is validated and returned unchanged. If no explicit
// revision is provided, a deterministic tag is derived from the trimmed image ref.
func ImageBuildTag(explicitRevision string, imageRef string) (string, error) {
	if explicitRevision != "" {
		if err := ValidateBuildTag(explicitRevision); err != nil {
			return "", err
		}
		return explicitRevision, nil
	}

	trimmedImageRef := strings.TrimSpace(imageRef)
	if trimmedImageRef == "" {
		return "", fmt.Errorf("image ref must not be empty when deriving image build tag")
	}

	sum := sha256.Sum256([]byte(trimmedImageRef))
	return "image-" + hex.EncodeToString(sum[:])[:12], nil
}

// ArchiveBuildTag returns the build tag for a deploy from a local source archive.
// An explicit revision is validated and returned unchanged. If no explicit
// revision is provided, a deterministic tag is derived from the archive digest.
func ArchiveBuildTag(explicitRevision string, digest []byte) (string, error) {
	if explicitRevision != "" {
		if err := ValidateBuildTag(explicitRevision); err != nil {
			return "", err
		}
		return explicitRevision, nil
	}
	if len(digest) < 6 {
		return "", fmt.Errorf("archive digest must be at least 6 bytes when deriving archive build tag")
	}
	return "archive-" + hex.EncodeToString(digest)[:12], nil
}

func isDockerTagFirstChar(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_'
}

func isDockerTagChar(c byte) bool {
	return isDockerTagFirstChar(c) || c == '.' || c == '-'
}
