package platform

import (
	"fmt"
	"path/filepath"
	"strings"
)

const (
	TakodUnitName  = "takod.service"
	WorkerUnitName = "tako-platform-worker.service"
)

func RenderTakodUnit(binaryPath string, socketPath string, stateDir string, nodeName string, identityPath string, dockerDataRoot string, socketGroup string, policy ResourcePolicy) (string, error) {
	for label, value := range map[string]string{
		"binary path": binaryPath, "socket path": socketPath, "state directory": stateDir,
		"node name": nodeName, "identity path": identityPath, "Docker data root": dockerDataRoot, "socket group": socketGroup,
	} {
		if err := validateSystemdArgument(label, value); err != nil {
			return "", err
		}
	}
	if err := policy.Validate(); err != nil {
		return "", err
	}
	return fmt.Sprintf(`[Unit]
Description=Tako node agent
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
Type=simple
User=root
Group=%s
RuntimeDirectory=tako
RuntimeDirectoryMode=0750
UMask=0007
ExecStart=%s takod run --socket %s --data-dir %s --node %s --identity-file %s --minimum-free-disk-bytes %d --max-concurrent-builds %d --docker-data-root %s
Restart=always
RestartSec=5
OOMScoreAdjust=-500
MemoryMin=%d
MemoryLow=%d
TasksMax=512
CPUWeight=200
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=%s /run/tako /etc/wireguard /etc/tako/proxy /var/log/tako/proxy

[Install]
WantedBy=multi-user.target
	`, socketGroup, binaryPath, socketPath, filepath.Dir(stateDir), nodeName, identityPath,
		policy.MinimumFreeDiskBytes, policy.MaximumConcurrentBuilds,
		dockerDataRoot, policy.ReservedMemoryBytes, policy.ReservedMemoryBytes, filepath.Dir(stateDir)), nil
}

func RenderWorkerUnit(config BootstrapConfig) (string, error) {
	config = config.withDefaults()
	if err := config.Validate(); err != nil {
		return "", err
	}
	for label, value := range map[string]string{
		"binary path": config.ServiceBinaryPath, "socket path": config.SocketPath,
		"worker socket":   config.WorkerSocketPath,
		"state directory": config.StateDir, "config directory": config.ConfigDir,
		"audit directory": config.AuditDir, "identity path": config.IdentityPath,
	} {
		if err := validateSystemdArgument(label, value); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf(`[Unit]
Description=Tako durable platform deployment worker
After=network-online.target takod.service
Wants=network-online.target
Requires=takod.service

[Service]
Type=simple
User=%s
Group=%s
SupplementaryGroups=%s
UMask=0027
ExecStart=%s platform worker run --state-dir %s --config %s/platform.json --socket %s
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal
SyslogIdentifier=tako-platform-worker
OOMScoreAdjust=-500
MemoryMin=%d
MemoryLow=%d
TasksMax=256
CPUWeight=200
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true
RuntimeDirectory=tako-platform
RuntimeDirectoryMode=0750
ReadOnlyPaths=%s %s/platform.json %s
ReadWritePaths=%s /run/tako-platform

[Install]
WantedBy=multi-user.target
`, config.WorkerUser, config.WorkerGroup, config.SocketGroup, config.ServiceBinaryPath, config.StateDir, config.ConfigDir, config.SocketPath,
		config.Policy.ReservedMemoryBytes, config.Policy.ReservedMemoryBytes,
		config.IdentityPath, config.ConfigDir, config.AuditDir, config.StateDir), nil
}

func validateSystemdArgument(label string, value string) error {
	if strings.TrimSpace(value) == "" || strings.ContainsAny(value, " \t\r\n\x00%") {
		return fmt.Errorf("invalid %s", label)
	}
	return nil
}
