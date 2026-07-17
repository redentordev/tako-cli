package nodeclient

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// SSHPool is the provisioning-owned connection pool capability needed to
// open an SSH direct-streamlocal runtime transport. No shell execution is
// exposed by Factory's result.
type SSHPool interface {
	GetOrCreateWithAuth(host string, port int, user string, keyPath string, password string) (*ssh.Client, error)
}

// Factory resolves and caches structured runtime clients independently from
// workload placement. Legacy server entries never probe the local socket.
type Factory struct {
	config       *config.Config
	pool         SSHPool
	remoteSocket string
	localSocket  string

	mu        sync.Mutex
	clients   map[string]*takodclient.AgentClient
	decisions map[string]Decision
	newLocal  func(string, uint32) (*takodclient.AgentClient, error)
}

// NewFactory constructs a runtime client factory. pool may be nil only when
// every requested server is explicitly and successfully resolved as local.
func NewFactory(cfg *config.Config, pool SSHPool, socket string) (*Factory, error) {
	return NewFactoryWithLocalSocket(cfg, pool, socket, takodclient.DefaultWorkerSocket)
}

// NewFactoryWithLocalSocket allows tests and embedded engines to select a
// protected worker ingress path while keeping the remote takod socket path.
func NewFactoryWithLocalSocket(cfg *config.Config, pool SSHPool, remoteSocket string, localSocket string) (*Factory, error) {
	if cfg == nil {
		return nil, fmt.Errorf("runtime client factory requires a loaded config")
	}
	if strings.TrimSpace(remoteSocket) == "" {
		remoteSocket = takodclient.DefaultSocket
	}
	if strings.TrimSpace(localSocket) == "" {
		localSocket = takodclient.DefaultWorkerSocket
	}
	return &Factory{
		config:       cfg,
		pool:         pool,
		remoteSocket: remoteSocket,
		localSocket:  localSocket,
		clients:      make(map[string]*takodclient.AgentClient),
		decisions:    make(map[string]Decision),
		newLocal:     takodclient.NewLocalAgentClientForUID,
	}, nil
}

// Client resolves transport before opening SSH. PolicyAuto never changes its
// decision after an SSH failure, so connectivity problems cannot become local
// host mutations. Enrolled SSH targets are identity-checked as well.
func (f *Factory) Client(ctx context.Context, serverName string) (*takodclient.AgentClient, Decision, error) {
	if f == nil || f.config == nil {
		return nil, Decision{}, fmt.Errorf("runtime client factory is not initialized")
	}
	serverName = strings.TrimSpace(serverName)
	server, ok := f.config.Servers[serverName]
	if !ok {
		return nil, Decision{}, fmt.Errorf("server %s not found", serverName)
	}

	f.mu.Lock()
	if client := f.clients[serverName]; client != nil {
		decision := f.decisions[serverName]
		f.mu.Unlock()
		return client, decision, nil
	}
	f.mu.Unlock()

	policy := Policy(server.Transport)
	expected := nodeidentity.Reference{ClusterID: server.ClusterID, NodeID: server.NodeID}
	var local *takodclient.AgentClient
	if policy == PolicyAuto || policy == PolicyLocal {
		var err error
		if server.WorkerUID <= 0 {
			return nil, Decision{}, fmt.Errorf("local runtime transport for %s requires a persisted non-root worker UID", serverName)
		}
		local, err = f.newLocal(f.localSocket, uint32(server.WorkerUID))
		if err != nil {
			return nil, Decision{}, fmt.Errorf("construct local runtime client for %s: %w", serverName, err)
		}
	}
	decision, err := Resolve(ctx, policy, expected, local)
	if err != nil {
		if local != nil {
			local.CloseIdleConnections()
		}
		return nil, Decision{}, fmt.Errorf("resolve runtime transport for %s: %w", serverName, err)
	}

	client := local
	if decision.Transport == TransportSSH {
		if local != nil {
			local.CloseIdleConnections()
		}
		if f.pool == nil {
			return nil, decision, fmt.Errorf("runtime transport for %s resolved to SSH but no SSH pool is available", serverName)
		}
		sshClient, connectErr := f.pool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if connectErr != nil {
			return nil, decision, fmt.Errorf("connect runtime transport for %s over SSH: %w", serverName, connectErr)
		}
		sshSocket := f.remoteSocket
		if expected.ClusterID != "" && expected.NodeID != "" && server.WorkerUID > 0 {
			sshSocket = f.localSocket
		}
		client, err = takodclient.NewAgentClient(sshClient, sshSocket)
		if err != nil {
			return nil, decision, fmt.Errorf("construct SSH-forwarded runtime client for %s: %w", serverName, err)
		}
		if expected.ClusterID != "" || expected.NodeID != "" {
			status, statusErr := client.Status(ctx)
			if statusErr != nil {
				client.CloseIdleConnections()
				return nil, decision, fmt.Errorf("verify enrolled runtime identity for %s: %w", serverName, statusErr)
			}
			if status.Identity == nil || !status.Identity.MatchesReference(expected) {
				client.CloseIdleConnections()
				return nil, decision, fmt.Errorf("verify enrolled runtime identity for %s: remote takod identity does not match configured cluster member", serverName)
			}
		}
	}

	f.mu.Lock()
	if existing := f.clients[serverName]; existing != nil {
		cachedDecision := f.decisions[serverName]
		f.mu.Unlock()
		client.CloseIdleConnections()
		return existing, cachedDecision, nil
	}
	f.clients[serverName] = client
	f.decisions[serverName] = decision
	f.mu.Unlock()
	return client, decision, nil
}

// Decision returns a previously resolved decision without causing a probe or
// connection. The bool is false until Client has successfully resolved it.
func (f *Factory) Decision(serverName string) (Decision, bool) {
	if f == nil {
		return Decision{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	decision, ok := f.decisions[strings.TrimSpace(serverName)]
	return decision, ok
}

// CloseIdleConnections releases runtime HTTP keepalives. The caller still
// owns and closes the SSH pool.
func (f *Factory) CloseIdleConnections() {
	if f == nil {
		return
	}
	f.mu.Lock()
	clients := make([]*takodclient.AgentClient, 0, len(f.clients))
	for _, client := range f.clients {
		clients = append(clients, client)
	}
	f.mu.Unlock()
	for _, client := range clients {
		client.CloseIdleConnections()
	}
}
