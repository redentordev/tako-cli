package platform

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/redentordev/tako-cli/pkg/takodclient"
)

type RuntimeAgent interface {
	Status(context.Context) (*takodclient.AgentStatus, error)
	RequestJSON(context.Context, string, string, any) (string, error)
}

type Worker struct {
	configPath string
	stateDir   string
	socketPath string
	agent      RuntimeAgent
	disk       DiskProbe
	now        func() time.Time
}

func NewWorker(configPath string, stateDir string, socketPath string, agent RuntimeAgent, disk DiskProbe) (*Worker, error) {
	if configPath == "" || stateDir == "" || socketPath == "" || agent == nil || disk == nil {
		return nil, fmt.Errorf("worker config, state, socket, agent, and disk probe are required")
	}
	return &Worker{configPath: configPath, stateDir: stateDir, socketPath: socketPath, agent: agent, disk: disk, now: time.Now}, nil
}

func (w *Worker) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	document, err := readConfigDocument(w.configPath)
	if err != nil {
		return fmt.Errorf("load platform worker config: %w", err)
	}
	if err := document.Policy.Validate(); err != nil {
		return err
	}
	if err := document.State.Validate(); err != nil {
		return err
	}
	if filepath.Clean(w.stateDir) != filepath.Clean(document.State.StateDir) {
		return fmt.Errorf("worker state directory does not match protected platform config")
	}
	if filepath.Clean(w.socketPath) != filepath.Clean(document.State.SocketPath) {
		return fmt.Errorf("worker socket does not match protected platform config")
	}
	status, err := w.agent.Status(ctx)
	if err != nil {
		return fmt.Errorf("attest local takod identity: %w", err)
	}
	if status != nil {
		if err := status.Validate(); err != nil {
			return fmt.Errorf("attest local takod identity: %w", err)
		}
	}
	if status == nil || status.Identity == nil || !status.Identity.Matches(document.State.ClusterID, document.State.NodeID) {
		return fmt.Errorf("local takod does not match the bootstrapped controller identity")
	}
	admission, err := NewAdmissionController(document.Policy, w.disk, w.stateDir)
	if err != nil {
		return err
	}
	store, err := NewOperationStore(w.stateDir, document.State.NodeID, w.now)
	if err != nil {
		return err
	}
	engine, err := NewOperationEngine(store, AgentOperationExecutor{Agent: w.agent}, admission, document.Policy.MaximumConcurrentOps)
	if err != nil {
		return err
	}
	ingress, err := newWorkerIngress(document.State.WorkerSocketPath, document.State.SocketGroupGID, store, w.agent, admission, w.stateDir, document.State.NodeID, w.now)
	if err != nil {
		return err
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = ingress.Shutdown(shutdownCtx)
	}()
	journal, _ := NewJournal(filepath.Join(w.stateDir, DefaultJournalName))
	operationID := fmt.Sprintf("worker-start-%d", w.now().UnixNano())
	if err := journal.Append(OperationRecord{
		OperationID: operationID, Operation: "platform.worker", Phase: "ready", Status: "completed",
		NodeID: document.State.NodeID, Message: "durable single-controller worker ready", Timestamp: w.now().UTC(),
	}); err != nil {
		return err
	}
	log.Printf("platform audit: operation=%s phase=identity-attestation status=completed node=%s", operationID, document.State.NodeID)
	errCh := make(chan error, 2)
	go func() { errCh <- ingress.Serve() }()
	go func() { errCh <- engine.Run(runCtx) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case runErr := <-errCh:
		if runErr == nil || runErr == context.Canceled {
			return runErr
		}
		return runErr
	}
}
