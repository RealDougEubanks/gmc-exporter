package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/RealDougEubanks/gmc-exporter/internal/config"
)

// testConfigWithInterval builds a configuration with the given poll interval in
// seconds.
func testConfigWithInterval(seconds int) *config.Config {
	return &config.Config{
		Poll: config.Poll{Interval: time.Duration(seconds) * time.Second},
	}
}

// TestParseArgs covers the command line.
//
// The --version case is here because of a real incident during hardware
// testing: with no flag parsing at all, `gmc-exporter --version` silently
// ignored the argument and started a full exporter, which then polled the
// device forever. A diagnostic command that instead launches the service is a
// genuinely surprising failure, and it held a serial port that something else
// needed.
func TestParseArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		args        []string
		wantProceed bool
		wantCode    int
		wantOutput  string
	}{
		{
			name:        "no arguments runs the exporter",
			args:        nil,
			wantProceed: true,
			wantCode:    exitOK,
		},
		{
			name:        "version prints and exits cleanly",
			args:        []string{"--version"},
			wantProceed: false,
			wantCode:    exitOK,
			wantOutput:  "gmc-exporter",
		},
		{
			name:        "single dash version is accepted too",
			args:        []string{"-version"},
			wantProceed: false,
			wantCode:    exitOK,
			wantOutput:  "gmc-exporter",
		},
		{
			name:        "unknown flag is rejected rather than ignored",
			args:        []string{"--nonsense"},
			wantProceed: false,
			wantCode:    exitFailure,
		},
		{
			// The previous exporter took a config file path as an argument.
			// Anyone carrying that habit over should get a clear message
			// rather than a process that appears to start and then ignores
			// the file.
			name:        "positional argument is rejected with guidance",
			args:        []string{"/config.ini"},
			wantProceed: false,
			wantCode:    exitFailure,
			wantOutput:  "GOGMC_",
		},
		{
			name:        "help exits without running",
			args:        []string{"-h"},
			wantProceed: false,
			wantCode:    exitFailure,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			proceed, code := parseArgs(tc.args, &out)

			if proceed != tc.wantProceed {
				t.Errorf("proceed = %v, want %v", proceed, tc.wantProceed)
			}
			if code != tc.wantCode {
				t.Errorf("code = %d, want %d (output %q)", code, tc.wantCode, out.String())
			}
			if tc.wantOutput != "" && !strings.Contains(out.String(), tc.wantOutput) {
				t.Errorf("output %q does not contain %q", out.String(), tc.wantOutput)
			}
		})
	}
}

// TestVersionOutputCarriesBuildIdentifiers checks the version string is
// actually useful for identifying a deployed build.
func TestVersionOutputCarriesBuildIdentifiers(t *testing.T) {
	t.Parallel()

	origVersion, origCommit := version, commit
	t.Cleanup(func() { version, commit = origVersion, origCommit })

	version, commit = "v1.2.3", "deadbee"

	var out bytes.Buffer
	if proceed, code := parseArgs([]string{"--version"}, &out); proceed || code != exitOK {
		t.Fatalf("proceed=%v code=%d, want false and %d", proceed, code, exitOK)
	}

	for _, want := range []string{"v1.2.3", "deadbee", "go1."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("version output %q does not contain %q", out.String(), want)
		}
	}
}

// TestSinkTimeoutStaysBelowPollInterval checks a stalled backend cannot delay
// the next reading.
func TestSinkTimeoutStaysBelowPollInterval(t *testing.T) {
	t.Parallel()

	for _, interval := range []int{1, 5, 30, 60, 300, 3600} {
		cfg := testConfigWithInterval(interval)
		got := sinkTimeout(cfg)
		if got >= cfg.Poll.Interval && cfg.Poll.Interval > time.Second {
			t.Errorf("poll interval %s gave a sink timeout of %s, which is not shorter",
				cfg.Poll.Interval, got)
		}
		if got <= 0 {
			t.Errorf("poll interval %s gave a non-positive sink timeout %s",
				cfg.Poll.Interval, got)
		}
	}
}
