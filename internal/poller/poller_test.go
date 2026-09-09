package poller

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RealDougEubanks/gmc-exporter/internal/config"
	"github.com/RealDougEubanks/gmc-exporter/internal/reading"
	"github.com/RealDougEubanks/gmc-exporter/internal/serialport"
)

// realCaptureFile is the config-block capture taken from the attached GMC-320.
// The poller tests answer <GETCFG>> with those exact bytes so the calibration
// path is exercised against genuine device data rather than a hand-made block.
const realCaptureFile = "../gmc/testdata/captures/getcfg-100iter.json"

// Responses recorded from the real device, used to script the fake below.
const (
	realVersion = "474d432d333230526520342e3632" // "GMC-320Re 4.62"
	realSerial  = "f628c40009888c"
	realCPM     = "001c" // 28 CPM
	realVolt    = "2a"   // 4.2 V
	realTemp    = "1f0100aa"
)

// fakeDevice answers commands the way the real unit does, and can be told to
// misbehave in the ways the real unit was measured misbehaving.
type fakeDevice struct {
	mu sync.Mutex

	configBlock []byte
	pending     []byte

	// failCPMAfter makes <GETCPM>> start failing once this many have
	// succeeded, to exercise the skip-and-continue path.
	failCPMAfter int
	cpmCount     int

	// readErr, when set, is returned by every Read, simulating an adapter
	// that has been unplugged.
	readErr error

	// desyncCPM makes <GETCPM>> answer with the config block, reproducing the
	// desynchronisation measured on real hardware.
	desyncCPM bool

	closed bool
}

func (f *fakeDevice) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch cmd := string(p); cmd {
	case "<GETVER>>":
		f.pending = mustHex(realVersion)
	case "<GETSERIAL>>":
		f.pending = mustHex(realSerial)
	case "<GETCFG>>":
		f.pending = append([]byte(nil), f.configBlock...)
	case "<GETVOLT>>":
		f.pending = mustHex(realVolt)
	case "<GETTEMP>>":
		f.pending = mustHex(realTemp)
	case "<GETCPM>>":
		f.cpmCount++
		switch {
		case f.desyncCPM:
			f.pending = append([]byte(nil), f.configBlock...)
		case f.failCPMAfter > 0 && f.cpmCount > f.failCPMAfter:
			f.pending = nil
		default:
			f.pending = mustHex(realCPM)
		}
	default:
		f.pending = nil
	}
	return len(p), nil
}

// Read hands back the pending response in 32-byte pieces, which is how the
// CH340 bridge was measured delivering it.
func (f *fakeDevice) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.readErr != nil {
		return 0, f.readErr
	}
	if len(f.pending) == 0 {
		return 0, serialport.ErrTimeout
	}

	chunk := f.pending
	if len(chunk) > 32 {
		chunk = chunk[:32]
	}
	n := copy(p, chunk)
	f.pending = f.pending[n:]
	if n == 0 {
		return 0, serialport.ErrTimeout
	}
	return n, nil
}

func (f *fakeDevice) Drain() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = nil
	return nil
}

func (f *fakeDevice) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// loadRealConfigBlock reads a genuine 256-byte config block from the capture.
func loadRealConfigBlock(t *testing.T) []byte {
	t.Helper()

	raw, err := os.ReadFile(realCaptureFile)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	var run struct {
		Commands []struct {
			Name       string `json:"name"`
			Iterations []struct {
				Hex        string `json:"hex"`
				TotalBytes int    `json:"totalBytes"`
			} `json:"iterations"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(raw, &run); err != nil {
		t.Fatalf("decode capture: %v", err)
	}
	for _, c := range run.Commands {
		for _, it := range c.Iterations {
			if it.TotalBytes == 256 {
				return mustHex(it.Hex)
			}
		}
	}
	t.Fatal("capture contained no 256-byte config block")
	return nil
}

// recordingPublisher captures what the poller published.
type recordingPublisher struct {
	mu       sync.Mutex
	readings []reading.Reading
}

func (p *recordingPublisher) Publish(_ context.Context, r reading.Reading) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.readings = append(p.readings, r)
}

func (p *recordingPublisher) all() []reading.Reading {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]reading.Reading(nil), p.readings...)
}

// countingRecorder tallies poll outcomes.
type countingRecorder struct {
	mu        sync.Mutex
	successes int
	failures  int
}

func (r *countingRecorder) RecordPollSuccess(time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.successes++
}

func (r *countingRecorder) RecordPollFailure() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures++
}

func (r *countingRecorder) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.successes, r.failures
}

// testConfig builds a configuration with a short interval so tests are quick.
func testConfig() *config.Config {
	return &config.Config{
		Serial: config.Serial{
			Port:            "/dev/fake",
			Baud:            115200,
			ResponseTimeout: 200 * time.Millisecond,
			ReadTimeout:     20 * time.Millisecond,
		},
		Poll: config.Poll{Interval: 30 * time.Millisecond, AverageWindow: 60},
	}
}

// newTestPoller wires a poller to a fake device.
func newTestPoller(t *testing.T, dev *fakeDevice) (*Poller, *recordingPublisher, *countingRecorder, *bytes.Buffer) {
	t.Helper()

	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	pub := &recordingPublisher{}
	rec := &countingRecorder{}

	p := New(testConfig(), log, pub, rec, func(serialport.Config) (serialport.Port, error) {
		return dev, nil
	})
	return p, pub, rec, &logBuf
}

// TestPollerReadsRealDeviceResponses is the end-to-end happy path, driven by
// bytes recorded from the attached GMC-320.
func TestPollerReadsRealDeviceResponses(t *testing.T) {
	t.Parallel()

	dev := &fakeDevice{configBlock: loadRealConfigBlock(t)}
	p, pub, rec, _ := newTestPoller(t, dev)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	// Capture status while the loop is live. Run closes the port on the way
	// out, so a snapshot taken afterwards would correctly report
	// disconnected and tell us nothing about the running state.
	var status Status
	deadline := time.After(2 * time.Second)
	for {
		if status = p.Status(); status.Connected && len(pub.all()) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("poller never reported a successful reading; status=%+v", status)
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	readings := pub.all()
	if len(readings) == 0 {
		t.Fatal("no readings were published")
	}

	first := readings[0]
	if first.CPM != 28 {
		t.Errorf("CPM = %d, want 28", first.CPM)
	}
	// The device's own calibration is 0.0065 uSv/h per CPM, so 28 CPM is
	// 0.182. This asserts the measured calibration table is actually applied.
	if got := first.MicroSievertsPerHour; got < 0.1819 || got > 0.1821 {
		t.Errorf("uSv/h = %v, want 0.182 (28 CPM x 0.0065)", got)
	}
	if !first.Voltage.Valid || first.Voltage.Value != 4.2 {
		t.Errorf("voltage = %+v, want 4.2", first.Voltage)
	}
	if !first.TemperatureC.Valid || first.TemperatureC.Value != 31.1 {
		t.Errorf("temperature = %+v, want 31.1", first.TemperatureC)
	}

	if successes, failures := rec.counts(); successes == 0 || failures != 0 {
		t.Errorf("recorded %d successes and %d failures, want successes and no failures",
			successes, failures)
	}

	if status.Device.Model != "GMC-320" {
		t.Errorf("model = %q, want %q", status.Device.Model, "GMC-320")
	}
	if !strings.Contains(status.Calibration, "60 CPM=0.39") {
		t.Errorf("calibration = %q, expected the measured table", status.Calibration)
	}
}

// TestPollerSurvivesReadFailures covers reliability requirement 1: no transient
// failure may terminate the process.
func TestPollerSurvivesReadFailures(t *testing.T) {
	t.Parallel()

	// Succeed once, then fail on every subsequent CPM read.
	dev := &fakeDevice{configBlock: loadRealConfigBlock(t), failCPMAfter: 1}
	p, pub, rec, _ := newTestPoller(t, dev)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run returned an error on recoverable failures: %v", err)
	}

	successes, failures := rec.counts()
	if failures == 0 {
		t.Fatal("expected the failing reads to be recorded as failures")
	}
	if successes == 0 {
		t.Fatal("expected the first read to succeed")
	}
	// Failed cycles must not publish a fabricated reading.
	if len(pub.all()) != successes {
		t.Errorf("published %d readings but recorded %d successes; a failed cycle published data",
			len(pub.all()), successes)
	}
}

// TestPollerSurvivesDesynchronizedDevice covers the measured anomaly where the
// device answers one command with another command's reply.
func TestPollerSurvivesDesynchronizedDevice(t *testing.T) {
	t.Parallel()

	dev := &fakeDevice{configBlock: loadRealConfigBlock(t), desyncCPM: true}
	p, pub, rec, _ := newTestPoller(t, dev)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, failures := rec.counts(); failures == 0 {
		t.Error("expected desynchronized replies to be recorded as failures")
	}
	// The critical assertion: a 256-byte reply to GETCPM must never be
	// truncated into a plausible-looking count.
	if got := pub.all(); len(got) != 0 {
		t.Errorf("published %d readings from desynchronized replies: %+v", len(got), got)
	}
}

// TestPollerSurvivesDisconnectedAdapter covers a USB adapter disappearing,
// which must drop the connection and keep retrying rather than exit.
func TestPollerSurvivesDisconnectedAdapter(t *testing.T) {
	t.Parallel()

	dev := &fakeDevice{
		configBlock: loadRealConfigBlock(t),
		readErr:     errors.New("input/output error"),
	}

	var opens int
	var mu sync.Mutex
	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	p := New(testConfig(), log, &recordingPublisher{}, &countingRecorder{},
		func(serialport.Config) (serialport.Port, error) {
			mu.Lock()
			defer mu.Unlock()
			opens++
			return dev, nil
		})

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run returned an error for a disconnected adapter: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if opens < 2 {
		t.Errorf("port was opened %d time(s); expected the poller to reconnect after an I/O error", opens)
	}
}

// TestPollerSurvivesUnopenablePort covers a device node that is absent at
// startup, which must be retried rather than treated as fatal.
func TestPollerSurvivesUnopenablePort(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rec := &countingRecorder{}

	p := New(testConfig(), log, &recordingPublisher{}, rec,
		func(serialport.Config) (serialport.Port, error) {
			return nil, errors.New("no such file or directory")
		})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run returned an error for an absent device: %v", err)
	}

	if _, failures := rec.counts(); failures == 0 {
		t.Error("expected failures to be recorded while the device is absent")
	}
	if status := p.Status(); status.Connected {
		t.Error("expected the poller to report itself disconnected")
	}
}

// TestPollerContainsSinkPanics covers reliability requirement 3.
func TestPollerContainsSinkPanics(t *testing.T) {
	t.Parallel()

	dev := &fakeDevice{configBlock: loadRealConfigBlock(t)}

	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rec := &countingRecorder{}

	p := New(testConfig(), log, panickingPublisher{}, rec,
		func(serialport.Config) (serialport.Port, error) { return dev, nil })

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	// The assertion is simply that this returns rather than crashing the test
	// binary.
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(logBuf.String(), "panicked") {
		t.Error("expected the recovered panic to be logged")
	}
}

// panickingPublisher stands in for a sink whose client library panics.
type panickingPublisher struct{}

func (panickingPublisher) Publish(context.Context, reading.Reading) {
	panic("a third-party client exploded")
}

// TestPollerCalibrationOverride covers the escape hatch for a device whose
// config block does not match the measured layout.
func TestPollerCalibrationOverride(t *testing.T) {
	t.Parallel()

	dev := &fakeDevice{configBlock: loadRealConfigBlock(t)}

	cfg := testConfig()
	cfg.Poll.CalibrationOverride = "100:1.0"

	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	pub := &recordingPublisher{}

	p := New(cfg, log, pub, &countingRecorder{},
		func(serialport.Config) (serialport.Port, error) { return dev, nil })

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	readings := pub.all()
	if len(readings) == 0 {
		t.Fatal("no readings published")
	}
	// 28 CPM at 0.01 uSv/h per CPM is 0.28, not the device's own 0.182.
	if got := readings[0].MicroSievertsPerHour; got < 0.279 || got > 0.281 {
		t.Errorf("uSv/h = %v, want 0.28 from the override", got)
	}
}

// TestPollerRejectsBadCalibrationOverride confirms a malformed override is
// refused rather than silently ignored, since falling back would publish doses
// computed from a different table than the operator configured.
func TestPollerRejectsBadCalibrationOverride(t *testing.T) {
	t.Parallel()

	dev := &fakeDevice{configBlock: loadRealConfigBlock(t)}

	cfg := testConfig()
	cfg.Poll.CalibrationOverride = "not-a-calibration"

	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	pub := &recordingPublisher{}

	p := New(cfg, log, pub, &countingRecorder{},
		func(serialport.Config) (serialport.Port, error) { return dev, nil })

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(pub.all()) != 0 {
		t.Error("expected no readings to be published with an invalid calibration")
	}
	if !strings.Contains(logBuf.String(), "CALIBRATION") {
		t.Error("expected the log to name the offending setting")
	}
}
