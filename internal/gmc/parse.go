package gmc

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// terminator is the 0xAA byte GQ-RFC1201 places at the end of several
// responses (sections 23, 24 and 25).
const terminator = 0xAA

// checkLength rejects a buffer that is not exactly the expected size.
//
// Every parser calls this before touching a single index. That ordering is the
// whole point: indexing a response that arrived short is the most likely way
// for a program like this to die, and it is entirely preventable.
func checkLength(name string, b []byte, want int) error {
	if len(b) != want {
		return &LengthError{Command: name, Want: want, Got: len(b)}
	}
	return nil
}

// checkTerminator verifies the trailing 0xAA on responses that carry one.
func checkTerminator(name string, b []byte) error {
	last := b[len(b)-1]
	if last != terminator {
		return fmt.Errorf("gmc: %s: last byte is 0x%02X, expected 0x%02X: %w",
			name, last, terminator, ErrBadTerminator)
	}
	return nil
}

// ParseCPM decodes a <GETCPM>> reply.
//
// GQ-RFC1201 section 2: a 16-bit unsigned integer, most significant byte
// first. There is no terminator byte on this response, so length is the only
// structural check available and the trailing-data check in Exchange is what
// guards against a desynchronized reply being accepted.
func ParseCPM(b []byte) (uint16, error) {
	if err := checkLength(CmdGetCPM.Name, b, CmdGetCPM.ResponseBytes); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b), nil
}

// ParseVoltage decodes a <GETVOLT>> reply into volts.
//
// GQ-RFC1201 section 5 describes a single byte holding the voltage times ten;
// its worked example is 0x62 (98) meaning 9.8V. The attached GMC-320 returns
// 0x2A (42) for its 4.2V lithium cell, which matches the same rule.
func ParseVoltage(b []byte) (float64, error) {
	if err := checkLength(CmdGetVoltage.Name, b, CmdGetVoltage.ResponseBytes); err != nil {
		return 0, err
	}
	return float64(b[0]) / 10.0, nil
}

// ParseTemperature decodes a <GETTEMP>> reply into degrees Celsius.
//
// GQ-RFC1201 section 24: byte 0 is the integer part, byte 1 the decimal part,
// byte 2 is a sign flag where any non-zero value means the temperature is
// below zero, and byte 3 is always 0xAA.
func ParseTemperature(b []byte) (float64, error) {
	if err := checkLength(CmdGetTemperature.Name, b, CmdGetTemperature.ResponseBytes); err != nil {
		return 0, err
	}
	if err := checkTerminator(CmdGetTemperature.Name, b); err != nil {
		return 0, err
	}

	celsius := float64(b[0]) + float64(b[1])/10.0
	if b[2] != 0 {
		celsius = -celsius
	}
	return celsius, nil
}

// Gyro is a three-axis reading from the device's accelerometer.
//
// GQ-RFC1201 section 25 documents the framing but not the units or the
// reference frame, so these are reported as raw device counts.
type Gyro struct {
	X, Y, Z int16
}

// ParseGyro decodes a <GETGYRO>> reply.
//
// GQ-RFC1201 section 25: three 16-bit big-endian values followed by 0xAA.
func ParseGyro(b []byte) (Gyro, error) {
	if err := checkLength(CmdGetGyro.Name, b, CmdGetGyro.ResponseBytes); err != nil {
		return Gyro{}, err
	}
	if err := checkTerminator(CmdGetGyro.Name, b); err != nil {
		return Gyro{}, err
	}
	return Gyro{
		X: int16(binary.BigEndian.Uint16(b[0:2])),
		Y: int16(binary.BigEndian.Uint16(b[2:4])),
		Z: int16(binary.BigEndian.Uint16(b[4:6])),
	}, nil
}

// ParseDateTime decodes a <GETDATETIME>> reply.
//
// GQ-RFC1201 section 23: YY MM DD HH MM SS then 0xAA, where YY is the year
// within the 2000s. The device has no timezone concept, so the caller supplies
// the location to interpret the wall-clock values in.
func ParseDateTime(b []byte, loc *time.Location) (time.Time, error) {
	if err := checkLength(CmdGetDateTime.Name, b, CmdGetDateTime.ResponseBytes); err != nil {
		return time.Time{}, err
	}
	if err := checkTerminator(CmdGetDateTime.Name, b); err != nil {
		return time.Time{}, err
	}
	if loc == nil {
		loc = time.UTC
	}

	year, month, day := 2000+int(b[0]), int(b[1]), int(b[2])
	hour, minute, second := int(b[3]), int(b[4]), int(b[5])

	if month < 1 || month > 12 || day < 1 || day > 31 ||
		hour > 23 || minute > 59 || second > 59 {
		return time.Time{}, fmt.Errorf(
			"gmc: %s: device reported an impossible timestamp %04d-%02d-%02d %02d:%02d:%02d",
			CmdGetDateTime.Name, year, month, day, hour, minute, second)
	}
	return time.Date(year, time.Month(month), day, hour, minute, second, 0, loc), nil
}

// Version is the model and firmware revision reported by <GETVER>>.
type Version struct {
	// Model is the first 7 bytes, e.g. "GMC-320".
	Model string
	// Firmware is the remaining 7 bytes, e.g. "Re 3.03".
	Firmware string
	// Raw is the full 14-byte string as reported.
	Raw string
}

// ParseVersion decodes a <GETVER>> reply.
//
// GQ-RFC1201 section 1: 14 ASCII bytes, 7 of hardware model followed by 7 of
// firmware version, with "GMC-300Re 2.10" as the worked example.
func ParseVersion(b []byte) (Version, error) {
	if err := checkLength(CmdGetVersion.Name, b, CmdGetVersion.ResponseBytes); err != nil {
		return Version{}, err
	}
	raw := string(b)
	return Version{
		Model:    strings.TrimSpace(raw[:7]),
		Firmware: strings.TrimSpace(raw[7:]),
		Raw:      strings.TrimSpace(raw),
	}, nil
}

// ParseSerial decodes a <GETSERIAL>> reply into a hexadecimal string.
//
// GQ-RFC1201 section 11 states the length but not the encoding, and the bytes
// are not printable ASCII on the attached device, so they are rendered as hex.
func ParseSerial(b []byte) (string, error) {
	if err := checkLength(CmdGetSerial.Name, b, CmdGetSerial.ResponseBytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
