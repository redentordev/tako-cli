# Placement Strategies Test Example

This example exercises takod placement across one or more nodes.

## Services

- `web-default`: default placement with two replicas.
- `web-spread`: spreads three replicas across nodes labeled `role=web`.
- `monitoring-agent`: global placement, one container per node.
- `pinned-api`: pinned placement on `node1`.

## Run

```bash
cp .env.example .env
tako setup
tako deploy
tako ps
```

## Verify

```bash
tako ps --server node1
tako ps --server node2
```

Global placement should create one `monitoring-agent` container per configured node. Spread placement should balance slots across reachable nodes that match the service constraints. Pinned placement should keep the service on the named node.
