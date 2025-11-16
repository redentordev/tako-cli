# Tako Local State Management Design

## Overview
Local state management for Tako CLI stored in `.tako/` directory to track:
- Deployment history and rollback points
- Service states and configurations
- Build cache and artifacts
- Logs and metrics
- Swarm/single-server mode transitions

## Directory Structure

```
.tako/
├── .gitignore                     # Auto-generated, ignores everything
├── state.json                     # Global state file
├── deployments/                   # Deployment history
│   ├── production/
│   │   ├── current.json          # Current deployment state
│   │   ├── history/              # Historical deployments
│   │   │   ├── 2024-11-13T10-30-00Z.json
│   │   │   └── 2024-11-12T15-45-00Z.json
│   │   └── rollback/             # Rollback points
│   │       └── last-stable.json
│   └── staging/
│       └── ...
├── services/                      # Per-service state
│   ├── web/
│   │   ├── state.json           # Service state
│   │   ├── health.json          # Health check history
│   │   └── metrics.json         # Performance metrics
│   └── api/
│       └── ...
├── builds/                        # Build artifacts and cache
│   ├── cache/
│   │   ├── web/
│   │   │   └── checksums.json   # File checksums for change detection
│   │   └── api/
│   └── artifacts/
│       ├── web-v1.0.0-abc123.tar
│       └── api-v1.0.0-def456.tar
├── logs/                          # Local logs
│   ├── deployments/
│   │   ├── 2024-11-13T10-30-00Z.log
│   │   └── 2024-11-12T15-45-00Z.log
│   ├── builds/
│   │   └── ...
│   └── errors/
│       └── ...
├── swarm/                         # Swarm-specific state
│   ├── cluster.json              # Cluster configuration
│   ├── nodes.json                # Node information
│   └── services.json             # Service definitions
└── config/                        # Cached configurations
    ├── traefik/
    │   ├── routes.json           # Route configurations
    │   └── certificates.json     # SSL cert tracking
    └── networks/
        └── overlay.json          # Network configurations
```

## State File Schemas

### 1. Global State (`state.json`)
```json
{
  "version": "1.0.0",
  "project": {
    "name": "myapp",
    "created_at": "2024-11-13T10:00:00Z",
    "last_deployed": "2024-11-13T10:30:00Z"
  },
  "mode": "swarm|single",  // Current deployment mode
  "environments": {
    "production": {
      "active": true,
      "servers": ["server1", "server2"],
      "last_deployment": "2024-11-13T10:30:00Z",
      "deployment_id": "deploy-abc123"
    }
  },
  "metadata": {
    "tako_version": "1.0.0",
    "config_hash": "sha256:abc..."
  }
}
```

### 2. Deployment State (`deployments/production/current.json`)
```json
{
  "deployment_id": "deploy-abc123",
  "timestamp": "2024-11-13T10:30:00Z",
  "environment": "production",
  "mode": "swarm",
  "status": "success|failed|partial",
  "duration_seconds": 45,
  "services": {
    "web": {
      "image": "myapp/web:v1.0.0",
      "image_id": "sha256:abc123...",
      "replicas": 3,
      "ports": [3000],
      "domains": ["myapp.com"],
      "health": "healthy",
      "containers": [
        {
          "id": "container123",
          "node": "server1",
          "status": "running",
          "started_at": "2024-11-13T10:30:00Z"
        }
      ]
    }
  },
  "network": {
    "name": "tako_myapp_production",
    "driver": "overlay",
    "id": "network123"
  },
  "volumes": [
    {
      "name": "myapp_data",
      "driver": "local",
      "mount_point": "/data"
    }
  ],
  "config": {
    "hash": "sha256:def456...",
    "source": "tako.yaml"
  },
  "rollback_point": true,  // Mark as safe rollback point
  "notes": "Added caching layer"
}
```

### 3. Service State (`services/web/state.json`)
```json
{
  "name": "web",
  "current": {
    "version": "v1.0.0",
    "image": "myapp/web:v1.0.0",
    "deployed_at": "2024-11-13T10:30:00Z",
    "replicas": 3,
    "status": "running"
  },
  "history": [
    {
      "version": "v0.9.0",
      "deployed_at": "2024-11-12T15:45:00Z",
      "retired_at": "2024-11-13T10:30:00Z",
      "reason": "upgrade"
    }
  ],
  "health_checks": {
    "endpoint": "/health",
    "last_check": "2024-11-13T10:35:00Z",
    "status": "healthy",
    "response_time_ms": 25
  },
  "resources": {
    "cpu_limit": "500m",
    "memory_limit": "512Mi",
    "current_cpu": "120m",
    "current_memory": "256Mi"
  }
}
```

### 4. Build Cache (`builds/cache/web/checksums.json`)
```json
{
  "last_build": "2024-11-13T10:25:00Z",
  "files": {
    "package.json": "sha256:abc123...",
    "src/index.js": "sha256:def456...",
    "Dockerfile": "sha256:ghi789..."
  },
  "image": {
    "tag": "myapp/web:v1.0.0",
    "id": "sha256:jkl012...",
    "size_bytes": 125829120
  },
  "build_args": {
    "NODE_ENV": "production"
  }
}
```

### 5. Swarm State (`swarm/cluster.json`)
```json
{
  "initialized": true,
  "manager_token": "SWMTKN-1-...",
  "worker_token": "SWMTKN-1-...",
  "created_at": "2024-11-10T08:00:00Z",
  "nodes": [
    {
      "id": "node123",
      "hostname": "server1",
      "role": "manager",
      "status": "ready",
      "ip": "10.0.0.1"
    },
    {
      "id": "node456",
      "hostname": "server2",
      "role": "worker",
      "status": "ready",
      "ip": "10.0.0.2"
    }
  ],
  "services": ["traefik", "registry", "myapp_production_web"],
  "networks": ["tako_myapp_production"],
  "registry": {
    "url": "10.0.0.1:5000",
    "insecure": true
  }
}
```

## Implementation Code

### State Manager Package
```go
// pkg/state/manager.go
package state

import (
    "encoding/json"
    "fmt"
    "os"
    "path/filepath"
    "time"
)

type Manager struct {
    basePath string
    project  string
    env      string
}

func NewManager(projectPath, projectName, environment string) (*Manager, error) {
    basePath := filepath.Join(projectPath, ".tako")

    // Ensure .tako directory exists
    if err := os.MkdirAll(basePath, 0755); err != nil {
        return nil, err
    }

    // Create .gitignore if it doesn't exist
    gitignorePath := filepath.Join(basePath, ".gitignore")
    if _, err := os.Stat(gitignorePath); os.IsNotExist(err) {
        if err := os.WriteFile(gitignorePath, []byte("*\n!.gitignore\n"), 0644); err != nil {
            return nil, err
        }
    }

    return &Manager{
        basePath: basePath,
        project:  projectName,
        env:      environment,
    }, nil
}

// SaveDeployment saves deployment state
func (m *Manager) SaveDeployment(deployment *DeploymentState) error {
    // Save to current
    currentPath := filepath.Join(m.basePath, "deployments", m.env, "current.json")
    if err := m.saveJSON(currentPath, deployment); err != nil {
        return err
    }

    // Save to history
    timestamp := deployment.Timestamp.Format("2006-01-02T15-04-05Z")
    historyPath := filepath.Join(m.basePath, "deployments", m.env, "history", timestamp+".json")
    if err := m.saveJSON(historyPath, deployment); err != nil {
        return err
    }

    // Update global state
    return m.updateGlobalState(deployment)
}

// GetCurrentDeployment returns the current deployment state
func (m *Manager) GetCurrentDeployment() (*DeploymentState, error) {
    path := filepath.Join(m.basePath, "deployments", m.env, "current.json")
    return m.loadJSON(path, &DeploymentState{})
}

// SaveServiceState saves service-specific state
func (m *Manager) SaveServiceState(serviceName string, state *ServiceState) error {
    path := filepath.Join(m.basePath, "services", serviceName, "state.json")
    return m.saveJSON(path, state)
}

// LogDeployment logs deployment output
func (m *Manager) LogDeployment(message string) error {
    timestamp := time.Now().Format("2006-01-02T15-04-05Z")
    logPath := filepath.Join(m.basePath, "logs", "deployments", timestamp+".log")

    // Ensure directory exists
    if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
        return err
    }

    f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
    if err != nil {
        return err
    }
    defer f.Close()

    _, err = f.WriteString(fmt.Sprintf("[%s] %s\n", time.Now().Format(time.RFC3339), message))
    return err
}

// Helper methods
func (m *Manager) saveJSON(path string, data interface{}) error {
    // Ensure directory exists
    if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
        return err
    }

    bytes, err := json.MarshalIndent(data, "", "  ")
    if err != nil {
        return err
    }

    return os.WriteFile(path, bytes, 0644)
}

func (m *Manager) loadJSON(path string, target interface{}) error {
    bytes, err := os.ReadFile(path)
    if err != nil {
        return err
    }

    return json.Unmarshal(bytes, target)
}
```

## Usage in Deploy Command

```go
// In cmd/deploy.go
stateManager, err := state.NewManager(".", cfg.Project.Name, environment)
if err != nil {
    return fmt.Errorf("failed to initialize state manager: %w", err)
}

// Log deployment start
stateManager.LogDeployment("Starting deployment...")

// Save deployment state after success
deployment := &state.DeploymentState{
    DeploymentID: generateID(),
    Timestamp:    time.Now(),
    Environment:  environment,
    Mode:         deploymentMode,
    Status:       "success",
    // ... other fields
}
stateManager.SaveDeployment(deployment)
```

## Benefits

1. **Rollback Capability**: Can restore previous deployment states
2. **Downgrade Detection**: Automatically detect when switching from swarm to single-server
3. **Build Cache**: Skip rebuilds when files haven't changed
4. **Audit Trail**: Complete deployment history with logs
5. **Health Tracking**: Monitor service health over time
6. **Resource Usage**: Track resource consumption patterns
7. **Debugging**: Detailed logs for troubleshooting

## Migration Path

For existing projects without `.tako/`:
1. Create directory structure on first run
2. Infer current state from Docker inspection
3. Mark as "imported" deployment
4. Continue tracking from that point