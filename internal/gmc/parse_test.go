package gmc

import (
	"errors"
	"testing"
	"time"
)

// TestParseTemperatureRejectsObservedCorruption pins a real corrupted reply.
//
// During a live run against the hardware, a device that had reported 30.8 C on
// three consecutive polls returned 94.8 C on the fourth:
//
//	expected  1e 08 00 aa   ->  30.8 C
//	received  5e 08 00 aa   ->  94.8 C
//
// One flipped bit, 0x1E against 0x5E. The link is 8N1 with no parity, so
// nothing structural catches it: the length is right and the 0xAA terminator
// is right. Only a plausibility bound rejects it, and without one a fabricated
// 94.8 C would have been published to the user's monitoring history.
func TestParseTemperatureRejectsObservedCorruption(t *testing.T) {
	t.Parallel()

	corrupted := []byte{0x5e, 0x08, 0x00, 0xAA}
	if _, err := ParseTemperature(corrupted); !errors.Is(err, ErrImplausibleValue) {
		t.Fatalf("ParseTemperature(% x) = %v, want ErrImplausibleValue", corrupted, err)
	}

	// The uncorrupted form of the same reading must still parse.
	expected := []byte{0x1e, 0x08, 0x00, 0xAA}
	got, err := ParseTemperature(expected)
	if err != nil {
		t.Fatalf("ParseTemperature(% x): %v", expected, err)
	}
	if got != 30.8 {
		t.Fatalf("ParseTemperature(% x) = %v, want 30.8", expected, got)
	}
}

// TestParseTemperature covers the decoding rules in GQ-RFC1201 section 24.
func TestParseTemperature(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      []byte
		want    float64
		wantErr error
	}{
		// Recorded from the attached GMC-320.
		{name: "captured 31.1C", in: []byte{0x1f, 0x01, 0x00, 0xAA}, want: 31.1},
		{name: "captured 31.0C", in: []byte{0x1f, 0x00, 0x00, 0xAA}, want: 31.0},
		{name: "zero", in: []byte{0x00, 0x00, 0x00, 0xAA}, want: 0},
		// Section 24: byte 2 non-zero means below zero.
		{name: "negative", in: []byte{0x05, 0x05, 0x01, 0xAA}, want: -5.5},
		{name: "negative with any non-zero sign", in: []byte{0x0a, 0x00, 0xff, 0xAA}, want: -10},
		{name: "upper bound accepted", in: []byte{0x55, 0x00, 0x00, 0xAA}, want: 85},

		{name: "short read", in: []byte{0x1f, 0x01}, wantErr: ErrUnexpectedLength},
		{name: "empty", in: nil, wantErr: ErrUnexpectedLength},
		{name: "too long", in: []byte{0x1f, 0x01, 0x00, 0xAA, 0x00}, wantErr: ErrUnexpectedLength},
		{name: "bad terminator", in: []byte{0x1f, 0x01, 0x00, 0x00}, wantErr: ErrBadTerminator},
		{name: "decimal byte is not a digit", in: []byte{0x1f, 0x63, 0x00, 0xAA}, wantErr: ErrImplausibleValue},
		{name: "above plausible range", in: []byte{0x64, 0x00, 0x00, 0xAA}, wantErr: ErrImplausibleValue},
		{name: "below plausible range", in: []byte{0x50, 0x00, 0x01, 0xAA}, wantErr: ErrImplausibleValue},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseTemperature(tc.in)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got error %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestParseVoltage covers section 5, including the specification's own worked
// example and the value the attached device reports.
func TestParseVoltage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      []byte
		want    float64
		wantErr error
	}{
		// GQ-RFC1201 section 5 states 0x62 means 9.8V.
		{name: "spec example 9.8V", in: []byte{0x62}, want: 9.8},
		// Recorded from the attached GMC-320's lithium cell.
		{name: "captured 4.2V", in: []byte{0x2a}, want: 4.2},
		{name: "flat battery", in: []byte{0x0a}, want: 1.0},

		{name: "empty", in: nil, wantErr: ErrUnexpectedLength},
		{name: "too long", in: []byte{0x2a, 0x00}, wantErr: ErrUnexpectedLength},
		// A single flipped bit turns 4.2V into 17.0V with nothing structural
		// to catch it, which is why the bound exists.
		{name: "bit flip to 17.0V", in: []byte{0xaa}, wantErr: ErrImplausibleValue},
		{name: "zero is not a running device", in: []byte{0x00}, wantErr: ErrImplausibleValue},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseVoltage(tc.in)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got error %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestParseCPM covers section 2's big-endian 16-bit count.
func TestParseCPM(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      []byte
		want    uint16
		wantErr error
	}{
		// The specification's worked example: 00 1C is 28 CPM.
		{name: "spec example", in: []byte{0x00, 0x1c}, want: 28},
		{name: "zero counts", in: []byte{0x00, 0x00}, want: 0},
		{name: "maximum", in: []byte{0xff, 0xff}, want: 65535},
		// Byte order matters: read little-endian this would be 256.
		{name: "big endian ordering", in: []byte{0x01, 0x00}, want: 256},

		// The single-byte case is the classic short read.
		{name: "short read", in: []byte{0x00}, wantErr: ErrUnexpectedLength},
		{name: "empty", in: nil, wantErr: ErrUnexpectedLength},
		{name: "too long", in: []byte{0x00, 0x1c, 0x00}, wantErr: ErrUnexpectedLength},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseCPM(tc.in)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got error %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestParseVersion covers section 1, including the specification's example and
// the attached device.
func TestParseVersion(t *testing.T) {
	t.Parallel()

	// Recorded from the attached device.
	got, err := ParseVersion([]byte("GMC-320Re 4.62"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Model != "GMC-320" || got.Firmware != "Re 4.62" {
		t.Fatalf("got model=%q firmware=%q, want %q and %q",
			got.Model, got.Firmware, "GMC-320", "Re 4.62")
	}

	// The specification's own example, which has a trailing space in the
	// model field.
	got, err = ParseVersion([]byte("GMC-300Re 2.10"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Model != "GMC-300" {
		t.Fatalf("got model=%q, want %q", got.Model, "GMC-300")
	}

	if _, err := ParseVersion([]byte("GMC-320")); !errors.Is(err, ErrUnexpectedLength) {
		t.Fatalf("got %v, want ErrUnexpectedLength", err)
	}
}

// TestParseDateTime covers section 23, including the rejection of a timestamp
// the device could not legitimately hold.
func TestParseDateTime(t *testing.T) {
	t.Parallel()

	got, err := ParseDateTime([]byte{26, 9, 8, 21, 4, 30, 0xAA}, time.UTC)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Date(2026, time.September, 8, 21, 4, 30, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %s, want %s", got, want)
	}

	// A nil location must not panic; UTC is the documented fallback.
	if _, err := ParseDateTime([]byte{26, 9, 8, 21, 4, 30, 0xAA}, nil); err != nil {
		t.Fatalf("nil location: %v", err)
	}

	for _, bad := range [][]byte{
		{26, 13, 8, 21, 4, 30, 0xAA}, // month 13
		{26, 9, 32, 21, 4, 30, 0xAA}, // day 32
		{26, 9, 8, 24, 4, 30, 0xAA},  // hour 24
		{26, 9, 8, 21, 60, 30, 0xAA}, // minute 60
		{26, 9, 0, 21, 4, 30, 0xAA},  // day 0
	} {
		if _, err := ParseDateTime(bad, time.UTC); err == nil {
			t.Errorf("ParseDateTime(% x) accepted an impossible timestamp", bad)
		}
	}

	if _, err := ParseDateTime([]byte{26, 9, 8, 21, 4, 30, 0x00}, time.UTC); !errors.Is(err, ErrBadTerminator) {
		t.Errorf("got %v, want ErrBadTerminator", err)
	}
}

// TestParseGyro covers section 25's three big-endian signed axes.
func TestParseGyro(t *testing.T) {
	t.Parallel()

	got, err := ParseGyro([]byte{0x00, 0x0a, 0xff, 0xf6, 0x01, 0x00, 0xAA})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.X != 10 || got.Y != -10 || got.Z != 256 {
		t.Fatalf("got %+v, want X=10 Y=-10 Z=256", got)
	}

	if _, err := ParseGyro([]byte{0x00, 0x0a, 0xff}); !errors.Is(err, ErrUnexpectedLength) {
		t.Errorf("got %v, want ErrUnexpectedLength", err)
	}
	if _, err := ParseGyro([]byte{0, 0, 0, 0, 0, 0, 0x00}); !errors.Is(err, ErrBadTerminator) {
		t.Errorf("got %v, want ErrBadTerminator", err)
	}
}

// TestParseSerial covers section 11.
func TestParseSerial(t *testing.T) {
	t.Parallel()

	// Recorded from the attached device. The bytes are not printable ASCII,
	// which is why they are rendered as hex.
	got, err := ParseSerial([]byte{0xf6, 0x28, 0xc4, 0x00, 0x09, 0x88, 0x8c})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "f628c40009888c" {
		t.Fatalf("got %q, want %q", got, "f628c40009888c")
	}

	if _, err := ParseSerial([]byte{0xf6, 0x28}); !errors.Is(err, ErrUnexpectedLength) {
		t.Errorf("got %v, want ErrUnexpectedLength", err)
	}
}

// TestCalibrationConversion checks the measured table and the interpolation
// around it.
func TestCalibrationConversion(t *testing.T) {
	t.Parallel()

	cal := Calibration{Points: []CalibrationPoint{
		{CPM: 60, MicroSievertsPerHour: 0.39},
		{CPM: 240, MicroSievertsPerHour: 1.56},
		{CPM: 1000, MicroSievertsPerHour: 6.5},
	}}

	// The device's table is linear at 0.0065 uSv/h per CPM, so every path
	// through the conversion should agree on that factor.
	tests := []struct {
		cpm  float64
		want float64
	}{
		{cpm: 0, want: 0},
		{cpm: 28, want: 0.182},  // below the first point
		{cpm: 60, want: 0.39},   // exactly the first point
		{cpm: 150, want: 0.975}, // between points
		{cpm: 1000, want: 6.5},  // exactly the last point
		{cpm: 2000, want: 13.0}, // extrapolated above the table
		{cpm: -5, want: 0},      // nonsense input must not produce nonsense
	}

	for _, tc := range tests {
		got := cal.MicroSievertsPerHour(tc.cpm)
		if diff := got - tc.want; diff > 0.0001 || diff < -0.0001 {
			t.Errorf("MicroSievertsPerHour(%v) = %v, want %v", tc.cpm, got, tc.want)
		}
	}

	// An empty table must return zero rather than dividing by zero.
	var empty Calibration
	if got := empty.MicroSievertsPerHour(100); got != 0 {
		t.Errorf("empty calibration returned %v, want 0", got)
	}
}

// TestParseCalibrationSpec covers the configuration override.
func TestParseCalibrationSpec(t *testing.T) {
	t.Parallel()

	cal, err := ParseCalibrationSpec("1000:6.5, 60:0.39,240:1.56")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Entries must be sorted by CPM regardless of the order supplied, since
	// interpolation depends on it.
	if len(cal.Points) != 3 || cal.Points[0].CPM != 60 || cal.Points[2].CPM != 1000 {
		t.Fatalf("got %+v, want three points sorted by CPM", cal.Points)
	}

	for _, bad := range []string{
		"",
		"not-a-calibration",
		"60",
		"60:",
		":0.39",
		"0:0.39",    // a zero CPM would divide by zero
		"60:0",      // a zero dose rate is not a calibration
		"60:-1",     // negative dose rate
		"70000:1.0", // beyond a uint16
		"60:notanumber",
	} {
		if _, err := ParseCalibrationSpec(bad); err == nil {
			t.Errorf("ParseCalibrationSpec(%q) was accepted", bad)
		}
	}
}
