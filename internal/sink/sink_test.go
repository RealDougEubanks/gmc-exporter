package sink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/RealDougEubanks/gmc-exporter/internal/reading"
)

// discardLogger keeps the fan-out's own error logging out of the test output;
// a failing sink is the expected condition in most of these tests.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeSink is a scriptable sink. Every behaviour the fan-out is required to
// contain — an error, a panic, an indefinite block — is expressed here.
type fakeSink struct {
	name string

	// publish, when set, replaces the default success behaviour.
	publish func(ctx context.Context, r reading.Reading) error
	// closeErr is returned by Close.
	closeErr error

	mu       sync.Mutex
	received []reading.Reading
	closed   int
}

func (f *fakeSink) Name() string { return f.name }

func (f *fakeSink) Publish(ctx context.Context, r reading.Reading) error {
	f.mu.Lock()
	f.received = append(f.received, r)
	f.mu.Unlock()
	if f.publish != nil {
		return f.publish(ctx, r)
	}
	return nil
}

func (f *fakeSink) Close() error {
	f.mu.Lock()
	f.closed++
	f.mu.Unlock()
	return f.closeErr
}

func (f *fakeSink) publishCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.received)
}

func (f *fakeSink) lastReading() (reading.Reading, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.received) == 0 {
		return reading.Reading{}, false
	}
	return f.received[len(f.received)-1], true
}

func (f *fakeSink) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// observation is one recorded ObservePublish call.
type observation struct {
	name     string
	duration time.Duration
	err      error
}

// recordingObserver collects observations from the concurrent fan-out.
type recordingObserver struct {
	mu   sync.Mutex
	seen []observation
}

func (o *recordingObserver) ObservePublish(sinkName string, duration time.Duration, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, observation{name: sinkName, duration: duration, err: err})
}

func (o *recordingObserver) all() []observation {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]observation, len(o.seen))
	copy(out, o.seen)
	return out
}

func (o *recordingObserver) byName(name string) (observation, bool) {
	for _, obs := range o.all() {
		if obs.name == name {
			return obs, true
		}
	}
	return observation{}, false
}

func sampleReading() reading.Reading {
	return reading.Reading{
		Timestamp:            time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		CPM:                  28,
		AverageCPM:           27.5,
		MicroSievertsPerHour: 0.18,
		Voltage:              reading.Some(4.2),
		TemperatureC:         reading.None[float64](),
	}
}

func TestNewSetDefaults(t *testing.T) {
	// A non-positive timeout must become a real bound, not an unbounded
	// publish, and a nil logger must not panic on first use.
	s := NewSet([]Sink{&fakeSink{name: "a"}}, 0, nil, nil)
	if s.timeout != 10*time.Second {
		t.Fatalf("timeout with a zero option = %s, want 10s", s.timeout)
	}
	if s.log == nil {
		t.Fatal("logger is nil; the fan-out would panic on its first log call")
	}
	s.Publish(context.Background(), sampleReading())
}

func TestSetNamesAndLen(t *testing.T) {
	s := NewSet([]Sink{&fakeSink{name: "alpha"}, &fakeSink{name: "beta"}}, time.Second, discardLogger(), nil)
	if got := s.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
	names := s.Names()
	if len(names) != 2 || names[0] != "alpha" || names[1] != "beta" {
		t.Fatalf("Names = %v, want [alpha beta]", names)
	}
}

func TestPublishWithNoSinksIsNoOp(t *testing.T) {
	obs := &recordingObserver{}
	s := NewSet(nil, time.Second, discardLogger(), obs)
	if got := s.Len(); got != 0 {
		t.Fatalf("Len = %d, want 0", got)
	}
	s.Publish(context.Background(), sampleReading())
	if got := len(obs.all()); got != 0 {
		t.Fatalf("observations with no sinks = %d, want 0", got)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close with no sinks = %v, want nil", err)
	}
}

func TestPublishReachesEverySink(t *testing.T) {
	a := &fakeSink{name: "a"}
	b := &fakeSink{name: "b"}
	c := &fakeSink{name: "c"}
	s := NewSet([]Sink{a, b, c}, time.Second, discardLogger(), nil)

	want := sampleReading()
	s.Publish(context.Background(), want)

	for _, f := range []*fakeSink{a, b, c} {
		got, ok := f.lastReading()
		if !ok {
			t.Fatalf("sink %s received no reading", f.name)
		}
		if got.CPM != want.CPM || !got.Timestamp.Equal(want.Timestamp) {
			t.Fatalf("sink %s received %+v, want %+v", f.name, got, want)
		}
	}
}

// TestFailingSinkDoesNotBlockOthers is the core reliability guarantee: one
// broken backend must not cost the others their reading.
func TestFailingSinkDoesNotBlockOthers(t *testing.T) {
	boom := errors.New("backend refused the write")
	failing := &fakeSink{name: "failing", publish: func(context.Context, reading.Reading) error {
		return boom
	}}
	good1 := &fakeSink{name: "good1"}
	good2 := &fakeSink{name: "good2"}

	obs := &recordingObserver{}
	s := NewSet([]Sink{failing, good1, good2}, time.Second, discardLogger(), obs)
	s.Publish(context.Background(), sampleReading())

	for _, f := range []*fakeSink{good1, good2} {
		if f.publishCount() != 1 {
			t.Fatalf("sink %s publish count = %d, want 1", f.name, f.publishCount())
		}
	}

	got, ok := obs.byName("failing")
	if !ok {
		t.Fatal("no observation recorded for the failing sink")
	}
	if !errors.Is(got.err, boom) {
		t.Fatalf("observed error = %v, want %v", got.err, boom)
	}
}

// TestPanickingSinkIsContained covers reliability requirement 3: a third-party
// client library that panics must not take down a process whose whole job is to
// keep running unattended, and must not cost the other sinks their reading.
func TestPanickingSinkIsContained(t *testing.T) {
	panicking := &fakeSink{name: "panicking", publish: func(context.Context, reading.Reading) error {
		panic("client library exploded")
	}}
	good1 := &fakeSink{name: "good1"}
	good2 := &fakeSink{name: "good2"}

	obs := &recordingObserver{}
	s := NewSet([]Sink{panicking, good1, good2}, time.Second, discardLogger(), obs)
	s.Publish(context.Background(), sampleReading())

	for _, f := range []*fakeSink{good1, good2} {
		if f.publishCount() != 1 {
			t.Fatalf("sink %s publish count = %d, want 1 after another sink panicked", f.name, f.publishCount())
		}
	}

	got, ok := obs.byName("panicking")
	if !ok {
		t.Fatal("no observation recorded for the panicking sink")
	}
	if got.err == nil {
		t.Fatal("panic was swallowed without being reported as an error")
	}
	if want := "sink panicking panicked: client library exploded"; got.err.Error() != want {
		t.Fatalf("observed error = %q, want %q", got.err.Error(), want)
	}
}

// TestSlowSinkIsBoundedByTimeout asserts both halves of the guarantee: the slow
// sink's context is cancelled at the timeout, and the fast sinks are not made to
// wait for it any longer than that.
func TestSlowSinkIsBoundedByTimeout(t *testing.T) {
	const timeout = 100 * time.Millisecond
	const blockFor = 10 * time.Second

	released := make(chan struct{})
	slow := &fakeSink{name: "slow", publish: func(ctx context.Context, _ reading.Reading) error {
		select {
		case <-ctx.Done():
			close(released)
			return ctx.Err()
		case <-time.After(blockFor):
			return nil
		}
	}}
	fast := &fakeSink{name: "fast"}

	obs := &recordingObserver{}
	s := NewSet([]Sink{slow, fast}, timeout, discardLogger(), obs)

	start := time.Now()
	s.Publish(context.Background(), sampleReading())
	elapsed := time.Since(start)

	if elapsed >= blockFor/2 {
		t.Fatalf("Publish took %s; the slow sink was not bounded by the %s timeout", elapsed, timeout)
	}

	select {
	case <-released:
	default:
		t.Fatal("the slow sink's context was never cancelled")
	}

	if fast.publishCount() != 1 {
		t.Fatalf("fast sink publish count = %d, want 1", fast.publishCount())
	}

	slowObs, ok := obs.byName("slow")
	if !ok {
		t.Fatal("no observation recorded for the slow sink")
	}
	if !errors.Is(slowObs.err, context.DeadlineExceeded) {
		t.Fatalf("slow sink error = %v, want context.DeadlineExceeded", slowObs.err)
	}
	if slowObs.duration < timeout {
		t.Fatalf("slow sink duration = %s, want at least the %s timeout", slowObs.duration, timeout)
	}
}

// TestErrSkippedIsNotAFailure confirms the skip path is distinguishable from a
// failure, which is what stops "no temperature on this model" from looking like
// a broken backend.
func TestErrSkippedIsNotAFailure(t *testing.T) {
	skipping := &fakeSink{name: "skipping", publish: func(context.Context, reading.Reading) error {
		return fmt.Errorf("no temperature available: %w", ErrSkipped)
	}}
	obs := &recordingObserver{}
	s := NewSet([]Sink{skipping}, time.Second, discardLogger(), obs)
	s.Publish(context.Background(), sampleReading())

	got, ok := obs.byName("skipping")
	if !ok {
		t.Fatal("no observation recorded for the skipping sink")
	}
	if !errors.Is(got.err, ErrSkipped) {
		t.Fatalf("observed error = %v, want it to wrap ErrSkipped", got.err)
	}
}

func TestObserverSeesOneCallPerSink(t *testing.T) {
	fail := errors.New("nope")
	sinks := []Sink{
		&fakeSink{name: "ok"},
		&fakeSink{name: "slowish", publish: func(context.Context, reading.Reading) error {
			time.Sleep(20 * time.Millisecond)
			return nil
		}},
		&fakeSink{name: "broken", publish: func(context.Context, reading.Reading) error { return fail }},
	}

	obs := &recordingObserver{}
	s := NewSet(sinks, time.Second, discardLogger(), obs)
	s.Publish(context.Background(), sampleReading())

	all := obs.all()
	if len(all) != len(sinks) {
		t.Fatalf("observation count = %d, want %d", len(all), len(sinks))
	}

	seen := map[string]observation{}
	for _, o := range all {
		if _, dup := seen[o.name]; dup {
			t.Fatalf("sink %s was observed more than once", o.name)
		}
		seen[o.name] = o
		if o.duration < 0 {
			t.Fatalf("sink %s reported a negative duration %s", o.name, o.duration)
		}
	}

	if err := seen["ok"].err; err != nil {
		t.Fatalf("successful sink observed with error %v", err)
	}
	if err := seen["broken"].err; !errors.Is(err, fail) {
		t.Fatalf("failing sink observed with error %v, want %v", err, fail)
	}
	if d := seen["slowish"].duration; d < 20*time.Millisecond {
		t.Fatalf("slow sink duration = %s, want at least 20ms", d)
	}
}

// TestPublishSurvivesEverySinkMisbehaving documents the API contract: Publish
// has no error return, and no sink behaviour may escape it.
func TestPublishSurvivesEverySinkMisbehaving(t *testing.T) {
	sinks := []Sink{
		&fakeSink{name: "panics", publish: func(context.Context, reading.Reading) error { panic("boom") }},
		&fakeSink{name: "nil panic", publish: func(context.Context, reading.Reading) error { panic(nil) }},
		&fakeSink{name: "errors", publish: func(context.Context, reading.Reading) error {
			return errors.New("bad")
		}},
	}
	s := NewSet(sinks, 50*time.Millisecond, discardLogger(), nil)

	// A panic escaping the fan-out would fail the test by crashing it.
	s.Publish(context.Background(), sampleReading())
}

// TestPublishHonoursCallerCancellation checks that an already-cancelled parent
// context short-circuits the sinks rather than waiting out the per-sink timeout.
func TestPublishHonoursCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sk := &fakeSink{name: "ctx", publish: func(ctx context.Context, _ reading.Reading) error {
		return ctx.Err()
	}}
	obs := &recordingObserver{}
	s := NewSet([]Sink{sk}, time.Minute, discardLogger(), obs)
	s.Publish(ctx, sampleReading())

	got, ok := obs.byName("ctx")
	if !ok {
		t.Fatal("no observation recorded")
	}
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", got.err)
	}
}

// TestCloseCollectsAllErrors proves Close does not stop at the first failure:
// every sink must get the chance to release its resources at shutdown.
func TestCloseCollectsAllErrors(t *testing.T) {
	first := errors.New("first failed")
	third := errors.New("third failed")
	a := &fakeSink{name: "a", closeErr: first}
	b := &fakeSink{name: "b"}
	c := &fakeSink{name: "c", closeErr: third}

	s := NewSet([]Sink{a, b, c}, time.Second, discardLogger(), nil)
	err := s.Close()
	if err == nil {
		t.Fatal("Close returned nil, want the collected errors")
	}
	if !errors.Is(err, first) || !errors.Is(err, third) {
		t.Fatalf("Close error = %v, want it to carry both %v and %v", err, first, third)
	}

	for _, f := range []*fakeSink{a, b, c} {
		if got := f.closeCount(); got != 1 {
			t.Fatalf("sink %s Close called %d times, want 1", f.name, got)
		}
	}
}

func TestCloseWithNoErrorsReturnsNil(t *testing.T) {
	a := &fakeSink{name: "a"}
	b := &fakeSink{name: "b"}
	s := NewSet([]Sink{a, b}, time.Second, discardLogger(), nil)
	if err := s.Close(); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
}

// TestConcurrentPublishIsRaceFree hammers the fan-out the way the poll loop and
// a slow backend would, so -race can catch shared-state mistakes.
func TestConcurrentPublishIsRaceFree(t *testing.T) {
	sinks := []Sink{
		&fakeSink{name: "a"},
		&fakeSink{name: "b", publish: func(context.Context, reading.Reading) error {
			return errors.New("flaky")
		}},
		&fakeSink{name: "c", publish: func(context.Context, reading.Reading) error { panic("boom") }},
	}
	obs := &recordingObserver{}
	s := NewSet(sinks, time.Second, discardLogger(), obs)

	var wg sync.WaitGroup
	const rounds = 25
	for i := 0; i < rounds; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Publish(context.Background(), sampleReading())
			_ = s.Names()
			_ = s.Len()
		}()
	}
	wg.Wait()

	if got := len(obs.all()); got != rounds*len(sinks) {
		t.Fatalf("observation count = %d, want %d", got, rounds*len(sinks))
	}
}
