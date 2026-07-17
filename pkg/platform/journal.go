package platform

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

type OperationRecord struct {
	OperationID string         `json:"operationId"`
	Operation   string         `json:"operation"`
	Phase       string         `json:"phase"`
	Status      string         `json:"status"`
	NodeID      string         `json:"nodeId,omitempty"`
	Message     string         `json:"message,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	Timestamp   time.Time      `json:"timestamp"`
}

type Journal struct {
	path string
	mu   sync.Mutex
}

const maxJournalFrameBytes int64 = 2 << 20

func NewJournal(path string) (*Journal, error) {
	if path == "" {
		return nil, fmt.Errorf("journal path is required")
	}
	return &Journal{path: path}, nil
}

func (j *Journal) Append(record OperationRecord) error {
	if j == nil {
		return fmt.Errorf("journal is not initialized")
	}
	if record.OperationID == "" || record.Operation == "" || record.Phase == "" || record.Status == "" || record.Timestamp.IsZero() {
		return fmt.Errorf("journal record is incomplete")
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if int64(len(data)+1) > maxJournalFrameBytes {
		return fmt.Errorf("journal record exceeds %d-byte frame limit", maxJournalFrameBytes)
	}
	data = append(data, '\n')
	j.mu.Lock()
	defer j.mu.Unlock()
	file, err := openJournalAppend(j.path)
	if err != nil {
		return fmt.Errorf("open operation journal: %w", err)
	}
	defer file.Close()
	unlock, err := lockJournalFile(file)
	if err != nil {
		return fmt.Errorf("lock operation journal: %w", err)
	}
	defer unlock()
	if err := repairTornJournalTail(file); err != nil {
		return fmt.Errorf("repair operation journal tail: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("append operation journal: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync operation journal: %w", err)
	}
	return nil
}

// Each newline is the durable frame boundary. A process crash can leave only
// the final frame torn; truncate that suffix before publishing another frame.
func repairTornJournalTail(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return nil
	}
	last := []byte{0}
	if _, err := file.ReadAt(last, info.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	const chunkSize int64 = 64 << 10
	floor := info.Size() - maxJournalFrameBytes
	if floor < 0 {
		floor = 0
	}
	end := info.Size()
	for end > floor {
		start := end - chunkSize
		if start < floor {
			start = floor
		}
		chunk := make([]byte, end-start)
		if _, err := file.ReadAt(chunk, start); err != nil {
			return err
		}
		if index := bytes.LastIndexByte(chunk, '\n'); index >= 0 {
			if err := file.Truncate(start + int64(index) + 1); err != nil {
				return err
			}
			return file.Sync()
		}
		end = start
	}
	if floor > 0 {
		return fmt.Errorf("journal has no frame boundary within the %d-byte recovery limit", maxJournalFrameBytes)
	}
	if err := file.Truncate(0); err != nil {
		return err
	}
	return file.Sync()
}
