package freshnessapp

import (
	"testing"
	"time"
)

// TestWithClockReplacesOnlyAnExplicitClock keeps the injected seam from
// changing what the shipped binary composes. Every assertion is on what a
// composed application's clock does, so wrapping the default in a function that
// reads the same source stays passing while a default that stopped reading a
// real clock, or a nil option that overwrote one, does not.
func TestWithClockReplacesOnlyAnExplicitClock(t *testing.T) {
	t.Setenv("ACR_STATE_HOME", t.TempDir())

	// The default is a live reading rather than a stub: it is not the zero
	// time, and it never runs backwards. Both hold on every machine and on
	// every date, so neither the suite's speed nor the calendar decides them.
	for _, name := range []string{"the default", "a nil clock option"} {
		options := []Option(nil)
		if name == "a nil clock option" {
			options = append(options, WithClock(nil))
		}
		clock := NewApplication(nil, options...).runner.clock
		if clock == nil {
			t.Fatalf("%s left the application with no clock", name)
		}
		first, second := clock(), clock()
		if first.IsZero() {
			t.Fatalf("%s reads the zero time", name)
		}
		if second.Before(first) {
			t.Fatalf("%s ran backwards: %s then %s", name, first, second)
		}
	}

	fixed := time.Date(2001, time.February, 3, 4, 5, 6, 0, time.UTC)
	injected := NewApplication(nil, WithClock(func() time.Time { return fixed }))
	if reading := injected.runner.clock(); !reading.Equal(fixed) {
		t.Fatalf("the injected application reads %s, want %s", reading, fixed)
	}

	// A nil option after an explicit clock ignores the nil, not the clock.
	kept := NewApplication(nil, WithClock(func() time.Time { return fixed }), WithClock(nil))
	if reading := kept.runner.clock(); !reading.Equal(fixed) {
		t.Fatalf("a nil option after an explicit clock reads %s, want %s", reading, fixed)
	}

	// The last explicit clock wins, and reading twice is stable.
	later := fixed.Add(72 * time.Hour)
	replaced := NewApplication(nil,
		WithClock(func() time.Time { return fixed }),
		WithClock(func() time.Time { return later }),
	)
	if first, second := replaced.runner.clock(), replaced.runner.clock(); !first.Equal(later) || !second.Equal(later) {
		t.Fatalf("the replaced clock reads %s then %s, want %s twice", first, second, later)
	}
}
