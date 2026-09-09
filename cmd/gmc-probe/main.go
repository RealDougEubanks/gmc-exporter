// Command gmc-probe measures the wire behaviour of a GQ Electronics GMC-series
// Geiger counter over its USB-serial link.
//
// It exists because GQ-RFC1201 documents intended response lengths but says
// nothing about how those responses actually arrive across successive read(2)
// calls. On the CH340/CH341 bridges these devices ship with, a response
// routinely arrives split across several reads. The probe records exactly what
// happens: how many bytes each command returns, how they are chunked, and when
// each chunk lands.
//
// Its JSON output is committed as the raw evidence behind the protocol test
// fixtures, so every fixture is traceable to a real capture rather than
// invention.
//
// The probe issues read-only commands only. It never sends POWEROFF, REBOOT,
// FACTORYRESET, ECFG, WCFG or any SETDATE/SETTIME command, so it cannot alter
// the device's configuration or clock.
package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/RealDougEubanks/gmc-exporter/internal/serialport"
)

// probeCommand is one read-only command to characterise.
type probeCommand struct {
	// Name identifies the command in the output.
	Name string `json:"name"`
	// Command is the exact ASCII sent on the wire.
	Command string `json:"command"`
	// SpecBytes is the response length GQ-RFC1201 documents, or 0 where the
	// specification documents no response. It is recorded for comparison
	// only; the probe never assumes it.
	SpecBytes int `json:"specBytes"`
	// SpecSection is the numbered section of GQ-RFC1201 documenting it.
	SpecSection string `json:"specSection"`
}

// readOnlyCommands are the commands the probe is permitted to send.
var readOnlyCommands = []probeCommand{
	{Name: "GETVER", Command: "<GETVER>>", SpecBytes: 14, SpecSection: "1"},
	{Name: "GETCPM", Command: "<GETCPM>>", SpecBytes: 2, SpecSection: "2"},
	{Name: "GETVOLT", Command: "<GETVOLT>>", SpecBytes: 1, SpecSection: "5"},
	{Name: "GETCFG", Command: "<GETCFG>>", SpecBytes: 256, SpecSection: "7"},
	{Name: "GETSERIAL", Command: "<GETSERIAL>>", SpecBytes: 7, SpecSection: "11"},
	{Name: "GETDATETIME", Command: "<GETDATETIME>>", SpecBytes: 7, SpecSection: "23"},
	{Name: "GETTEMP", Command: "<GETTEMP>>", SpecBytes: 4, SpecSection: "24"},
	{Name: "GETGYRO", Command: "<GETGYRO>>", SpecBytes: 7, SpecSection: "25"},
}

// chunk is a single successful read(2) return.
type chunk struct {
	// AtMicros is microseconds elapsed since the command was written.
	AtMicros int64 `json:"atMicros"`
	// N is how many bytes this read returned.
	N int `json:"n"`
	// Hex is the raw bytes of this chunk.
	Hex string `json:"hex"`
}

// iteration is one request/response exchange.
type iteration struct {
	Index       int     `json:"index"`
	Chunks      []chunk `json:"chunks"`
	TotalBytes  int     `json:"totalBytes"`
	ElapsedMic  int64   `json:"elapsedMicros"`
	Hex         string  `json:"hex"`
	ASCII       string  `json:"ascii,omitempty"`
	TerminalErr string  `json:"terminalError,omitempty"`
}

// commandResult aggregates every iteration of one command.
type commandResult struct {
	probeCommand
	Iterations []iteration `json:"iterations"`
	// Observed length statistics across iterations.
	MinBytes      int            `json:"minBytes"`
	MaxBytes      int            `json:"maxBytes"`
	LengthCounts  map[string]int `json:"lengthCounts"`
	ChunkCounts   map[string]int `json:"chunkCounts"`
	SplitReads    int            `json:"splitReadCount"`
	MatchesSpec   bool           `json:"matchesSpec"`
	SpecDiscrepan string         `json:"specDiscrepancy,omitempty"`
}

// capture is the whole probe run.
type capture struct {
	Tool          string          `json:"tool"`
	Device        string          `json:"device"`
	Baud          int             `json:"baud"`
	Host          string          `json:"host"`
	StartedAt     string          `json:"startedAt"`
	Iterations    int             `json:"iterationsPerCommand"`
	SettleMillis  int             `json:"interCommandSettleMillis"`
	QuietMillis   int             `json:"quietPeriodMillis"`
	ReadTimeoutMs int             `json:"perReadTimeoutMillis"`
	Commands      []commandResult `json:"commands"`
}

func main() {
	var (
		device      = flag.String("device", "/dev/ttyUSB0", "serial device node")
		baud        = flag.Int("baud", serialport.DefaultBaud, "line rate")
		iterations  = flag.Int("iterations", 20, "how many times to issue each command")
		quietMs     = flag.Int("quiet-ms", 300, "stop reading a response after this long with no new bytes")
		overallMs   = flag.Int("overall-ms", 4000, "hard cap on how long to collect one response")
		settleMs    = flag.Int("settle-ms", 120, "pause between commands")
		readTimeout = flag.Int("read-timeout-ms", 250, "per-read poll timeout")
		out         = flag.String("out", "", "write JSON capture to this path (default stdout)")
		only        = flag.String("only", "", "comma-separated command names to probe (default all)")
	)
	flag.Parse()

	if err := run(*device, *baud, *iterations, *quietMs, *overallMs, *settleMs, *readTimeout, *out, *only); err != nil {
		fmt.Fprintf(os.Stderr, "gmc-probe: %v\n", err)
		os.Exit(1)
	}
}

// selectCommands filters readOnlyCommands by a comma-separated name list.
// An empty list selects everything.
func selectCommands(only string) ([]probeCommand, error) {
	if only == "" {
		return readOnlyCommands, nil
	}
	wanted := map[string]bool{}
	for _, name := range strings.Split(only, ",") {
		name = strings.ToUpper(strings.TrimSpace(name))
		if name != "" {
			wanted[name] = true
		}
	}
	var selected []probeCommand
	for _, cmd := range readOnlyCommands {
		if wanted[cmd.Name] {
			selected = append(selected, cmd)
			delete(wanted, cmd.Name)
		}
	}
	if len(wanted) > 0 {
		var unknown []string
		for name := range wanted {
			unknown = append(unknown, name)
		}
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown command(s): %s", strings.Join(unknown, ", "))
	}
	return selected, nil
}

func run(device string, baud, iterations, quietMs, overallMs, settleMs, readTimeoutMs int, out, only string) error {
	commands, err := selectCommands(only)
	if err != nil {
		return err
	}

	port, err := serialport.Open(serialport.Config{
		Path:        device,
		Baud:        baud,
		ReadTimeout: time.Duration(readTimeoutMs) * time.Millisecond,
	})
	if err != nil {
		return err
	}
	defer func() {
		if c, ok := port.(interface{ Close() error }); ok {
			_ = c.Close()
		}
	}()

	host, _ := os.Hostname()
	result := capture{
		Tool:          "gmc-probe",
		Device:        device,
		Baud:          baud,
		Host:          host,
		StartedAt:     time.Now().UTC().Format(time.RFC3339),
		Iterations:    iterations,
		SettleMillis:  settleMs,
		QuietMillis:   quietMs,
		ReadTimeoutMs: readTimeoutMs,
	}

	for _, cmd := range commands {
		fmt.Fprintf(os.Stderr, "probing %-12s (%s) ", cmd.Name, cmd.Command)
		cr := commandResult{
			probeCommand: cmd,
			LengthCounts: map[string]int{},
			ChunkCounts:  map[string]int{},
			MinBytes:     -1,
		}

		for i := 0; i < iterations; i++ {
			it := exchange(port, cmd.Command, i,
				time.Duration(quietMs)*time.Millisecond,
				time.Duration(overallMs)*time.Millisecond)
			cr.Iterations = append(cr.Iterations, it)

			cr.LengthCounts[fmt.Sprintf("%d", it.TotalBytes)]++
			cr.ChunkCounts[fmt.Sprintf("%d", len(it.Chunks))]++
			if len(it.Chunks) > 1 {
				cr.SplitReads++
			}
			if cr.MinBytes < 0 || it.TotalBytes < cr.MinBytes {
				cr.MinBytes = it.TotalBytes
			}
			if it.TotalBytes > cr.MaxBytes {
				cr.MaxBytes = it.TotalBytes
			}
			if iterations <= 50 || i%25 == 0 {
				fmt.Fprint(os.Stderr, ".")
			}
			if settleMs > 0 {
				time.Sleep(time.Duration(settleMs) * time.Millisecond)
			}
		}

		cr.MatchesSpec = cr.MinBytes == cmd.SpecBytes && cr.MaxBytes == cmd.SpecBytes
		if !cr.MatchesSpec {
			cr.SpecDiscrepan = fmt.Sprintf(
				"GQ-RFC1201 section %s documents %d bytes; device returned between %d and %d",
				cmd.SpecSection, cmd.SpecBytes, cr.MinBytes, cr.MaxBytes)
		}
		fmt.Fprintf(os.Stderr, " len=%d..%d chunks=%v splits=%d\n",
			cr.MinBytes, cr.MaxBytes, cr.ChunkCounts, cr.SplitReads)

		result.Commands = append(result.Commands, cr)
	}

	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encode capture: %w", err)
	}
	if out == "" {
		fmt.Println(string(encoded))
		return nil
	}
	if err := os.WriteFile(out, append(encoded, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", out, err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", out, len(encoded))
	return nil
}

// exchange writes one command and records every chunk that comes back until the
// line goes quiet for quiet, or overall elapses.
//
// It deliberately does not stop at the specification's expected length. Reading
// until the device stops talking is the only way to discover a response that is
// longer than documented.
func exchange(port serialport.Port, command string, index int, quiet, overall time.Duration) iteration {
	it := iteration{Index: index}

	if err := port.Drain(); err != nil {
		it.TerminalErr = fmt.Sprintf("drain: %v", err)
		return it
	}

	start := time.Now()
	if _, err := port.Write([]byte(command)); err != nil {
		it.TerminalErr = fmt.Sprintf("write: %v", err)
		return it
	}

	var collected []byte
	hardDeadline := start.Add(overall)
	lastByteAt := start

	buf := make([]byte, 512)
	for {
		now := time.Now()
		if now.After(hardDeadline) {
			break
		}
		if len(collected) > 0 && now.Sub(lastByteAt) > quiet {
			break
		}

		n, err := port.Read(buf)
		if err != nil {
			if errors.Is(err, serialport.ErrTimeout) {
				// No data this poll. Keep waiting until the quiet period
				// expires so we can see late-arriving trailing bytes.
				if len(collected) == 0 && time.Since(start) > overall {
					break
				}
				continue
			}
			it.TerminalErr = err.Error()
			break
		}

		lastByteAt = time.Now()
		it.Chunks = append(it.Chunks, chunk{
			AtMicros: lastByteAt.Sub(start).Microseconds(),
			N:        n,
			Hex:      hex.EncodeToString(buf[:n]),
		})
		collected = append(collected, buf[:n]...)
	}

	it.TotalBytes = len(collected)
	it.ElapsedMic = time.Since(start).Microseconds()
	it.Hex = hex.EncodeToString(collected)
	it.ASCII = printableASCII(collected)
	return it
}

// printableASCII renders a response as text, with non-printable bytes shown as
// a dot. Useful for GETVER and GETSERIAL where part of the payload is text.
func printableASCII(b []byte) string {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 0x20 && c < 0x7f {
			out[i] = c
		} else {
			out[i] = '.'
		}
	}
	return string(out)
}
