package gmc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RealDougEubanks/gmc-exporter/internal/serialport"
)

// TestExchangeReplaysEveryCapturedIteration replays every recorded exchange
// from every capture and asserts the protocol layer reproduces the response
// exactly.
//
// This is the test that converts one hardware session into permanent coverage:
// it runs on any machine, with no Geiger counter attached, and it exercises the
// real chunk boundaries the CH340 produced rather than idealised ones.
func TestExchangeReplaysEveryCapturedIteration(t *testing.T) {
	t.Parallel()

	byName := map[string]Command{
		CmdGetVersion.Name:     CmdGetVersion,
		CmdGetCPM.Name:         CmdGetCPM,
		CmdGetVoltage.Name:     CmdGetVoltage,
		CmdGetConfig.Name:      CmdGetConfig,
		CmdGetSerial.Name:      CmdGetSerial,
		CmdGetDateTime.Name:    CmdGetDateTime,
		CmdGetTemperature.Name: CmdGetTemperature,
		CmdGetGyro.Name:        CmdGetGyro,
	}

	for _, path := range []string{captureBaseline, captureAggressive, captureGetCfg} {
		run := loadCapture(t, path)

		for _, recorded := range run.Commands {
			cmd, ok := byName[recorded.Name]
			if !ok {
				t.Fatalf("%s: capture contains unknown command %q", path, recorded.Name)
			}

			var replayed, wrongLength, absent int
			for _, it := range recorded.Iterations {
				port := newFakePort(it.chunkBytes(t)...)
				dev := NewDevice(port, time.Second)

				got, err := dev.Exchange(context.Background(), cmd)

				switch it.TotalBytes {
				case cmd.ResponseBytes:
					if err != nil {
						t.Fatalf("%s %s iteration %d: unexpected error: %v",
							path, recorded.Name, it.Index, err)
					}
					if h := toHex(got); h != it.Hex {
						t.Fatalf("%s %s iteration %d: got %s, recorded %s",
							path, recorded.Name, it.Index, h, it.Hex)
					}
					replayed++

				case 0:
					// The device sent nothing at all. Measured on real
					// hardware; must be a clean error, never a panic.
					if !errors.Is(err, ErrNoResponse) {
						t.Fatalf("%s %s iteration %d: empty response gave %v, want ErrNoResponse",
							path, recorded.Name, it.Index, err)
					}
					absent++

				default:
					// A reply of the wrong size. Either it was short, or it
					// was a longer reply belonging to another command.
					if err == nil {
						t.Fatalf("%s %s iteration %d: %d-byte response accepted for a %d-byte command",
							path, recorded.Name, it.Index, it.TotalBytes, cmd.ResponseBytes)
					}
					if !errors.Is(err, ErrUnexpectedLength) && !errors.Is(err, ErrDesynchronized) {
						t.Fatalf("%s %s iteration %d: got %v, want a length or desync error",
							path, recorded.Name, it.Index, err)
					}
					wrongLength++
				}

				// Every exchange must drain before writing, so a stale reply
				// cannot be mistaken for the current one.
				if port.drains == 0 {
					t.Fatalf("%s %s iteration %d: Exchange did not drain before writing",
						path, recorded.Name, it.Index)
				}
				if len(port.writes) != 1 || string(port.writes[0]) != cmd.Wire {
					t.Fatalf("%s %s iteration %d: wrote %q, want %q",
						path, recorded.Name, it.Index, port.writes, cmd.Wire)
				}
			}

			t.Logf("%-24s %-12s replayed=%d wrongLength=%d absent=%d",
				shortPath(path), recorded.Name, replayed, wrongLength, absent)
		}
	}
}

// TestExchangeRejectsDesynchronizedReply pins the exact hardware anomaly that
// makes the trailing-data check necessary.
//
// During a 400-iteration aggressive poll, a <GETTEMP>> returned a full 256-byte
// configuration block instead of 4 bytes. Reading only the first 4 bytes would
// have yielded a plausible, completely wrong temperature. This asserts the
// desynchronized reply is refused rather than truncated.
func TestExchangeRejectsDesynchronizedReply(t *testing.T) {
	t.Parallel()

	run := loadCapture(t, captureAggressive)
	temp := run.command(t, CmdGetTemperature.Name)

	var found bool
	for _, it := range temp.Iterations {
		if it.TotalBytes != 256 {
			continue
		}
		found = true

		port := newFakePort(it.chunkBytes(t)...)
		dev := NewDevice(port, time.Second)

		got, err := dev.Exchange(context.Background(), CmdGetTemperature)
		if err == nil {
			t.Fatalf("a 256-byte reply to GETTEMP was accepted as %x", got)
		}
		if !errors.Is(err, ErrDesynchronized) {
			t.Fatalf("got %v, want ErrDesynchronized", err)
		}
		if got != nil {
			t.Fatalf("expected no data alongside the error, got %x", got)
		}
		// The link must be cleared so the next exchange starts clean.
		if port.drains < 2 {
			t.Fatalf("expected a drain after desync, saw %d drain(s)", port.drains)
		}
	}

	if !found {
		t.Fatal("capture no longer contains the 256-byte GETTEMP anomaly this test exists to cover")
	}
}

// TestExchangeAccumulatesAcrossSplitReads proves a response arriving in pieces
// is reassembled rather than truncated.
//
// GETCFG is the case that always splits: 256 bytes arrive as eight 32-byte
// reads, because that is the CH340 bulk endpoint size. A single-read
// implementation would see 32 bytes and get this wrong every time.
func TestExchangeAccumulatesAcrossSplitReads(t *testing.T) {
	t.Parallel()

	run := loadCapture(t, captureGetCfg)
	cfg := run.command(t, CmdGetConfig.Name)

	var multiChunk int
	for _, it := range cfg.Iterations {
		if len(it.Chunks) < 2 || it.TotalBytes != 256 {
			continue
		}
		multiChunk++

		chunks := it.chunkBytes(t)
		port := newFakePort(chunks...)
		dev := NewDevice(port, time.Second)

		got, err := dev.Exchange(context.Background(), CmdGetConfig)
		if err != nil {
			t.Fatalf("iteration %d (%d chunks): %v", it.Index, len(chunks), err)
		}
		if len(got) != 256 {
			t.Fatalf("iteration %d: reassembled %d bytes, want 256", it.Index, len(got))
		}
		if toHex(got) != it.Hex {
			t.Fatalf("iteration %d: reassembly does not match the recorded bytes", it.Index)
		}
	}

	if multiChunk == 0 {
		t.Fatal("capture contains no split GETCFG responses")
	}
	t.Logf("reassembled %d split GETCFG responses", multiChunk)
}

// TestExchangeErrorPaths covers failures that are awkward to provoke on real
// hardware but must never crash the process.
func TestExchangeErrorPaths(t *testing.T) {
	t.Parallel()

	t.Run("silent device reports ErrNoResponse", func(t *testing.T) {
		t.Parallel()
		dev := NewDevice(newFakePort(), 100*time.Millisecond)
		if _, err := dev.Exchange(context.Background(), CmdGetCPM); !errors.Is(err, ErrNoResponse) {
			t.Fatalf("got %v, want ErrNoResponse", err)
		}
	})

	t.Run("truncated reply reports a length error", func(t *testing.T) {
		t.Parallel()
		// One byte of a two-byte CPM reply: the classic short read.
		dev := NewDevice(newFakePort([]byte{0x00}), 100*time.Millisecond)
		_, err := dev.Exchange(context.Background(), CmdGetCPM)
		if !errors.Is(err, ErrUnexpectedLength) {
			t.Fatalf("got %v, want ErrUnexpectedLength", err)
		}
		var le *LengthError
		if !errors.As(err, &le) || le.Got != 1 || le.Want != 2 {
			t.Fatalf("got %#v, want a LengthError of got=1 want=2", err)
		}
	})

	t.Run("device error is surfaced not swallowed", func(t *testing.T) {
		t.Parallel()
		port := newFakePort()
		port.readErr = errors.New("device unplugged")
		dev := NewDevice(port, 100*time.Millisecond)
		if _, err := dev.Exchange(context.Background(), CmdGetCPM); err == nil {
			t.Fatal("expected the read error to propagate")
		}
	})

	t.Run("cancelled context stops promptly", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		dev := NewDevice(newFakePort(), time.Second)
		if _, err := dev.Exchange(ctx, CmdGetCPM); !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want context.Canceled", err)
		}
	})

	t.Run("read timeout is not treated as data", func(t *testing.T) {
		t.Parallel()
		// A port that always times out must not spin or be read as a zero
		// byte. This is the trap that (0, nil) timeout conventions create.
		port := newFakePort()
		port.readErr = serialport.ErrTimeout
		dev := NewDevice(port, 80*time.Millisecond)
		start := time.Now()
		_, err := dev.Exchange(context.Background(), CmdGetCPM)
		if !errors.Is(err, ErrNoResponse) {
			t.Fatalf("got %v, want ErrNoResponse", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("timeout handling took %s, expected it to stop at the deadline", elapsed)
		}
	})
}

// toHex renders bytes the same way the probe recorded them.
func toHex(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0x0f])
	}
	return string(out)
}

// shortPath trims the testdata prefix for readable test log lines.
func shortPath(p string) string {
	const prefix = "testdata/captures/"
	if len(p) > len(prefix) && p[:len(prefix)] == prefix {
		return p[len(prefix):]
	}
	return p
}
