package takod

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/selfheal"
)

// healScopeLocks serialize converged-record mutations (reconcile, remove,
// restore, cleanup, preview teardown) with a self-healing action's critical
// section in the same environment, so an in-flight docker restart/start
// finishes before a teardown or rollout rewrites the record.
var healScopeLocks sync.Map

func healScopeLock(project, environment string) *sync.Mutex {
	value, _ := healScopeLocks.LoadOrStore(project+"/"+environment, &sync.Mutex{})
	return value.(*sync.Mutex)
}

var healRecordNow = func() time.Time { return time.Now().UTC() }

func logHealRecordError(action string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "takod: self-healing record %s failed: %v\n", action, err)
	}
}

// reconcileIsWarmOnly reports whether every container of a reconcile is a
// blue-green warm container (tako.active=false). Warming never changes the
// live containers, so it leaves their converged record alone.
func reconcileIsWarmOnly(req ReconcileServiceRequest) bool {
	if len(req.Containers) == 0 {
		return false
	}
	for _, container := range req.Containers {
		if container.Labels["tako.active"] != "false" {
			return false
		}
	}
	return true
}

// healMarkConverging marks the service's record converging before a
// reconcile mutates its containers.
func healMarkConverging(req ReconcileServiceRequest) {
	if strings.TrimSpace(req.DataDir) == "" || reconcileIsWarmOnly(req) {
		return
	}
	lock := healScopeLock(req.Project, req.Environment)
	lock.Lock()
	defer lock.Unlock()
	logHealRecordError("mark converging", selfheal.MarkConverging(req.DataDir, req.Project, req.Environment, req.Service, req.Image, req.Revision, healRecordNow()))
}

// healRecordConverged writes the final record after every container started
// and passed its readiness wait, using the IDs `docker run -d` printed. A
// container without a recognisable ID leaves the record converging, which
// self-healing ignores.
func healRecordConverged(req ReconcileServiceRequest, started []string, ids map[string]string) {
	if strings.TrimSpace(req.DataDir) == "" || reconcileIsWarmOnly(req) {
		return
	}
	containers := make([]selfheal.RecordContainer, 0, len(started))
	for _, name := range started {
		id := ids[name]
		if !isHexIdentifier(id) {
			logHealRecordError("write", fmt.Errorf("container %s could not be identified", name))
			return
		}
		containers = append(containers, selfheal.RecordContainer{Name: name, ID: id})
	}
	lock := healScopeLock(req.Project, req.Environment)
	lock.Lock()
	defer lock.Unlock()
	logHealRecordError("write", selfheal.Converged(req.DataDir, req.Project, req.Environment, req.Service, selfheal.Record{
		Image: req.Image, Revision: req.Revision, Containers: containers, Restart: req.Restart, RecordedAt: healRecordNow(),
	}))
}

// healClearRecord removes a service's record (scale to zero, removal).
func healClearRecord(dataDir, project, environment, service string) {
	if strings.TrimSpace(dataDir) == "" {
		return
	}
	lock := healScopeLock(project, environment)
	lock.Lock()
	defer lock.Unlock()
	logHealRecordError("clear", selfheal.ClearRecord(dataDir, project, environment, service))
}

// healClearRecordUnlessRevision removes a service's record unless it names
// keepRevision (a rolling or blue-green cleanup of older revisions).
func healClearRecordUnlessRevision(dataDir, project, environment, service, keepRevision string) {
	if strings.TrimSpace(dataDir) == "" {
		return
	}
	lock := healScopeLock(project, environment)
	lock.Lock()
	defer lock.Unlock()
	if keepRevision != "" {
		record, err := selfheal.ReadRecord(dataDir, project, environment, service)
		if err == nil && record != nil && record.Revision == keepRevision {
			return
		}
	}
	logHealRecordError("clear", selfheal.ClearRecord(dataDir, project, environment, service))
}

// healClearScope removes every heal document of an environment (or of the
// whole project when environment is empty) before its containers are torn
// down. It waits for an in-flight heal action of that environment.
func healClearScope(dataDir, project, environment string) error {
	if strings.TrimSpace(dataDir) == "" {
		return nil
	}
	if environment == "" {
		environments, err := selfheal.Environments(dataDir, project)
		if err != nil {
			return err
		}
		for _, env := range environments {
			lock := healScopeLock(project, env)
			lock.Lock()
			lock.Unlock()
		}
		return selfheal.ClearProject(dataDir, project)
	}
	lock := healScopeLock(project, environment)
	lock.Lock()
	defer lock.Unlock()
	return selfheal.ClearScope(dataDir, project, environment)
}

// healRecordRestored rebuilds the record from the previous containers a
// restore brought back under their original names and IDs. When they do not
// match the previous record the record is cleared, so self-healing ignores
// the service until its next successful reconcile.
func healRecordRestored(dataDir string, state *retainedServiceState) {
	if strings.TrimSpace(dataDir) == "" || state == nil {
		return
	}
	lock := healScopeLock(state.Project, state.Environment)
	lock.Lock()
	defer lock.Unlock()
	restored := make([]selfheal.RecordContainer, 0, len(state.Containers))
	for _, retained := range state.Containers {
		restored = append(restored, selfheal.RecordContainer{Name: retained.Name, ID: retained.ID})
	}
	logHealRecordError("restore", selfheal.RestoreRecord(dataDir, state.Project, state.Environment, state.Service, restored, healRecordNow()))
}
