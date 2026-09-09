// Package gmc implements the GQ Electronics GMC-series Geiger counter serial
// protocol, as documented in GQ-RFC1201 and as measured against a real GMC-320.
//
// Every response length in this package was verified empirically before being
// relied upon; see docs/protocol-measurements.md for the capture evidence and
// for the places where the device's behaviour is not covered by the
// specification.
//
// The device is driven through an injected serialport.Port rather than a
// package-level handle, so the whole protocol layer is exercisable in tests
// with no hardware attached.
package gmc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/RealDougEubanks/gmc-exporter/internal/serialport"
)

// Command is a single request/response exchange with the device.
type Command struct {
	// Name is a short identifier used in logs and metrics.
	Name string

	// Wire is the exact ASCII sent to the device. GQ-RFC1201 "Command format"
	// specifies commands open with '<' and close with '>>'.
	Wire string

	// ResponseBytes is how many bytes a complete response contains. Every
	// value here was confirmed against a real GMC-320; see
	// docs/protocol-measurements.md.
	ResponseBytes int

	// SpecSection is the numbered section of GQ-RFC1201 that documents this
	// command, for traceability.
	SpecSection string
}

// The read-only command set this exporter uses.
//
// Commands that alter the device — POWEROFF, REBOOT, FACTORYRESET, ECFG, WCFG
// and the SETDATE/SETTIME family — are deliberately not implemented. An
// exporter has no reason to mutate the instrument it is reading, and omitting
// them removes any chance of doing so by accident.
var (
	// CmdGetVersion returns 7 bytes of model plus 7 bytes of firmware version.
	CmdGetVersion = Command{Name: "GETVER", Wire: "<GETVER>>", ResponseBytes: 14, SpecSection: "1"}

	// CmdGetCPM returns counts per minute as a 16-bit big-endian integer.
	CmdGetCPM = Command{Name: "GETCPM", Wire: "<GETCPM>>", ResponseBytes: 2, SpecSection: "2"}

	// CmdGetVoltage returns battery voltage as a single byte in tenths of a volt.
	CmdGetVoltage = Command{Name: "GETVOLT", Wire: "<GETVOLT>>", ResponseBytes: 1, SpecSection: "5"}

	// CmdGetConfig returns the 256-byte configuration block, which carries the
	// CPM-to-microsievert calibration table.
	CmdGetConfig = Command{Name: "GETCFG", Wire: "<GETCFG>>", ResponseBytes: 256, SpecSection: "7"}

	// CmdGetSerial returns the 7-byte serial number.
	CmdGetSerial = Command{Name: "GETSERIAL", Wire: "<GETSERIAL>>", ResponseBytes: 7, SpecSection: "11"}

	// CmdGetDateTime returns YY MM DD HH MM SS followed by a 0xAA terminator.
	CmdGetDateTime = Command{Name: "GETDATETIME", Wire: "<GETDATETIME>>", ResponseBytes: 7, SpecSection: "23"}

	// CmdGetTemperature returns integer, decimal, sign and a 0xAA terminator.
	CmdGetTemperature = Command{Name: "GETTEMP", Wire: "<GETTEMP>>", ResponseBytes: 4, SpecSection: "24"}

	// CmdGetGyro returns X, Y and Z as 16-bit big-endian values plus a 0xAA
	// terminator.
	CmdGetGyro = Command{Name: "GETGYRO", Wire: "<GETGYRO>>", ResponseBytes: 7, SpecSection: "25"}
)

// Protocol errors. Callers distinguish these to decide whether to skip a
// reading and carry on, which is always the correct response to a bad read.
var (
	// ErrNoResponse means the device sent nothing at all before the deadline.
	ErrNoResponse = errors.New("gmc: device sent no response")

	// ErrUnexpectedLength means a response arrived but was not the documented
	// size. Measured on real hardware at roughly 1 exchange in 400 under
	// aggressive polling, where a 4-byte GETTEMP reply came back as a 256-byte
	// configuration block. It is recoverable: skip the reading and retry.
	ErrUnexpectedLength = errors.New("gmc: unexpected response length")

	// ErrBadTerminator means a response of the right length did not carry the
	// 0xAA terminator byte GQ-RFC1201 specifies for that command.
	ErrBadTerminator = errors.New("gmc: bad response terminator")

	// ErrDesynchronized means the device sent more bytes than the command's
	// response should contain, so the extra bytes belong to some other reply
	// and the link is out of step.
	//
	// This is not hypothetical. On a real GMC-320 polled back to back, a
	// <GETTEMP>> was observed returning a full 256-byte configuration block.
	// Truncating that to its first 4 bytes would have produced a plausible
	// but entirely wrong temperature, so it is treated as an error and the
	// input buffer is drained.
	ErrDesynchronized = errors.New("gmc: response longer than expected, link desynchronized")
)

// LengthError carries the detail behind ErrUnexpectedLength.
type LengthError struct {
	Command string
	Want    int
	Got     int
}

func (e *LengthError) Error() string {
	return fmt.Sprintf("gmc: %s returned %d bytes, expected %d", e.Command, e.Got, e.Want)
}

// Unwrap lets callers match with errors.Is(err, ErrUnexpectedLength).
func (e *LengthError) Unwrap() error { return ErrUnexpectedLength }

// DefaultResponseTimeout bounds a complete response.
//
// Measured first-byte latency on a real GMC-320 reached 171ms, and a full
// 256-byte GETCFG takes about 265ms to arrive across its eight chunks. Two
// seconds leaves a wide margin over both while still failing fast enough that
// a wedged device cannot stall a 60-second poll cycle.
const DefaultResponseTimeout = 2 * time.Second

// Device talks to one Geiger counter over an injected port.
//
// Device is not safe for concurrent use. The serial link is a single
// request/response channel, so callers must serialise access; the poll loop
// does this naturally by driving all reads from one goroutine.
type Device struct {
	port    serialport.Port
	timeout time.Duration
}

// NewDevice wraps a port. A non-positive timeout selects DefaultResponseTimeout.
func NewDevice(port serialport.Port, timeout time.Duration) *Device {
	if timeout <= 0 {
		timeout = DefaultResponseTimeout
	}
	return &Device{port: port, timeout: timeout}
}

// Exchange sends a command and returns exactly cmd.ResponseBytes bytes.
//
// It accumulates across successive reads rather than trusting a single one.
// That is mandatory, not defensive: the CH340/CH341 USB-serial bridges these
// devices ship with deliver at most 32 bytes per USB packet, so any response
// larger than 32 bytes always arrives split. A 256-byte GETCFG was measured
// arriving as eight separate reads on 100 consecutive attempts.
//
// The input buffer is drained first so a stale or partial reply from an earlier
// exchange cannot be misread as this one's.
//
// Every failure is returned as an error. Exchange never returns a short buffer,
// which is what makes it impossible for a caller to index past the end of a
// truncated response.
func (d *Device) Exchange(ctx context.Context, cmd Command) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := d.port.Drain(); err != nil {
		return nil, fmt.Errorf("gmc: %s: drain before write: %w", cmd.Name, err)
	}
	if _, err := d.port.Write([]byte(cmd.Wire)); err != nil {
		return nil, fmt.Errorf("gmc: %s: write command: %w", cmd.Name, err)
	}

	deadline := time.Now().Add(d.timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	buf := make([]byte, 0, cmd.ResponseBytes)
	chunk := make([]byte, cmd.ResponseBytes)

	for len(buf) < cmd.ResponseBytes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if time.Now().After(deadline) {
			break
		}

		n, err := d.port.Read(chunk[:cmd.ResponseBytes-len(buf)])
		if err != nil {
			if errors.Is(err, serialport.ErrTimeout) {
				// No bytes this round. Keep waiting until the overall
				// deadline; a slow device is not a failed device.
				continue
			}
			return nil, fmt.Errorf("gmc: %s: read: %w", cmd.Name, err)
		}
		buf = append(buf, chunk[:n]...)
	}

	if len(buf) == 0 {
		return nil, fmt.Errorf("gmc: %s after %s: %w", cmd.Name, d.timeout, ErrNoResponse)
	}
	if len(buf) != cmd.ResponseBytes {
		return nil, &LengthError{Command: cmd.Name, Want: cmd.ResponseBytes, Got: len(buf)}
	}
	if err := d.checkNoTrailingData(cmd); err != nil {
		return nil, err
	}
	return buf, nil
}

// checkNoTrailingData verifies the device has stopped talking now that a
// complete response has been collected.
//
// Without this, a desynchronized link is invisible: reading only as many bytes
// as the command expects would quietly slice the front off some other, longer
// reply and hand it back as valid data. Costing one short read timeout per
// exchange is a good trade for never publishing a fabricated reading.
func (d *Device) checkNoTrailingData(cmd Command) error {
	var scratch [64]byte
	n, err := d.port.Read(scratch[:])
	switch {
	case errors.Is(err, serialport.ErrTimeout):
		// The expected case: the device finished exactly on the boundary.
		return nil
	case err != nil:
		return fmt.Errorf("gmc: %s: checking for trailing data: %w", cmd.Name, err)
	case n > 0:
		// Extra bytes mean this reply is not the one we asked for. Clear the
		// line so the next exchange starts from a known state.
		_ = d.port.Drain()
		return fmt.Errorf("gmc: %s: %d trailing byte(s) after a complete %d-byte response: %w",
			cmd.Name, n, cmd.ResponseBytes, ErrDesynchronized)
	default:
		return nil
	}
}
