package sink

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RealDougEubanks/gmc-exporter/internal/reading"
)

// countingSink records how many times it was published to, and can be told to
// fail.
type countingSink struct {
	name    string
	calls   atomic.Int32
	closes  atomic.Int32
	failing atomic.Bool
}

func (c *countingSink) Name() string { return c.name }

func (c *countingSink) Publish(context.Context, reading.Reading) error {
	c.calls.Add(1)
	if c.failing.Load() {
		return errors.New("backend unavailable")
	}
	return nil
}

func (c *countingSink) Close() error {
	c.closes.Add(1)
	return nil
}

// TestThrottleZeroIntervalIsTransparent checks the wrapper disappears when it
// is not wanted, so an unthrottled sink carries no overhead or behaviour change.
func TestThrottleZeroIntervalIsTransparent(t *testing.T) {
	t.Parallel()

	inner := &countingSink{name: "plain"}
	for _, interval := range []time.Duration{0, -time.Second} {
		if got := Throttle(inner, interval); got != Sink(inner) {
			t.Errorf("Throttle(s, %s) wrapped the sink; it should return it unchanged", interval)
		}
	}
}

// TestThrottleLimitsPublishRate is the behaviour the setting exists for.
func TestThrottleLimitsPublishRate(t *testing.T) {
	t.Parallel()

	inner := &countingSink{name: "safecast"}
	s := Throttle(inner, 150*time.Millisecond)
	ctx := context.Background()

	// The first publish always goes through. Nothing has been sent yet, so
	// there is nothing to throttle against.
	if err := s.Publish(ctx, reading.Reading{CPM: 1}); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if n := inner.calls.Load(); n != 1 {
		t.Fatalf("inner called %d times, want 1", n)
	}

	// Subsequent publishes inside the interval are skipped, and skipped is
	// not the same as failed: the fan-out counts them separately so they do
	// not look like something an operator should act on.
	for i := 0; i < 5; i++ {
		err := s.Publish(ctx, reading.Reading{CPM: 2})
		if !errors.Is(err, ErrSkipped) {
			t.Fatalf("publish %d inside the interval returned %v, want ErrSkipped", i, err)
		}
	}
	if n := inner.calls.Load(); n != 1 {
		t.Fatalf("inner called %d times; the wrapped sink must not be reached while throttled", n)
	}

	// Once the interval has elapsed, publishing resumes.
	time.Sleep(200 * time.Millisecond)
	if err := s.Publish(ctx, reading.Reading{CPM: 3}); err != nil {
		t.Fatalf("publish after the interval: %v", err)
	}
	if n := inner.calls.Load(); n != 2 {
		t.Fatalf("inner called %d times, want 2", n)
	}
}

// TestThrottleDoesNotDelayRetryAfterFailure covers the case that makes the
// difference between throttling and dropping data.
//
// Only a successful publish resets the clock. If a failure reset it too, a
// single backend hiccup would cost the reading and then block the retry for the
// whole interval, turning one lost point into many.
func TestThrottleDoesNotDelayRetryAfterFailure(t *testing.T) {
	t.Parallel()

	inner := &countingSink{name: "influxv1"}
	inner.failing.Store(true)

	s := Throttle(inner, time.Hour) // deliberately long
	ctx := context.Background()

	// Three consecutive failures must all reach the sink, despite the hour
	// long interval, because none of them succeeded.
	for i := 0; i < 3; i++ {
		if err := s.Publish(ctx, reading.Reading{CPM: 10}); err == nil {
			t.Fatalf("publish %d should have failed", i)
		}
		if errors.Is(s.Publish(ctx, reading.Reading{CPM: 10}), ErrSkipped) {
			t.Fatalf("a failed publish must not start the throttle interval")
		}
	}

	// Now let it succeed. That starts the interval.
	inner.failing.Store(false)
	if err := s.Publish(ctx, reading.Reading{CPM: 10}); err != nil {
		t.Fatalf("publish after recovery: %v", err)
	}
	before := inner.calls.Load()

	// And the next one is throttled.
	if err := s.Publish(ctx, reading.Reading{CPM: 10}); !errors.Is(err, ErrSkipped) {
		t.Fatalf("got %v, want ErrSkipped once a publish has succeeded", err)
	}
	if after := inner.calls.Load(); after != before {
		t.Fatalf("inner was called again after a successful publish started the interval")
	}
}

// TestThrottlePassesThroughIdentityAndClose checks the wrapper is invisible to
// metrics and shutdown.
func TestThrottlePassesThroughIdentityAndClose(t *testing.T) {
	t.Parallel()

	inner := &countingSink{name: "gmcmap"}
	s := Throttle(inner, time.Minute)

	// The name becomes a metric label, so it must not change.
	if s.Name() != "gmcmap" {
		t.Errorf("Name() = %q, want %q", s.Name(), "gmcmap")
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close(): %v", err)
	}
	if n := inner.closes.Load(); n != 1 {
		t.Errorf("inner closed %d times, want 1", n)
	}
}

// TestThrottleIsConcurrencySafe runs under -race, because the fan-out publishes
// to every sink at once.
func TestThrottleIsConcurrencySafe(t *testing.T) {
	t.Parallel()

	inner := &countingSink{name: "mqtt"}
	s := Throttle(inner, 50*time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Errors are expected and irrelevant here; the point is that
			// concurrent access does not race or panic.
			_ = s.Publish(context.Background(), reading.Reading{CPM: 5})
		}()
	}
	wg.Wait()

	// With a 50ms interval and 40 near-simultaneous calls, only a small
	// number should have reached the sink.
	if n := inner.calls.Load(); n < 1 || n > 5 {
		t.Errorf("inner called %d times; expected a handful, not one per caller", n)
	}
}

// TestThrottledSinkCountsAsSkippedNotFailed checks the fan-out classifies a
// throttled publish correctly, so dashboards do not show it as an error.
func TestThrottledSinkCountsAsSkippedNotFailed(t *testing.T) {
	t.Parallel()

	inner := &countingSink{name: "safecast"}
	obs := &recordingObserver{}
	set := NewSet([]Sink{Throttle(inner, time.Hour)}, time.Second, nil, obs)

	set.Publish(context.Background(), reading.Reading{CPM: 1}) // succeeds
	set.Publish(context.Background(), reading.Reading{CPM: 2}) // throttled

	seen := obs.all()
	if len(seen) != 2 {
		t.Fatalf("observer saw %d publishes, want 2", len(seen))
	}
	if seen[0].err != nil {
		t.Errorf("first publish reported %v, want success", seen[0].err)
	}
	if !errors.Is(seen[1].err, ErrSkipped) {
		t.Errorf("throttled publish reported %v, want ErrSkipped", seen[1].err)
	}
}
