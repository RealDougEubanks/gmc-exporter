// Package sink defines the interface every telemetry backend implements, and
// the fan-out that publishes one reading to all of them.
package sink

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/RealDougEubanks/gmc-exporter/internal/reading"
)

// Sink publishes readings to one backend.
//
// Implementations must treat every failure as recoverable and return an error
// rather than terminating the process. Nothing a sink does may prevent the poll
// loop from continuing or another sink from publishing.
type Sink interface {
	// Name identifies the sink in logs and metrics. It must be stable, since
	// it becomes a metric label.
	Name() string

	// Publish sends one reading. It must honour ctx cancellation and must not
	// retain the reading after returning.
	Publish(ctx context.Context, r reading.Reading) error

	// Close releases resources. It must be safe to call on a sink that never
	// successfully published.
	Close() error
}

// ErrSkipped is returned by a sink that had nothing to send for this reading,
// for example a sink that requires a temperature the device did not supply. It
// is not a failure and is not counted as one.
var ErrSkipped = errors.New("sink: reading skipped")

// Observer receives the outcome of each publish so metrics can be recorded
// without the fan-out depending on a metrics implementation.
type Observer interface {
	ObservePublish(sinkName string, duration time.Duration, err error)
}

// Set is a group of sinks published to concurrently.
type Set struct {
	sinks    []Sink
	timeout  time.Duration
	log      *slog.Logger
	observer Observer
}

// NewSet groups sinks. The timeout bounds each individual sink's publish, so
// one slow backend cannot delay the others or the next poll.
func NewSet(sinks []Sink, timeout time.Duration, log *slog.Logger, observer Observer) *Set {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &Set{sinks: sinks, timeout: timeout, log: log, observer: observer}
}

// Names lists the configured sinks.
func (s *Set) Names() []string {
	out := make([]string, 0, len(s.sinks))
	for _, sk := range s.sinks {
		out = append(out, sk.Name())
	}
	return out
}

// Len reports how many sinks are configured.
func (s *Set) Len() int { return len(s.sinks) }

// Publish sends the reading to every sink concurrently and waits for all of
// them.
//
// Each sink gets its own timeout and its own error handling. A failing sink is
// logged and counted; it never affects the others and never propagates upward
// as a reason to stop. Publish deliberately returns no error: there is no
// caller decision to make, because the correct response to a failed publish is
// always to carry on to the next poll.
//
// A panic inside a sink is recovered here. A third-party client library that
// panics on a malformed server response must not be able to kill a process
// whose entire purpose is to keep running unattended.
func (s *Set) Publish(ctx context.Context, r reading.Reading) {
	if len(s.sinks) == 0 {
		return
	}

	var wg sync.WaitGroup
	for _, sk := range s.sinks {
		wg.Add(1)
		go func(sk Sink) {
			defer wg.Done()
			s.publishOne(ctx, sk, r)
		}(sk)
	}
	wg.Wait()
}

// publishOne publishes to a single sink, bounding it and containing its
// failures.
func (s *Set) publishOne(ctx context.Context, sk Sink, r reading.Reading) {
	start := time.Now()

	err := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("sink %s panicked: %v", sk.Name(), p)
			}
		}()

		sinkCtx, cancel := context.WithTimeout(ctx, s.timeout)
		defer cancel()
		return sk.Publish(sinkCtx, r)
	}()

	duration := time.Since(start)
	if s.observer != nil {
		s.observer.ObservePublish(sk.Name(), duration, err)
	}

	switch {
	case err == nil:
		s.log.Debug("published reading", "sink", sk.Name(), "duration", duration)
	case errors.Is(err, ErrSkipped):
		s.log.Debug("sink skipped reading", "sink", sk.Name(), "reason", err)
	default:
		// The error is logged and dropped here on purpose. Every sink error
		// is recoverable by definition, and the next poll will try again.
		s.log.Error("publish failed", "sink", sk.Name(), "duration", duration, "error", err)
	}
}

// Close closes every sink, collecting rather than short-circuiting on errors so
// one stubborn sink cannot prevent the rest from shutting down cleanly.
func (s *Set) Close() error {
	var errs []error
	for _, sk := range s.sinks {
		if err := sk.Close(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", sk.Name(), err))
		}
	}
	return errors.Join(errs...)
}
