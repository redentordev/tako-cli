# Tako API and SDK Foundation

This document describes the importable foundation APIs that exist today. These
packages are building blocks for Tako integrations and tests; they are not a
promise that `takod` exposes a public TCP REST API.

## Importable Packages

### `pkg/takoapi`

`pkg/takoapi` contains transport-neutral schema and identity types:

- API/version constants such as `APIVersionCurrent` and state schema constants.
- Deployment identity types that keep source, deployment revision, service
  revision, image identity, and optional git metadata separate.
- Canonical desired, actual, actual-node, event, deployment, and deployment
  history documents used around takod state.

Use this package when code needs to construct or decode Tako state documents
without importing CLI commands or `internal/*` packages.

### `pkg/takoapi/stateclient`

`pkg/takoapi/stateclient` is a typed client for takod `/v1/state` documents. It
uses the existing private `pkg/takodclient` request executor abstraction, which
normally runs commands over SSH and talks to takod through its Unix socket.

Supported helpers include reading and writing desired state, aggregate actual
state, per-node actual state, deployment history, single deployment records, and
appending state events.

### `pkg/deployplan`

`pkg/deployplan` contains CLI-independent deploy planning helpers, including:

- Image reference selection and merging with deployed/actual state.
- Source, archive, and image build tag generation for non-git,
  archive-backed, and image-backed deploys.
- Service selection for targeted deploys and force behavior, including targeted
  archive deploy adapters used by `tako deploy --service <name> --archive <file>`.
- Active revision planning for rolling and blue/green proxy behavior.
- Stable per-service revision IDs derived from project, environment, service,
  image reference, service config hash, and deploy strategy.

Use this package for planning logic that should be unit-testable without Cobra,
Viper, or command output.

## Current Image Deploy Boundary

Tako has two image deploy paths today:

- `tako deploy --service <name> --image <ref>` deploys an image for one service
  in an existing configured Tako project. It still requires `tako.yaml` with a
  defined service, server, and environment.
- `tako run IMAGE --name <name> --port <port> --server <host>` is the first
  configless path. It deploys a public image to an existing SSH-reachable
  VPS/takod node without local `tako.yaml`, while still using takod state,
  labels, leases, history, and proxy reconciliation.

`tako run` is public-image-only in this milestone. Private registry auth,
compose import, cloud provisioning, and discovery of arbitrary non-Tako Docker
containers are outside the current boundary. See the
[foundation roadmap](FOUNDATION-ROADMAP.md#configless-public-image-deploy-direction).

## Transport Boundary

Today the state client is intentionally private-control-plane only:

```text
integration code -> takodclient.RequestExecutor -> SSH executor -> curl --unix-socket /run/tako/takod.sock -> takod
```

There is no public network REST API, no public TCP listener contract, and no
external auth/TLS story for exposing takod directly. Integrations should treat
the Unix-socket API as node-local and use the same owned-server SSH boundary as
the CLI. If Tako later adds a public network API, it will need a separate
authentication, TLS, auditing, and operator opt-in design.

## State Client Examples

These examples use fake executor language so they do not contain credentials or
host-specific SSH details. In real code, provide an executor that satisfies
`takodclient.RequestExecutor` and reaches an operator-owned server.

```go
package example

import (
    "context"
    "io"
    "time"

    "github.com/redentordev/tako-cli/pkg/takoapi"
    "github.com/redentordev/tako-cli/pkg/takoapi/stateclient"
)

type fakeExecutor struct{}

func (fakeExecutor) ExecuteWithContext(ctx context.Context, cmd string) (string, error) {
    // Pseudocode: run cmd on the target server, for example over an SSH session.
    // The command is built by takodclient and uses curl against takod's Unix socket.
    return `{"found":true,"content":"{}"}`, nil
}

func (fakeExecutor) ExecuteWithInput(ctx context.Context, cmd string, input io.Reader) (string, error) {
    // Pseudocode: run cmd on the target server and stream input as the JSON body.
    return `{"found":true}`, nil
}

func readDesired() error {
    client := stateclient.New(fakeExecutor{}).
        WithSocket("/run/tako/takod.sock").
        WithTimeout(30 * time.Second)

    desired, err := client.ReadDesired("web-app", "production")
    if err != nil {
        return err
    }
    _ = desired.RevisionID
    return nil
}

func writeDesired() error {
    client := stateclient.New(fakeExecutor{})

    doc := takoapi.NewDesiredStateDocument("web-app", "production", "ci-20260705.12")
    doc.Source = "directory:."
    doc.TargetNodes = []string{"prod-1"}
    doc.Services["web"] = takoapi.DesiredServiceDocument{
        Name:     "web",
        Build:    ".",
        Replicas: 1,
    }

    return client.WriteDesired(doc)
}
```

For reads, `stateclient.ErrNotFound` indicates takod reported that the requested
state document was absent.

## Schema Versioning and Migration Guidance

- New canonical documents should use `takoapi.APIVersionCurrent` and the
  package constructors where available.
- State documents also carry `SchemaVersion`; current `/v1/state` documents use
  `takoapi.StateSchemaVersionCurrent`.
- Prefer additive fields. Readers should ignore unknown fields and tolerate
  omitted optional fields.
- Git metadata is optional display/trace information. Do not require it for
  directory, archive, image, CI, or other non-git inputs.
- Archive-backed deploys should use archive content identity (or an explicit
  revision label) for build tags; `pkg/deployplan.ArchiveBuildTag` implements
  the current deterministic tag helper.
- Do not treat a git commit as the deployment revision ID. Use
  `RevisionIdentity.ID`, deployment history `ID`, or service revision IDs for
  Tako revision identity.
- Deployment history schemas in `takoapi` intentionally mirror the existing
  internal JSON shape so replicated history can be decoded without importing
  `internal/state`.
- Avoid synthetic git commits to force non-git deploys through old code paths;
  leave git fields empty when no real git metadata exists.

## Deploy Progress Output

`pkg/deployer.Deployer` supports progress output injection:

```go
var out bytes.Buffer

deploy := &deployer.Deployer{}
deploy.SetOutput(&out)     // capture progress output
deploy.SetOutput(io.Discard) // silence progress output
deploy.SetOutput(nil)      // reset to os.Stdout
```

This only documents the deployer output hook that exists today. Broader stdout
injection across all reusable packages remains deferred.
