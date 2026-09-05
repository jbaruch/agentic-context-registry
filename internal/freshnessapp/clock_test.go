package freshnessapp

import (
	"testing"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/freshness"
)

// advancingClock reads from a fixed start and moves on by one second per call,
// so a test can tell a clock that is being read from one that is not without
// consulting the machine's clock.
func advancingClock(start time.Time) freshness.Clock {
	readings := 0
	return func() time.Time {
		reading := start.Add(time.Duration(readings) * time.Second)
		readings++
		return reading
	}
}

// TestWithClockReplacesOnlyAnExplicitClock keeps the injected seam from
// changing what the shipped binary composes. Every reading comes from a clock
// the test controls, and the assertions are on what the composed application
// reads rather than on which function it holds, so an internal rewiring that
// changes no reading leaves the test passing.
func TestWithClockReplacesOnlyAnExplicitClock(t *testing.T) {
	t.Setenv("ACR_STATE_HOME", t.TempDir())

	// A nil option leaves the shipped composition with the clock it builds for
	// itself rather than blanking it.
	if NewApplication(nil).runner.clock == nil {
		t.Fatal("the shipped application composed no clock")
	}
	if NewApplication(nil, WithClock(nil)).runner.clock == nil {
		t.Fatal("a nil clock option left the application with no clock")
	}

	fixed := time.Date(2001, time.February, 3, 4, 5, 6, 0, time.UTC)
	injected := NewApplication(nil, WithClock(func() time.Time { return fixed }))
	if reading := injected.runner.clock(); !reading.Equal(fixed) {
		t.Fatalf("the injected application reads %s, want %s", reading, fixed)
	}

	// A nil option after an explicit clock ignores the nil, not the clock: the
	// composed application keeps following the controlled source, reading by
	// reading.
	kept := NewApplication(nil, WithClock(advancingClock(fixed)), WithClock(nil))
	for step := range 3 {
		want := fixed.Add(time.Duration(step) * time.Second)
		if reading := kept.runner.clock(); !reading.Equal(want) {
			t.Fatalf("reading %d after a nil option is %s, want %s", step, reading, want)
		}
	}

	// The last explicit clock wins, and the application follows that one alone.
	later := fixed.Add(72 * time.Hour)
	replaced := NewApplication(nil,
		WithClock(advancingClock(fixed)),
		WithClock(advancingClock(later)),
	)
	for step := range 3 {
		want := later.Add(time.Duration(step) * time.Second)
		if reading := replaced.runner.clock(); !reading.Equal(want) {
			t.Fatalf("reading %d after a replacement is %s, want %s", step, reading, want)
		}
	}
}

// TestNewApplicationIgnoresANilOption keeps a conditionally assembled option
// slice from turning into a panic at construction.
func TestNewApplicationIgnoresANilOption(t *testing.T) {
	t.Setenv("ACR_STATE_HOME", t.TempDir())

	application := NewApplication(nil, nil)
	if application == nil {
		t.Fatal("NewApplication with a nil option returned no application")
	}
	if application.runner.clock == nil {
		t.Fatal("NewApplication with a nil option composed no clock")
	}
}
