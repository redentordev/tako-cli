package takod

// Server glue for add-server-status-history: native collection, history,
// node status, their routes and capabilities.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/redentordev/tako-cli/pkg/takod/hostmetrics"
)

type statusHistory struct {
	options   MetricsOptions
	collector *nodeMetricsCollector
	history   *metricsHistoryStore
	status    *nodeStatusManager
}

// initStatusHistory wires native collection (when the interval is > 0) and
// node status. replaceHostSamples is false when the caller injected its own
// HostSampleSource.
func (s *Server) initStatusHistory(opts MetricsOptions, replaceHostSamples bool) {
	sh := &statusHistory{options: opts}
	var collectorState nodeCollectorState
	if opts.Interval > 0 {
		meshInterfaces := append([]string{defaultMeshInterface}, opts.MeshInterfaces...)
		source := hostmetrics.NewSource(s.dockerDataRoot, meshInterfaces...)
		sh.history = newMetricsHistoryStore(filepath.Join(s.dataDir, "metrics", "history"), opts.Retention, opts.HistoryMaxBytes, opts.Interval)
		sh.collector = newNodeMetricsCollector(source, opts.Interval, sh.history)
		collector := sh.collector
		collectorState = func() (time.Time, bool, error, error, []string) {
			sample, ok := collector.Latest()
			readErr, historyErr := collector.Errors()
			return sample.Time, ok, readErr, historyErr, sample.Warnings
		}
		if replaceHostSamples {
			s.hostSamples = nativeHostSampleSource{collector: collector}
		}
	}
	sh.status = newNodeStatusManager(s.dataDir, func() string { return s.nodeName }, s.sourceAlerts, collectorState, opts.Interval)
	if err := s.sourceAlerts.RegisterSource(nodeAlertSourceSpec()); err != nil {
		panic(err)
	}
	if sh.collector != nil {
		sh.collector.onSample = func(sample hostmetrics.Sample) {
			sh.status.UpdateCollector()
			if opts.TextfileDir == "" {
				return
			}
			document := NativeMetricsDocument(sample, opts.Interval)
			status, _ := sh.status.Status()
			input := NodePrometheusInputFromDocument(s.nodeName, &document, status)
			if err := writeNodePrometheusTextfile(opts.TextfileDir, input); err != nil {
				fmt.Fprintf(os.Stderr, "takod: metrics textfile export failed: %v\n", err)
			}
		}
	}
	s.statusHistory = sh
}

// startStatusHistory starts the collector and the node status loop.
func (s *Server) startStatusHistory(ctx context.Context, start func(func())) {
	sh := s.statusHistory
	if sh == nil {
		return
	}
	sh.status.Load()
	if sh.collector != nil {
		start(func() { sh.collector.Run(ctx, nil) })
	}
	start(func() { sh.status.Run(ctx) })
}

// statusHistoryCapabilities lists node.status-v1 and, with native
// collection, node.metrics-history-v1.
func (s *Server) statusHistoryCapabilities() []string {
	if s.statusHistory == nil {
		return nil
	}
	if s.statusHistory.collector != nil {
		return []string{CapabilityNodeMetricsHistoryV1, CapabilityNodeStatusV1}
	}
	return []string{CapabilityNodeStatusV1}
}

// readMetricsResponse returns the /v1/metrics document: the native sample
// when collection is enabled, else the legacy agent file.
func (s *Server) readMetricsResponse(ctx context.Context, collect bool) (*MetricsResponse, error) {
	sh := s.statusHistory
	var response *MetricsResponse
	if sh == nil || sh.collector == nil {
		legacy, err := ReadNodeMetrics(ctx, collect)
		if err != nil {
			return nil, err
		}
		response = legacy
	} else {
		sample, ok := sh.collector.Latest()
		if collect || !ok {
			fresh, err := sh.collector.CollectNow(ctx)
			if err != nil {
				return nil, fmt.Errorf("failed to collect node metrics: %w", err)
			}
			sample = fresh
		}
		data, err := json.Marshal(NativeMetricsDocument(sample, sh.options.Interval))
		if err != nil {
			return nil, err
		}
		response = &MetricsResponse{Collected: collect, Metrics: data}
	}
	if sh != nil {
		if status, ok := sh.status.Status(); ok {
			response.Status = status
		}
	}
	return response, nil
}

func (s *Server) handleMetricsHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sh := s.statusHistory
	if sh == nil || sh.history == nil {
		http.Error(w, "native metrics collection is disabled (--metrics-interval 0)", http.StatusNotFound)
		return
	}
	query := r.URL.Query()
	response, err := sh.history.Query(MetricsHistoryQuery{Since: query.Get("since"), Until: query.Get("until"), Step: query.Get("step")}, time.Now())
	if err != nil {
		var requestErr *metricsHistoryRequestError
		if errors.As(err, &requestErr) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	response.Node = s.nodeName
	writeJSONResponse(w, response)
}

func (s *Server) handleNodeStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	response := &NodeStatusResponse{Node: s.nodeName, EvaluatedAt: time.Now().UTC(), Conditions: []NodeCondition{}}
	if s.statusHistory != nil {
		if status, ok := s.statusHistory.status.Status(); ok {
			response = status
		}
	}
	writeJSONResponse(w, response)
}

func writeJSONResponse(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}
