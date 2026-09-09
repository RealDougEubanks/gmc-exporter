package gmc

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/RealDougEubanks/gmc-exporter/internal/serialport"
)

// The fixtures replayed by these tests are verbatim recordings from a real
// GQ Electronics GMC-320 attached over a CH340 USB-serial bridge, captured with
// cmd/gmc-probe. Nothing here is hand-authored: every byte sequence, including
// every split and every malformed reply, actually came off the wire.
//
// See docs/protocol-measurements.md for how each capture was taken.
const (
	captureBaseline   = "testdata/captures/baseline-20iter.json"
	captureAggressive = "testdata/captures/aggressive-400iter.json"
	captureGetCfg     = "testdata/captures/getcfg-100iter.json"
)

// capturedChunk is one read(2) return as recorded by the probe.
type capturedChunk struct {
	AtMicros int64  `json:"atMicros"`
	N        int    `json:"n"`
	Hex      string `json:"hex"`
}

// capturedIteration is one request/response exchange.
type capturedIteration struct {
	Index      int             `json:"index"`
	Chunks     []capturedChunk `json:"chunks"`
	TotalBytes int             `json:"totalBytes"`
	Hex        string          `json:"hex"`
}

// capturedCommand aggregates every recorded iteration of one command.
type capturedCommand struct {
	Name       string              `json:"name"`
	Command    string              `json:"command"`
	SpecBytes  int                 `json:"specBytes"`
	MinBytes   int                 `json:"minBytes"`
	MaxBytes   int                 `json:"maxBytes"`
	Iterations []capturedIteration `json:"iterations"`
}

// capturedRun is a whole probe run.
type capturedRun struct {
	Device    string            `json:"device"`
	Baud      int               `json:"baud"`
	StartedAt string            `json:"startedAt"`
	Commands  []capturedCommand `json:"commands"`
}

// loadCapture reads a probe capture from testdata.
func loadCapture(t *testing.T, path string) capturedRun {
	t.Helper()

	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read capture %s: %v", path, err)
	}
	var run capturedRun
	if err := json.Unmarshal(raw, &run); err != nil {
		t.Fatalf("decode capture %s: %v", path, err)
	}
	if len(run.Commands) == 0 {
		t.Fatalf("capture %s contains no commands", path)
	}
	return run
}

// command returns the recorded results for one command name.
func (r capturedRun) command(t *testing.T, name string) capturedCommand {
	t.Helper()
	for _, c := range r.Commands {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("capture has no command %q", name)
	return capturedCommand{}
}

// chunkBytes converts a recorded iteration back into its read-by-read byte
// sequence, preserving exactly how the response was split on real hardware.
func (it capturedIteration) chunkBytes(t *testing.T) [][]byte {
	t.Helper()
	out := make([][]byte, 0, len(it.Chunks))
	for _, c := range it.Chunks {
		b, err := hex.DecodeString(c.Hex)
		if err != nil {
			t.Fatalf("iteration %d: decode chunk hex: %v", it.Index, err)
		}
		if len(b) != c.N {
			t.Fatalf("iteration %d: chunk claims n=%d but decoded %d bytes", it.Index, c.N, len(b))
		}
		out = append(out, b)
	}
	return out
}

// fakePort replays a recorded chunk sequence through the serialport.Port
// interface.
//
// It reproduces kernel read semantics rather than simplifying them: a Read that
// asks for fewer bytes than the pending chunk holds consumes only what it asked
// for and leaves the remainder queued. That detail matters, because it is what
// lets a test prove the protocol layer notices trailing bytes instead of
// silently truncating a desynchronized reply.
type fakePort struct {
	chunks [][]byte
	// leftover holds the unconsumed tail of a partially read chunk.
	leftover []byte

	writes  [][]byte
	drains  int
	reads   int
	readErr error
}

// newFakePort replays the given chunks, then reports ErrTimeout forever, which
// is how a real port behaves once the device has gone quiet.
func newFakePort(chunks ...[]byte) *fakePort {
	return &fakePort{chunks: chunks}
}

func (f *fakePort) Read(p []byte) (int, error) {
	f.reads++
	if f.readErr != nil {
		return 0, f.readErr
	}
	if len(p) == 0 {
		return 0, nil
	}

	if len(f.leftover) == 0 {
		if len(f.chunks) == 0 {
			return 0, serialport.ErrTimeout
		}
		f.leftover = f.chunks[0]
		f.chunks = f.chunks[1:]
	}

	n := copy(p, f.leftover)
	f.leftover = f.leftover[n:]
	if n == 0 {
		return 0, serialport.ErrTimeout
	}
	return n, nil
}

func (f *fakePort) Write(p []byte) (int, error) {
	cp := make([]byte, len(p))
	copy(cp, p)
	f.writes = append(f.writes, cp)
	return len(p), nil
}

// Drain discards the partially consumed chunk only.
//
// The queued chunks represent the reply to the command about to be sent, so
// clearing them would delete the fixture rather than model a flush. Exchange
// drains immediately before writing, when a real port's input queue is empty
// anyway.
func (f *fakePort) Drain() error {
	f.drains++
	f.leftover = nil
	return nil
}

// ensure fakePort satisfies the interface the protocol layer depends on.
var _ serialport.Port = (*fakePort)(nil)
