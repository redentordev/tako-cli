// Package takoapi defines Tako's canonical schema and identity value types.
//
// The package is intentionally small and transport-neutral. It is a foundation
// for API contracts, replicated state documents, local caches, and future SDKs;
// it is not a takod transport client and must not depend on CLI, SSH, Docker,
// cobra/viper, or internal packages.
//
// Identity fields deliberately separate source identity, whole-deployment
// revision identity, per-service revision identity, image identity, and optional
// git display metadata. Git metadata is trace information only and should not be
// used as a synthetic deployment revision for non-git sources.
package takoapi
