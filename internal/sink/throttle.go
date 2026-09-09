package sink

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/RealDougEubanks/gmc-exporter/internal/reading"
)

// throttled wraps a sink so it publishes at most once per interval, regardless
// of how often the poll loop runs.
//
// This exists because the useful poll interval and the appropriate publish
// interval are not the same number. Reading every 60 seconds gives Prometheus
// and InfluxDB the resolution that makes local graphs worth looking at. Sending
// every one of those readings to a permanent public archive is a different
// proposition: Safecast is built around mobile survey data, where dense
// sampling maps a route, and a fixed sensor posting every minute contributes
// half a million near-identical rows a year that cannot be deleted.
//
// Throttling belongs here rather than in each sink because it is the same
// logic every time, and because a sink should not have to know how often the
// poll loop happens to run.
type throttled struct {
	inner    Sink
	interval time.Duration

	mu   sync.Mutex
	last time.Time
}

// Throttle limits how often a sink publishes. A non-positive interval returns
// the sink unchanged, so the wrapper disappears entirely when unused.
func Throttle(s Sink, interval time.Duration) Sink {
	if interval <= 0 {
		return s
	}
	return &throttled{inner: s, interval: interval}
}

// Name passes through, so metric labels and log fields are unaffected by
// whether a sink happens to be throttled.
func (t *throttled) Name() string { return t.inner.Name() }

// Publish forwards the reading only if the interval has elapsed since the last
// successful publish.
//
// Only successes reset the clock. Throttling a failed publish would compound
// two problems: the reading was already lost, and the retry would then be
// delayed by the full interval. A failure should be retried on the next poll.
func (t *throttled) Publish(ctx context.Context, r reading.Reading) error {
	t.mu.Lock()
	last := t.last
	t.mu.Unlock()

	if !last.IsZero() {
		if elapsed := time.Since(last); elapsed < t.interval {
			return fmt.Errorf("%w: throttled, next publish in %s",
				ErrSkipped, (t.interval - elapsed).Round(time.Second))
		}
	}

	if err := t.inner.Publish(ctx, r); err != nil {
		return err
	}

	t.mu.Lock()
	t.last = time.Now()
	t.mu.Unlock()
	return nil
}

// Close passes through to the wrapped sink.
func (t *throttled) Close() error { return t.inner.Close() }
