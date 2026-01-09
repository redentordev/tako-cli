package logs

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"golang.org/x/sync/errgroup"
)

// LogLevel represents log severity
type LogLevel string

const (
	LevelDebug LogLevel = "DEBUG"
	LevelInfo  LogLevel = "INFO"
	LevelWarn  LogLevel = "WARN"
	LevelError LogLevel = "ERROR"
	LevelFatal LogLevel = "FATAL"
)

// AggregatedLog represents a log entry from any source
type AggregatedLog struct {
	Timestamp   time.Time         `json:"timestamp"`
	Level       LogLevel          `json:"level"`
	Service     string            `json:"service"`
	Server      string            `json:"server"`
	Container   string            `json:"container,omitempty"`
	Message     string            `json:"message"`
	Source      string            `json:"source"` // "container", "access", "system"
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// LogQuery defines search/filter criteria
type LogQuery struct {
	Services    []string
	Servers     []string
	Levels      []LogLevel
	Since       time.Time
	Until       time.Time
	Contains    string
	Regex       string
	Limit       int
}

// LogAggregator collects and aggregates logs from multiple sources
type LogAggregator struct {
	config      *config.Config
	sshPool     *ssh.Pool
	environment string
	verbose     bool

	// Local storage
	storagePath string
	
	// Streaming
	mu          sync.RWMutex
	subscribers map[string]chan AggregatedLog
}

// NewLogAggregator creates a new log aggregator
func NewLogAggregator(cfg *config.Config, sshPool *ssh.Pool, environment string, verbose bool) *LogAggregator {
	homeDir, _ := os.UserHomeDir()
	storagePath := filepath.Join(homeDir, ".tako", "logs", cfg.Project.Name, environment)

	return &LogAggregator{
		config:      cfg,
		sshPool:     sshPool,
		environment: environment,
		verbose:     verbose,
		storagePath: storagePath,
		subscribers: make(map[string]chan AggregatedLog),
	}
}

// Query searches logs based on criteria
func (a *LogAggregator) Query(ctx context.Context, query LogQuery) ([]AggregatedLog, error) {
	var allLogs []AggregatedLog
	var mu sync.Mutex

	servers, err := a.config.GetEnvironmentServers(a.environment)
	if err != nil {
		return nil, err
	}

	// Filter servers if specified
	if len(query.Servers) > 0 {
		var filtered []string
		for _, s := range servers {
			for _, qs := range query.Servers {
				if s == qs {
					filtered = append(filtered, s)
					break
				}
			}
		}
		servers = filtered
	}

	// Use errgroup for structured concurrency with limited parallelism
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(5) // Limit concurrent SSH connections

	// Query each server in parallel
	for _, serverName := range servers {
		serverCfg := a.config.Servers[serverName]
		name := serverName // Capture for goroutine
		cfg := serverCfg

		g.Go(func() error {
			client, err := a.sshPool.GetOrCreateWithAuth(cfg.Host, cfg.Port, cfg.User, cfg.SSHKey, cfg.Password)
			if err != nil {
				if a.verbose {
					fmt.Printf("  ⚠ Failed to connect to %s: %v\n", name, err)
				}
				return nil // Don't fail the whole query for one server
			}

			logs, err := a.queryServer(ctx, client, name, query)
			if err != nil {
				if a.verbose {
					fmt.Printf("  ⚠ Failed to query %s: %v\n", name, err)
				}
				return nil // Don't fail the whole query for one server
			}

			mu.Lock()
			allLogs = append(allLogs, logs...)
			mu.Unlock()
			return nil
		})
	}

	// Wait for all queries to complete
	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Sort by timestamp (newest first)
	sortLogsByTime(allLogs)

	// Apply limit
	if query.Limit > 0 && len(allLogs) > query.Limit {
		allLogs = allLogs[:query.Limit]
	}

	return allLogs, nil
}

// queryServer queries logs from a specific server
func (a *LogAggregator) queryServer(ctx context.Context, client *ssh.Client, serverName string, query LogQuery) ([]AggregatedLog, error) {
	var logs []AggregatedLog

	// Build service filter
	serviceFilter := ""
	if len(query.Services) > 0 {
		for _, svc := range query.Services {
			fullName := fmt.Sprintf("%s_%s_%s", a.config.Project.Name, a.environment, svc)
			serviceFilter += fmt.Sprintf(" --filter name=%s", fullName)
		}
	} else {
		serviceFilter = fmt.Sprintf(" --filter name=%s_%s_", a.config.Project.Name, a.environment)
	}

	// Build time filter
	timeArgs := ""
	if !query.Since.IsZero() {
		timeArgs += fmt.Sprintf(" --since %s", query.Since.Format(time.RFC3339))
	}
	if !query.Until.IsZero() {
		timeArgs += fmt.Sprintf(" --until %s", query.Until.Format(time.RFC3339))
	}

	// Get container logs
	cmd := fmt.Sprintf("docker service ls --format '{{.Name}}'%s", serviceFilter)
	output, err := client.Execute(cmd)
	if err != nil {
		return nil, err
	}

	for _, serviceName := range strings.Split(output, "\n") {
		serviceName = strings.TrimSpace(serviceName)
		if serviceName == "" {
			continue
		}

		// Get logs for this service
		logCmd := fmt.Sprintf("docker service logs %s --timestamps --tail 100%s 2>&1", serviceName, timeArgs)
		logOutput, err := client.Execute(logCmd)
		if err != nil {
			continue
		}

		// Parse logs
		for _, line := range strings.Split(logOutput, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}

			log := a.parseLine(line, serviceName, serverName)
			if log == nil {
				continue
			}

			// Apply filters
			if query.Contains != "" && !strings.Contains(strings.ToLower(log.Message), strings.ToLower(query.Contains)) {
				continue
			}

			if len(query.Levels) > 0 {
				found := false
				for _, l := range query.Levels {
					if log.Level == l {
						found = true
						break
					}
				}
				if !found {
					continue
				}
			}

			logs = append(logs, *log)
		}
	}

	return logs, nil
}

// parseLine parses a Docker log line
func (a *LogAggregator) parseLine(line, serviceName, serverName string) *AggregatedLog {
	// Docker service logs format: serviceName.1.taskId@nodeId | timestamp message
	// Or: 2024-01-01T12:00:00.000000000Z message

	parts := strings.SplitN(line, "|", 2)
	
	var container, message string
	var timestamp time.Time

	if len(parts) == 2 {
		container = strings.TrimSpace(parts[0])
		message = strings.TrimSpace(parts[1])
	} else {
		message = line
	}

	// Try to parse timestamp from message
	if len(message) > 30 && message[4] == '-' && message[7] == '-' {
		// Looks like ISO timestamp at start
		tsEnd := strings.Index(message, " ")
		if tsEnd > 0 {
			ts, err := time.Parse(time.RFC3339Nano, message[:tsEnd])
			if err == nil {
				timestamp = ts
				message = strings.TrimSpace(message[tsEnd:])
			}
		}
	}

	if timestamp.IsZero() {
		timestamp = time.Now()
	}

	// Extract service name from full name
	shortName := serviceName
	prefix := a.config.Project.Name + "_" + a.environment + "_"
	if strings.HasPrefix(serviceName, prefix) {
		shortName = strings.TrimPrefix(serviceName, prefix)
	}

	// Detect log level
	level := detectLogLevel(message)

	return &AggregatedLog{
		Timestamp: timestamp,
		Level:     level,
		Service:   shortName,
		Server:    serverName,
		Container: container,
		Message:   message,
		Source:    "container",
	}
}

// Stream starts streaming logs to a channel
func (a *LogAggregator) Stream(ctx context.Context, query LogQuery) (<-chan AggregatedLog, error) {
	ch := make(chan AggregatedLog, 100)

	// Register subscriber
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	a.mu.Lock()
	a.subscribers[id] = ch
	a.mu.Unlock()

	go func() {
		defer func() {
			a.mu.Lock()
			delete(a.subscribers, id)
			a.mu.Unlock()
			close(ch)
		}()

		servers, err := a.config.GetEnvironmentServers(a.environment)
		if err != nil {
			return
		}

		// Use errgroup for structured concurrency
		g, streamCtx := errgroup.WithContext(ctx)
		g.SetLimit(10) // Allow more concurrent streams

		for _, serverName := range servers {
			serverCfg := a.config.Servers[serverName]
			name := serverName // Capture for goroutine
			cfg := serverCfg

			g.Go(func() error {
				a.streamFromServer(streamCtx, name, cfg, query, ch)
				return nil
			})
		}

		g.Wait()
	}()

	return ch, nil
}

// streamFromServer streams logs from a single server
func (a *LogAggregator) streamFromServer(ctx context.Context, serverName string, cfg config.ServerConfig, query LogQuery, ch chan<- AggregatedLog) {
	client, err := a.sshPool.GetOrCreateWithAuth(cfg.Host, cfg.Port, cfg.User, cfg.SSHKey, cfg.Password)
	if err != nil {
		return
	}

	// Build service filter
	serviceFilter := fmt.Sprintf("%s_%s_", a.config.Project.Name, a.environment)
	if len(query.Services) > 0 {
		serviceFilter = fmt.Sprintf("%s_%s_%s", a.config.Project.Name, a.environment, query.Services[0])
	}

	// Stream logs using docker service logs -f
	cmd := fmt.Sprintf("docker service logs --follow --tail 10 $(docker service ls --filter name=%s --format '{{.Name}}' | head -1) 2>&1",
		serviceFilter)

	session, err := client.StartStream(cmd)
	if err != nil {
		return
	}
	defer session.Close()

	reader := bufio.NewReader(session.Stdout)
	for {
		select {
		case <-ctx.Done():
			return
		default:
			line, err := reader.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					return
				}
				time.Sleep(100 * time.Millisecond)
				continue
			}

			log := a.parseLine(strings.TrimSpace(line), serviceFilter, serverName)
			if log != nil {
				select {
				case ch <- *log:
				default:
					// Channel full, skip
				}
			}
		}
	}
}

// Export exports logs to a file
func (a *LogAggregator) Export(logs []AggregatedLog, format, outputPath string) error {
	file, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer file.Close()

	switch format {
	case "json":
		encoder := json.NewEncoder(file)
		encoder.SetIndent("", "  ")
		return encoder.Encode(logs)

	case "jsonl":
		encoder := json.NewEncoder(file)
		for _, log := range logs {
			if err := encoder.Encode(log); err != nil {
				return err
			}
		}
		return nil

	case "csv":
		file.WriteString("timestamp,level,service,server,message\n")
		for _, log := range logs {
			msg := strings.ReplaceAll(log.Message, "\"", "\"\"")
			file.WriteString(fmt.Sprintf("%s,%s,%s,%s,\"%s\"\n",
				log.Timestamp.Format(time.RFC3339),
				log.Level,
				log.Service,
				log.Server,
				msg))
		}
		return nil

	default:
		// Plain text
		for _, log := range logs {
			file.WriteString(fmt.Sprintf("[%s] %s %s@%s: %s\n",
				log.Timestamp.Format("2006-01-02 15:04:05"),
				log.Level,
				log.Service,
				log.Server,
				log.Message))
		}
		return nil
	}
}

// detectLogLevel tries to detect log level from message content
func detectLogLevel(message string) LogLevel {
	upper := strings.ToUpper(message)
	
	// Check for common log level patterns
	levelPatterns := map[LogLevel][]string{
		LevelFatal: {"FATAL", "PANIC", "CRITICAL"},
		LevelError: {"ERROR", "ERR ", "FAIL", "EXCEPTION"},
		LevelWarn:  {"WARN", "WARNING"},
		LevelDebug: {"DEBUG", "TRACE"},
	}

	for level, patterns := range levelPatterns {
		for _, pattern := range patterns {
			if strings.Contains(upper, pattern) {
				return level
			}
		}
	}

	return LevelInfo
}

// sortLogsByTime sorts logs by timestamp (newest first)
func sortLogsByTime(logs []AggregatedLog) {
	for i := 0; i < len(logs)-1; i++ {
		for j := i + 1; j < len(logs); j++ {
			if logs[i].Timestamp.Before(logs[j].Timestamp) {
				logs[i], logs[j] = logs[j], logs[i]
			}
		}
	}
}
