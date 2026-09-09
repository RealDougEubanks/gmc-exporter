package reading

import (
	"math"
	"sync"
	"testing"
)

func TestOptionalSomeNoneOr(t *testing.T) {
	some := Some(4.2)
	if !some.Valid {
		t.Fatal("Some produced an invalid Optional")
	}
	if some.Value != 4.2 {
		t.Fatalf("Some value = %v, want 4.2", some.Value)
	}
	if got := some.Or(99); got != 4.2 {
		t.Fatalf("Or on a present value = %v, want 4.2", got)
	}

	none := None[float64]()
	if none.Valid {
		t.Fatal("None produced a valid Optional")
	}
	if got := none.Or(99); got != 99 {
		t.Fatalf("Or on an absent value = %v, want the fallback 99", got)
	}
}

// TestOptionalZeroDistinguishableFromAbsent is the reason Optional exists at
// all: zero is a legitimate reading, so a sink must be able to tell a measured
// zero apart from a value the device never supplied.
func TestOptionalZeroDistinguishableFromAbsent(t *testing.T) {
	measuredZero := Some(0.0)
	absent := None[float64]()

	if measuredZero.Value != absent.Value {
		t.Fatal("test premise is wrong: both values should be the zero value")
	}
	if !measuredZero.Valid {
		t.Fatal("a measured zero must be Valid")
	}
	if absent.Valid {
		t.Fatal("an absent value must not be Valid")
	}
	if got := measuredZero.Or(-1); got != 0 {
		t.Fatalf("Or on a measured zero = %v, want 0 rather than the fallback", got)
	}
	if got := absent.Or(-1); got != -1 {
		t.Fatalf("Or on an absent value = %v, want the fallback -1", got)
	}
}

func TestAverageMeanBeforeAnySample(t *testing.T) {
	a := NewAverage(10)
	if got := a.Mean(); got != 0 {
		t.Fatalf("Mean with no samples = %v, want 0", got)
	}
	if got := a.Count(); got != 0 {
		t.Fatalf("Count with no samples = %d, want 0", got)
	}
}

func TestAverageMeanAndCountWhileFilling(t *testing.T) {
	a := NewAverage(4)

	if got := a.Add(10); got != 10 {
		t.Fatalf("mean after one sample = %v, want 10", got)
	}
	if got := a.Count(); got != 1 {
		t.Fatalf("Count after one sample = %d, want 1", got)
	}

	a.Add(20)
	a.Add(30)
	if got := a.Mean(); got != 20 {
		t.Fatalf("mean of 10,20,30 = %v, want 20", got)
	}
	if got := a.Count(); got != 3 {
		t.Fatalf("Count after three samples = %d, want 3", got)
	}

	a.Add(40)
	if got := a.Count(); got != 4 {
		t.Fatalf("Count once full = %d, want 4", got)
	}
	if got := a.Mean(); got != 25 {
		t.Fatalf("mean of 10,20,30,40 = %v, want 25", got)
	}
}

// TestAverageWindowWraps proves the window really is bounded: once it is full,
// the oldest sample must stop contributing rather than the mean being anchored
// by readings from hours ago.
func TestAverageWindowWraps(t *testing.T) {
	a := NewAverage(3)
	a.Add(1)
	a.Add(2)
	a.Add(3)
	if got := a.Mean(); got != 2 {
		t.Fatalf("mean of 1,2,3 = %v, want 2", got)
	}

	// 1 falls out of the window here, leaving 2,3,4.
	a.Add(4)
	if got := a.Mean(); got != 3 {
		t.Fatalf("mean after wrapping = %v, want 3 (1 should have fallen out)", got)
	}
	if got := a.Count(); got != 3 {
		t.Fatalf("Count after wrapping = %d, want 3", got)
	}

	// Two more wraps: the window should hold only 100 values.
	for i := 0; i < 6; i++ {
		a.Add(100)
	}
	if got := a.Mean(); got != 100 {
		t.Fatalf("mean after several wraps = %v, want 100", got)
	}
}

func TestAverageSizeBelowOneIsClamped(t *testing.T) {
	for _, size := range []int{0, -1, -100} {
		a := NewAverage(size)
		if a.size != 1 {
			t.Fatalf("NewAverage(%d) size = %d, want 1", size, a.size)
		}
		a.Add(7)
		if got := a.Mean(); got != 7 {
			t.Fatalf("NewAverage(%d) mean = %v, want 7", size, got)
		}
		a.Add(9)
		if got := a.Mean(); got != 9 {
			t.Fatalf("NewAverage(%d) mean after second sample = %v, want 9", size, got)
		}
		if got := a.Count(); got != 1 {
			t.Fatalf("NewAverage(%d) count = %d, want 1", size, got)
		}
	}
}

// TestAverageNonFiniteSamples checks that a bad sample cannot leave the mean
// permanently NaN. A NaN reported to a sink would poison a dashboard long after
// the device recovered.
func TestAverageNonFiniteSamples(t *testing.T) {
	cases := []struct {
		name  string
		value float64
	}{
		{"NaN", math.NaN()},
		{"positive infinity", math.Inf(1)},
		{"negative infinity", math.Inf(-1)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAverage(3)
			a.Add(10)
			a.Add(20)

			if got := a.Add(tc.value); math.IsNaN(got) || math.IsInf(got, 0) {
				t.Fatalf("mean including %s = %v, want a finite value", tc.name, got)
			}
			if got := a.Mean(); math.IsNaN(got) || math.IsInf(got, 0) {
				t.Fatalf("Mean including %s = %v, want a finite value", tc.name, got)
			}

			// Once the bad sample has been pushed out of the window the mean
			// must recover rather than stay poisoned.
			a.Add(30)
			a.Add(30)
			a.Add(30)
			if got := a.Mean(); got != 30 {
				t.Fatalf("mean after %s left the window = %v, want 30", tc.name, got)
			}
		})
	}
}

// TestAverageConcurrentAccess exercises the mutex under -race; the poll loop
// writes while the HTTP handlers read.
func TestAverageConcurrentAccess(t *testing.T) {
	a := NewAverage(16)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				a.Add(float64(g*i%17) + 1)
				_ = a.Mean()
				_ = a.Count()
			}
		}(g)
	}
	wg.Wait()

	if got := a.Count(); got != 16 {
		t.Fatalf("Count after concurrent use = %d, want 16", got)
	}
	if got := a.Mean(); math.IsNaN(got) || got <= 0 {
		t.Fatalf("Mean after concurrent use = %v, want a positive finite value", got)
	}
}

// TestAverageRejectsNonFiniteSamples pins that one bad sample cannot suppress
// the mean for a whole window.
//
// Storing a NaN would make every subsequent mean non-finite until it rotated
// out, which at the default settings is sixty polls. Reporting zero instead
// would be no better, because zero is a legitimate count rate and nothing
// downstream could tell the two apart.
func TestAverageRejectsNonFiniteSamples(t *testing.T) {
	t.Parallel()

	avg := NewAverage(4)
	avg.Add(10)
	avg.Add(20)

	before := avg.Mean()
	if before != 15 {
		t.Fatalf("mean = %v, want 15", before)
	}

	for _, bad := range []float64{
		math.NaN(),
		math.Inf(1),
		math.Inf(-1),
	} {
		got := avg.Add(bad)
		if math.IsNaN(got) || math.IsInf(got, 0) {
			t.Fatalf("Add(%v) returned a non-finite mean %v", bad, got)
		}
		if got != before {
			t.Fatalf("Add(%v) changed the mean from %v to %v; a rejected sample must not count",
				bad, before, got)
		}
	}

	// The rejected samples must not have consumed slots in the window.
	if n := avg.Count(); n != 2 {
		t.Fatalf("Count() = %d, want 2; non-finite samples should not occupy the window", n)
	}

	// Good samples still work afterwards.
	if got := avg.Add(30); got != 20 {
		t.Fatalf("mean after recovery = %v, want 20", got)
	}
}
