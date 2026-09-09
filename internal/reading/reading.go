// Package reading defines the measurement produced by one poll of the device.
//
// It is deliberately free of protocol and transport concerns so that both the
// device layer and every sink can depend on it without depending on each other.
package reading

import (
	"math"
	"sync"
	"time"
)

// Reading is one complete sample from the Geiger counter.
//
// Fields that the device did not supply on this cycle are marked not-Valid
// rather than defaulted to zero. Zero is a legitimate value for a count rate,
// so a sink must be able to tell "the tube counted nothing" apart from "the
// temperature read failed and this number is meaningless".
type Reading struct {
	// Timestamp is when the poll cycle began, in UTC.
	Timestamp time.Time

	// CPM is counts per minute, straight from the device.
	CPM uint16

	// AverageCPM is the running mean count rate over the averaging window.
	AverageCPM float64

	// MicroSievertsPerHour is CPM converted through the device's calibration
	// table.
	MicroSievertsPerHour float64

	// Voltage is the battery voltage. Optional: some models omit it.
	Voltage Optional[float64]

	// TemperatureC is the internal temperature in degrees Celsius. Optional:
	// GQ-RFC1201 lists it as GMC-320 Re.3.01 or later.
	TemperatureC Optional[float64]
}

// Optional carries a value that may not have been read this cycle.
type Optional[T any] struct {
	Value T
	Valid bool
}

// Some returns a populated Optional.
func Some[T any](v T) Optional[T] { return Optional[T]{Value: v, Valid: true} }

// None returns an empty Optional.
func None[T any]() Optional[T] { return Optional[T]{} }

// Or returns the value if present, otherwise the supplied fallback.
func (o Optional[T]) Or(fallback T) T {
	if o.Valid {
		return o.Value
	}
	return fallback
}

// Average keeps a running mean of the most recent count rates.
//
// The window is bounded so the average tracks current conditions instead of
// being anchored by readings from days ago, and so memory use is fixed for a
// process expected to run for months.
type Average struct {
	mu     sync.Mutex
	window []float64
	size   int
	next   int
	filled bool
}

// NewAverage creates a running mean over at most size samples. A size below 1
// is treated as 1.
func NewAverage(size int) *Average {
	if size < 1 {
		size = 1
	}
	return &Average{window: make([]float64, size), size: size}
}

// Add records a sample and returns the updated mean.
//
// A non-finite sample is rejected rather than stored. Storing one would poison
// every subsequent mean until it rotated out of the window, which at the
// default settings is sixty polls, or an hour of suppressed averages from a
// single bad reading. Returning zero in the meantime would be worse than
// useless, because zero is also a legitimate count rate and a dashboard could
// not tell the two apart.
func (a *Average) Add(v float64) float64 {
	a.mu.Lock()
	defer a.mu.Unlock()

	if math.IsNaN(v) || math.IsInf(v, 0) {
		return a.meanLocked()
	}

	a.window[a.next] = v
	a.next = (a.next + 1) % a.size
	if a.next == 0 {
		a.filled = true
	}
	return a.meanLocked()
}

// Mean returns the current mean without recording a sample. It is zero before
// the first sample.
func (a *Average) Mean() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.meanLocked()
}

// Count returns how many samples currently contribute to the mean.
func (a *Average) Count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.countLocked()
}

func (a *Average) countLocked() int {
	if a.filled {
		return a.size
	}
	return a.next
}

func (a *Average) meanLocked() float64 {
	n := a.countLocked()
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < n; i++ {
		sum += a.window[i]
	}
	mean := sum / float64(n)
	if math.IsNaN(mean) || math.IsInf(mean, 0) {
		return 0
	}
	return mean
}
