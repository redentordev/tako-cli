package selfheal

import (
	"os"
	"strings"
	"time"
)

// Record is a service's converged record on one node: the containers takod
// started and saw pass their readiness wait. Self-healing only ever acts on
// containers listed in a valid record.
type Record struct {
	SchemaVersion int               `json:"schemaVersion"`
	Image         string            `json:"image"`
	Revision      string            `json:"revision,omitempty"`
	Containers    []RecordContainer `json:"containers"`
	Restart       string            `json:"restart,omitempty"`
	RecordedAt    time.Time         `json:"recordedAt"`
	// Converging is set before a reconcile starts. A converging record is
	// never valid; it keeps the previous containers listed so disk hygiene
	// still protects them while the rollout runs.
	Converging bool `json:"converging,omitempty"`
	// Previous remembers the converged record a rollout replaces, so a
	// restore-on-failure can rebuild it without asking docker.
	Previous *PreviousRecord `json:"previous,omitempty"`
}

// PreviousRecord is the converged state before the latest rollout.
type PreviousRecord struct {
	Image      string            `json:"image"`
	Revision   string            `json:"revision,omitempty"`
	Restart    string            `json:"restart,omitempty"`
	Containers []RecordContainer `json:"containers"`
}

// RecordContainer identifies one recorded container by name and ID.
type RecordContainer struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

// Valid reports whether the record may be acted on for desiredImage: it is
// not converging, lists containers and names the desired image.
func (r *Record) Valid(desiredImage string) bool {
	return r != nil && !r.Converging && len(r.Containers) > 0 && r.Image != "" && r.Image == desiredImage
}

// Contains reports whether the record lists a container with this ID.
func (r *Record) Contains(id string) bool {
	if r == nil {
		return false
	}
	for _, container := range r.Containers {
		if container.ID == id {
			return true
		}
	}
	return false
}

// matchID returns the full recorded ID equal to id or starting with it
// (a short ID of at least 12 characters).
func (r *Record) matchID(id string) (string, bool) {
	if r == nil || len(id) < 12 {
		return "", false
	}
	for _, container := range r.Containers {
		if container.ID == id || strings.HasPrefix(container.ID, id) {
			return container.ID, true
		}
	}
	return "", false
}

// ReadRecord returns the service's record, or nil when none exists.
func ReadRecord(dataDir, project, environment, service string) (*Record, error) {
	path, err := RecordPath(dataDir, project, environment, service)
	if err != nil {
		return nil, err
	}
	var record Record
	found, err := readJSON(path, &record)
	if err != nil || !found {
		return nil, err
	}
	return &record, nil
}

// WriteRecord replaces the service's record.
func WriteRecord(dataDir, project, environment, service string, record Record) error {
	path, err := RecordPath(dataDir, project, environment, service)
	if err != nil {
		return err
	}
	record.SchemaVersion = SchemaVersion
	for i := range record.Containers {
		record.Containers[i].Name = strings.TrimPrefix(record.Containers[i].Name, "/")
	}
	if record.Containers == nil {
		record.Containers = []RecordContainer{}
	}
	record.RecordedAt = record.RecordedAt.UTC()
	return writeJSONAtomic(path, record)
}

// MarkConverging marks the service's record converging before a reconcile
// starts. The previous containers stay listed (for disk hygiene); image and
// revision name the rollout in progress.
func MarkConverging(dataDir, project, environment, service, image, revision string, now time.Time) error {
	previous, err := ReadRecord(dataDir, project, environment, service)
	if err != nil {
		// An unreadable record must not block a deploy; replace it.
		previous = nil
	}
	record := Record{Image: image, Revision: revision, RecordedAt: now, Converging: true}
	if previous != nil {
		record.Containers = append([]RecordContainer(nil), previous.Containers...)
		record.Restart = previous.Restart
		if previous.Converging {
			record.Previous = previous.Previous
		} else if previous.Image != "" && len(previous.Containers) > 0 {
			record.Previous = &PreviousRecord{
				Image:      previous.Image,
				Revision:   previous.Revision,
				Restart:    previous.Restart,
				Containers: append([]RecordContainer(nil), previous.Containers...),
			}
		}
	}
	return WriteRecord(dataDir, project, environment, service, record)
}

// Converged returns the final record for a rollout that succeeded with
// containers. It keeps the converging record's Previous so a cross-node
// restore (the rollout failed on another node) can rebuild the previous
// record.
func Converged(dataDir, project, environment, service string, record Record) error {
	if current, err := ReadRecord(dataDir, project, environment, service); err == nil && current != nil && current.Converging {
		record.Previous = current.Previous
	}
	return WriteRecord(dataDir, project, environment, service, record)
}

// RestoreRecord rebuilds the record after a failed rollout restored the
// previous containers: they become the converged containers again under
// the image and revision the record remembered from before the rollout.
// When nothing was remembered, or a restored container was not part of the
// previous record, the record is cleared so self-healing ignores the
// service until its next successful rollout.
func RestoreRecord(dataDir, project, environment, service string, restored []RecordContainer, now time.Time) error {
	current, err := ReadRecord(dataDir, project, environment, service)
	if err != nil || current == nil || current.Previous == nil || current.Previous.Image == "" || len(restored) == 0 {
		return ClearRecord(dataDir, project, environment, service)
	}
	previous := &Record{Containers: current.Previous.Containers}
	containers := make([]RecordContainer, 0, len(restored))
	for _, container := range restored {
		// Retention lists containers by their short ID (`docker ps -q`).
		full, ok := previous.matchID(container.ID)
		if !ok {
			return ClearRecord(dataDir, project, environment, service)
		}
		containers = append(containers, RecordContainer{Name: container.Name, ID: full})
	}
	return WriteRecord(dataDir, project, environment, service, Record{
		Image:      current.Previous.Image,
		Revision:   current.Previous.Revision,
		Containers: containers,
		Restart:    current.Previous.Restart,
		RecordedAt: now,
	})
}

// ClearRecord removes the service's record; a missing record is not an
// error.
func ClearRecord(dataDir, project, environment, service string) error {
	path, err := RecordPath(dataDir, project, environment, service)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
