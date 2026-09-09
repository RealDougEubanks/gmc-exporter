// Package serialport provides a minimal Linux serial port implementation for
// talking to GQ Electronics GMC-series Geiger counters.
//
// It deliberately avoids third-party serial libraries. The GMC hardware is
// attached via a CH340/CH341 USB-serial bridge, which routinely returns partial
// reads, so the protocol layer needs precise control over read deadlines. Some
// popular Go serial libraries signal a read timeout as (0, nil), which turns
// io.ReadFull and similar helpers into a busy loop. This implementation returns
// an explicit ErrTimeout instead, so callers cannot make that mistake.
package serialport

import (
	"errors"
	"io"
	"time"
)

// ErrTimeout is returned by Read when no data arrived before the read timeout
// elapsed. It is a normal, recoverable condition: the caller should decide
// whether to keep waiting or abandon the current response.
var ErrTimeout = errors.New("serialport: read timeout")

// Port is the narrow interface the protocol layer depends on. Keeping it this
// small is what allows every protocol test to run against a fake, with no
// hardware attached.
type Port interface {
	io.ReadWriter

	// Drain discards any bytes currently buffered on the input side. It is
	// called before issuing a command so a stale or partial response from a
	// previous exchange cannot be mistaken for the current one.
	Drain() error
}

// Config describes how to open a serial port.
type Config struct {
	// Path is the device node, e.g. /dev/ttyUSB0.
	Path string

	// Baud is the line rate. The GMC-320 factory default is 115200.
	Baud int

	// ReadTimeout bounds a single Read call. A device that has stopped
	// responding must not block the poll loop forever.
	ReadTimeout time.Duration
}

// DefaultBaud is the GMC-320 factory default line rate, per GQ-RFC1201
// "Serial Port configuration".
const DefaultBaud = 115200

// DefaultReadTimeout is the granularity of a single Read, not the time budget
// for a whole response. The protocol layer loops on Read until it has a
// complete reply, so this only controls how often that loop wakes up.
//
// It is kept short deliberately. After collecting a complete response the
// protocol layer performs one more Read to confirm the device has stopped
// talking, and that check costs exactly one of these timeouts on every
// exchange. Measured first-byte latency on a real GMC-320 peaked at 171ms, so
// 50ms means a normal response costs a handful of harmless loop iterations
// while the trailing-data check stays cheap.
const DefaultReadTimeout = 50 * time.Millisecond
