package streaming

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/ssh"
)

// StreamType represents different types of streams
type StreamType string

const (
	StreamMetrics StreamType = "metrics"
	StreamLogs    StreamType = "logs"
	StreamEvents  StreamType = "events"
	StreamDocker  StreamType = "docker"
)

// Message represents a streamed message
type Message struct {
	Type      StreamType      `json:"type"`
	Timestamp time.Time       `json:"timestamp"`
	Source    string          `json:"source"` // hostname or container name
	Data      json.RawMessage `json:"data"`
}

// Subscription represents a client subscription
type Subscription struct {
	ID       string
	Types    []StreamType
	Messages chan Message
	ctx      context.Context
	cancel   context.CancelFunc
}

// Server handles real-time streaming from remote servers
type Server struct {
	client        *ssh.Client
	hostname      string
	subscriptions map[string]*Subscription
	mu            sync.RWMutex
	ctx           context.Context
	cancel        context.CancelFunc
}

// NewServer creates a new streaming server
func NewServer(client *ssh.Client, hostname string) *Server {
	ctx, cancel := context.WithCancel(context.Background())

	return &Server{
		client:        client,
		hostname:      hostname,
		subscriptions: make(map[string]*Subscription),
		ctx:           ctx,
		cancel:        cancel,
	}
}

// Subscribe creates a new subscription for specific stream types
func (s *Server) Subscribe(types []StreamType) *Subscription {
	s.mu.Lock()
	defer s.mu.Unlock()

	ctx, cancel := context.WithCancel(s.ctx)

	sub := &Subscription{
		ID:       fmt.Sprintf("sub-%d", time.Now().UnixNano()),
		Types:    types,
		Messages: make(chan Message, 100), // Buffered channel
		ctx:      ctx,
		cancel:   cancel,
	}

	s.subscriptions[sub.ID] = sub

	return sub
}

// Unsubscribe removes a subscription
func (s *Server) Unsubscribe(subID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sub, exists := s.subscriptions[subID]; exists {
		sub.cancel()
		close(sub.Messages)
		delete(s.subscriptions, subID)
	}
}

// Start begins streaming from all sources
func (s *Server) Start() error {
	// Start metrics streaming
	go s.streamMetrics()

	// Start Docker events streaming
	go s.streamDockerEvents()

	// Start log aggregation
	go s.streamSystemLogs()

	return nil
}

// Stop stops all streaming
func (s *Server) Stop() {
	s.cancel()

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, sub := range s.subscriptions {
		sub.cancel()
		close(sub.Messages)
	}

	s.subscriptions = make(map[string]*Subscription)
}

// streamMetrics continuously streams system metrics
func (s *Server) streamMetrics() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			// Read metrics from agent
			output, err := s.client.Execute("cat /var/lib/tako/metrics/current.json 2>/dev/null || echo '{}'")
			if err != nil {
				continue
			}

			msg := Message{
				Type:      StreamMetrics,
				Timestamp: time.Now(),
				Source:    s.hostname,
				Data:      json.RawMessage(output),
			}

			s.broadcast(msg, StreamMetrics)
		}
	}
}

// streamDockerEvents streams Docker container events
func (s *Server) streamDockerEvents() {
	// Start Docker event stream
	cmd := "docker events --format '{{json .}}' --filter 'type=container' 2>/dev/null"

	session, err := s.client.StartStream(cmd)
	if err != nil {
		return
	}
	defer session.Close()

	scanner := NewLineScanner(session.Stdout)

	for {
		select {
		case <-s.ctx.Done():
			return
		default:
			line, err := scanner.ReadLine()
			if err != nil {
				if err == io.EOF {
					time.Sleep(1 * time.Second)
					continue
				}
				return
			}

			msg := Message{
				Type:      StreamDocker,
				Timestamp: time.Now(),
				Source:    s.hostname,
				Data:      json.RawMessage(line),
			}

			s.broadcast(msg, StreamDocker)
		}
	}
}

// streamSystemLogs streams system and application logs
func (s *Server) streamSystemLogs() {
	// Tail important log files
	cmd := "tail -F -n 0 /var/log/tako/agent.log /var/lib/tako/metrics/events.log 2>/dev/null"

	session, err := s.client.StartStream(cmd)
	if err != nil {
		return
	}
	defer session.Close()

	scanner := NewLineScanner(session.Stdout)

	for {
		select {
		case <-s.ctx.Done():
			return
		default:
			line, err := scanner.ReadLine()
			if err != nil {
				if err == io.EOF {
					time.Sleep(1 * time.Second)
					continue
				}
				return
			}

			logData := map[string]string{
				"message": line,
				"source":  s.hostname,
			}

			logJSON, _ := json.Marshal(logData)

			msg := Message{
				Type:      StreamLogs,
				Timestamp: time.Now(),
				Source:    s.hostname,
				Data:      json.RawMessage(logJSON),
			}

			s.broadcast(msg, StreamLogs)
		}
	}
}

// broadcast sends a message to all interested subscribers
func (s *Server) broadcast(msg Message, streamType StreamType) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, sub := range s.subscriptions {
		// Check if subscription is interested in this stream type
		interested := false
		for _, t := range sub.Types {
			if t == streamType {
				interested = true
				break
			}
		}

		if !interested {
			continue
		}

		// Non-blocking send
		select {
		case sub.Messages <- msg:
		default:
			// Channel full, drop message
		}
	}
}

// LineScanner is a helper for reading lines from a stream
type LineScanner struct {
	reader io.Reader
	buffer []byte
}

// NewLineScanner creates a new line scanner
func NewLineScanner(reader io.Reader) *LineScanner {
	return &LineScanner{
		reader: reader,
		buffer: make([]byte, 0, 4096),
	}
}

// ReadLine reads a line from the stream
func (ls *LineScanner) ReadLine() (string, error) {
	buf := make([]byte, 1024)

	for {
		n, err := ls.reader.Read(buf)
		if n > 0 {
			ls.buffer = append(ls.buffer, buf[:n]...)

			// Check for newline
			for i := 0; i < len(ls.buffer); i++ {
				if ls.buffer[i] == '\n' {
					line := string(ls.buffer[:i])
					ls.buffer = ls.buffer[i+1:]
					return line, nil
				}
			}
		}

		if err != nil {
			return "", err
		}
	}
}
