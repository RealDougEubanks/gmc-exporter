// Package httpserver exposes the exporter's metrics and health endpoints.
//
// Health is split three ways deliberately, because orchestrators and external
// monitors ask different questions:
//
//   - /healthz  liveness: is the process alive? Used to decide whether to
//     restart the container. It must not fail because a backend
//     is down, or a broker outage would cause a restart loop.
//   - /readyz   readiness: can this instance serve useful data right now?
//     Fails when the device is disconnected or readings have
//     gone stale.
//   - /health   detail: a human- and monitor-readable breakdown of every
//     dependency, returning 503 when unhealthy so external
//     monitors can alert on status code alone.
package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/RealDougEubanks/gmc-exporter/internal/config"
	"github.com/RealDougEubanks/gmc-exporter/internal/poller"
)

// StatusProvider supplies the current health snapshot.
type StatusProvider interface {
	Status() poller.Status
	SinceLastSuccess() (time.Duration, bool)
}

// BuildInfo identifies the running build.
type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Built   string `json:"built"`
}

// Server owns the HTTP listener.
type Server struct {
	cfg     config.HTTP
	stale   time.Duration
	status  StatusProvider
	build   BuildInfo
	sinks   []string
	log     *slog.Logger
	httpSrv *http.Server
}

// Options configures a Server.
type Options struct {
	Config config.HTTP
	// StaleAfter is how long without a successful read before readiness
	// fails.
	StaleAfter time.Duration
	Status     StatusProvider
	Build      BuildInfo
	// SinkNames is reported by /health so an operator can confirm which
	// backends this instance was configured with.
	SinkNames []string
	// MetricsHandler is the Prometheus handler, or nil when the Prometheus
	// sink is disabled.
	MetricsHandler http.Handler
	MetricsPath    string
	Log            *slog.Logger
}

// New builds the server and its routes.
func New(opts Options) *Server {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}

	s := &Server{
		cfg:    opts.Config,
		stale:  opts.StaleAfter,
		status: opts.Status,
		build:  opts.Build,
		sinks:  opts.SinkNames,
		log:    log,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleLiveness)
	mux.HandleFunc("/readyz", s.handleReadiness)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/", s.handleRoot)

	if opts.MetricsHandler != nil {
		path := opts.MetricsPath
		if path == "" {
			path = "/metrics"
		}
		mux.Handle(path, opts.MetricsHandler)
	}

	s.httpSrv = &http.Server{
		Addr:              opts.Config.Addr,
		Handler:           mux,
		ReadHeaderTimeout: opts.Config.ReadTimeout,
		ReadTimeout:       opts.Config.ReadTimeout,
	}
	return s
}

// Handler exposes the routes for testing without binding a port.
func (s *Server) Handler() http.Handler { return s.httpSrv.Handler }

// Run serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("http server listening", "addr", s.httpSrv.Addr)
		if err := s.httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http server: %w", err)
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
		defer cancel()
		if err := s.httpSrv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("http shutdown: %w", err)
		}
		return nil
	}
}

// handleLiveness answers whether the process is running.
//
// It deliberately checks nothing else. Liveness that depends on a backend turns
// a broker outage into a restart loop, which is strictly worse than the outage.
func (s *Server) handleLiveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadiness answers whether this instance is currently producing data.
func (s *Server) handleReadiness(w http.ResponseWriter, _ *http.Request) {
	ready, reason := s.readiness()
	code := http.StatusOK
	body := map[string]string{"status": "ready"}
	if !ready {
		code = http.StatusServiceUnavailable
		body = map[string]string{"status": "not ready", "reason": reason}
	}
	writeJSON(w, code, body)
}

// readiness evaluates whether readings are current.
func (s *Server) readiness() (bool, string) {
	if s.status == nil {
		return true, ""
	}
	st := s.status.Status()
	if !st.Connected {
		return false, "device is not connected"
	}
	age, ever := s.status.SinceLastSuccess()
	if !ever {
		return false, "no successful reading yet"
	}
	if s.stale > 0 && age > s.stale {
		return false, fmt.Sprintf("last successful reading was %s ago, threshold is %s",
			age.Round(time.Second), s.stale)
	}
	return true, ""
}

// dependency is one checked component in the /health response.
type dependency struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
	CheckedAt string `json:"checkedAt"`
	LatencyMS int64  `json:"latencyMs,omitempty"`
}

// healthResponse is the detailed health document.
//
// It reports which backends are configured but deliberately carries no URLs,
// hostnames, tokens or connection strings. This endpoint is unauthenticated so
// external monitors can reach it, which means it must not become a
// reconnaissance tool.
type healthResponse struct {
	Status       string       `json:"status"`
	Build        BuildInfo    `json:"build"`
	Device       deviceHealth `json:"device"`
	Sinks        []string     `json:"configuredSinks"`
	Dependencies []dependency `json:"dependencies"`
}

// deviceHealth describes the Geiger counter.
type deviceHealth struct {
	Connected       bool   `json:"connected"`
	Model           string `json:"model,omitempty"`
	Firmware        string `json:"firmware,omitempty"`
	Calibration     string `json:"calibration,omitempty"`
	LastReadingAgeS *int64 `json:"lastReadingAgeSeconds,omitempty"`
	LastError       string `json:"lastError,omitempty"`
}

// handleHealth reports the detail behind readiness.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().UTC().Format(time.RFC3339)

	resp := healthResponse{
		Build: s.build,
		Sinks: s.sinks,
	}

	if s.status != nil {
		start := time.Now()
		st := s.status.Status()
		resp.Device = deviceHealth{
			Connected:   st.Connected,
			Model:       st.Device.Model,
			Firmware:    st.Device.Firmware,
			Calibration: st.Calibration,
			LastError:   st.LastErrorText,
		}
		if age, ever := s.status.SinceLastSuccess(); ever {
			seconds := int64(age.Seconds())
			resp.Device.LastReadingAgeS = &seconds
		}

		dep := dependency{
			Name:      "geiger-counter",
			CheckedAt: now,
			LatencyMS: time.Since(start).Milliseconds(),
		}
		switch {
		case !st.Connected:
			dep.Status, dep.Detail = "fail", "serial port is not open"
		case resp.Device.LastReadingAgeS == nil:
			dep.Status, dep.Detail = "degraded", "no successful reading yet"
		case s.stale > 0 && time.Duration(*resp.Device.LastReadingAgeS)*time.Second > s.stale:
			dep.Status, dep.Detail = "degraded", "readings are stale"
		default:
			dep.Status = "ok"
		}
		resp.Dependencies = append(resp.Dependencies, dep)
	}

	ready, reason := s.readiness()
	code := http.StatusOK
	resp.Status = "ok"
	if !ready {
		code = http.StatusServiceUnavailable
		resp.Status = "unhealthy"
		if resp.Device.LastError == "" {
			resp.Device.LastError = reason
		}
	}

	writeJSON(w, code, resp)
}

// handleRoot serves a small index so someone who opens the port in a browser
// can find their way, and returns 404 for anything unrecognised.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":   "gmc-exporter",
		"build":     s.build,
		"endpoints": []string{"/metrics", "/healthz", "/readyz", "/health"},
	})
}

// writeJSON emits a JSON response, and never caches: every one of these
// endpoints reports a live, instance-specific state that must not be served
// from an intermediary.
func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already written, so this can only be logged.
		slog.Default().Debug("writing health response", "error", err)
	}
}
