package gmc

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/RealDougEubanks/gmc-exporter/internal/serialport"
)

// hardwareEnv names the device node to run the hardware tests against.
//
// These tests are opt-in and skipped by default, so CI stays green on runners
// with no Geiger counter attached. Everything they check is also covered by the
// fixture-replay tests; the value here is confirming that the fixtures still
// describe the device in front of you.
//
//	GOGMC_SERIAL=/dev/ttyUSB0 go test ./internal/gmc/ -run TestHardware -v
//
// The serial port is exclusive. Stop anything else using the device first, or
// the two processes will interleave and produce output that looks exactly like
// a protocol bug.
const hardwareEnv = "GOGMC_SERIAL"

// openHardware connects to the real device, or skips the test.
func openHardware(t *testing.T) *Device {
	t.Helper()

	path := os.Getenv(hardwareEnv)
	if path == "" {
		t.Skipf("set %s=/dev/ttyUSB0 to run the hardware tests", hardwareEnv)
	}

	port, err := serialport.Open(serialport.Config{
		Path:        path,
		Baud:        serialport.DefaultBaud,
		ReadTimeout: serialport.DefaultReadTimeout,
	})
	if err != nil {
		t.Fatalf("opening %s: %v (is another process holding the port?)", path, err)
	}
	t.Cleanup(func() {
		if c, ok := port.(interface{ Close() error }); ok {
			_ = c.Close()
		}
	})

	return NewDevice(port, DefaultResponseTimeout)
}

// TestHardwareResponseLengths confirms the attached device still returns the
// lengths this package relies on.
//
// If this fails, the device disagrees with the implementation and the device
// wins: update the command table and re-record the fixtures.
func TestHardwareResponseLengths(t *testing.T) {
	dev := openHardware(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, cmd := range []Command{
		CmdGetVersion, CmdGetCPM, CmdGetVoltage, CmdGetConfig,
		CmdGetSerial, CmdGetDateTime, CmdGetTemperature, CmdGetGyro,
	} {
		t.Run(cmd.Name, func(t *testing.T) {
			raw, err := dev.Exchange(ctx, cmd)
			if err != nil {
				t.Fatalf("%s: %v", cmd.Name, err)
			}
			if len(raw) != cmd.ResponseBytes {
				t.Fatalf("%s returned %d bytes, expected %d (GQ-RFC1201 section %s)",
					cmd.Name, len(raw), cmd.ResponseBytes, cmd.SpecSection)
			}
			t.Logf("%s: %d bytes, %x", cmd.Name, len(raw), raw)
		})
	}
}

// TestHardwareReadings takes a live reading and sanity-checks it.
//
// The bounds are deliberately wide. This is checking that the decoders produce
// physically plausible values, not that the environment has any particular
// radiation level.
func TestHardwareReadings(t *testing.T) {
	dev := openHardware(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	version, err := dev.Exchange(ctx, CmdGetVersion)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	v, err := ParseVersion(version)
	if err != nil {
		t.Fatalf("parse version: %v", err)
	}
	t.Logf("device: model=%q firmware=%q", v.Model, v.Firmware)

	cfg, err := dev.Exchange(ctx, CmdGetConfig)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	cal, err := ParseCalibration(cfg)
	if err != nil {
		t.Fatalf("parse calibration: %v (the config block layout is measured, "+
			"not specified, so a different firmware may store it elsewhere)", err)
	}
	t.Logf("calibration: %s", cal)

	raw, err := dev.Exchange(ctx, CmdGetCPM)
	if err != nil {
		t.Fatalf("cpm: %v", err)
	}
	cpm, err := ParseCPM(raw)
	if err != nil {
		t.Fatalf("parse cpm: %v", err)
	}
	usv := cal.MicroSievertsPerHour(float64(cpm))
	t.Logf("reading: %d CPM = %.4f uSv/h", cpm, usv)

	// Background is typically tens of CPM. Four figures means something is
	// wrong with the decode, or you have a genuine problem.
	if cpm > 10000 {
		t.Errorf("CPM of %d is implausibly high; check the decode", cpm)
	}

	raw, err = dev.Exchange(ctx, CmdGetVoltage)
	if err != nil {
		t.Fatalf("voltage: %v", err)
	}
	volts, err := ParseVoltage(raw)
	if err != nil {
		t.Fatalf("parse voltage: %v", err)
	}
	t.Logf("battery: %.1f V", volts)
	if volts < 1.0 || volts > 15.0 {
		t.Errorf("voltage of %.1f V is outside any plausible range for this hardware", volts)
	}

	raw, err = dev.Exchange(ctx, CmdGetTemperature)
	if err != nil {
		t.Fatalf("temperature: %v", err)
	}
	temp, err := ParseTemperature(raw)
	if err != nil {
		t.Fatalf("parse temperature: %v", err)
	}
	t.Logf("temperature: %.1f C", temp)
	if temp < -40 || temp > 85 {
		t.Errorf("temperature of %.1f C is outside the sensor's operating range", temp)
	}
}

// TestHardwareRepeatedPolling looks for the intermittent faults that only show
// up over many exchanges.
//
// Aggressive back-to-back polling is what surfaced the desynchronised replies
// recorded in the fixtures, at roughly 1 in 400. This reports what it sees
// rather than failing on it: intermittent faults are the device's normal
// behaviour, and the implementation's job is to survive them, which it does by
// skipping the reading.
func TestHardwareRepeatedPolling(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping repeated polling in short mode")
	}

	dev := openHardware(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const iterations = 200
	var ok, lengthErrors, desyncs, absent, other int

	for i := 0; i < iterations; i++ {
		_, err := dev.Exchange(ctx, CmdGetCPM)
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrDesynchronized):
			desyncs++
		case errors.Is(err, ErrNoResponse):
			absent++
		case errors.Is(err, ErrUnexpectedLength):
			lengthErrors++
		default:
			other++
			t.Logf("iteration %d: %v", i, err)
		}
	}

	t.Logf("%d exchanges: %d ok, %d wrong length, %d desynchronised, %d no response, %d other",
		iterations, ok, lengthErrors, desyncs, absent, other)

	if ok == 0 {
		t.Fatal("no exchange succeeded; the device is not responding correctly")
	}
	// Anything above a few percent suggests a cabling, power or driver
	// problem rather than the occasional glitch that is expected here.
	if failures := iterations - ok; failures*100/iterations > 5 {
		t.Errorf("%d of %d exchanges failed, which is well above the ~0.5%% "+
			"measured on healthy hardware", failures, iterations)
	}
}
