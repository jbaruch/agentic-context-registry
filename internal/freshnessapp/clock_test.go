package freshnessapp

import (
	"testing"
	"time"
)

// TestWithClockReplacesOnlyAnExplicitClock keeps the injected seam from
// changing what the shipped binary composes: no option leaves the runner on
// time.Now, and a nil clock is ignored rather than stopping time.
func TestWithClockReplacesOnlyAnExplicitClock(t *testing.T) {
	t.Setenv("ACR_STATE_HOME", t.TempDir())

	shipped := NewApplication(nil)
	if shipped.runner.clock == nil {
		t.Fatal("the shipped application composed no clock")
	}
	before := time.Now()
	if reading := shipped.runner.clock(); reading.Before(before.Add(-time.Minute)) {
		t.Fatalf("the shipped application reads %s, want the wall clock", reading)
	}

	ignored := NewApplication(nil, WithClock(nil))
	if ignored.runner.clock == nil {
		t.Fatal("a nil clock replaced the default instead of being ignored")
	}

	fixed := time.Date(2001, time.February, 3, 4, 5, 6, 0, time.UTC)
	injected := NewApplication(nil, WithClock(func() time.Time { return fixed }))
	if reading := injected.runner.clock(); !reading.Equal(fixed) {
		t.Fatalf("the injected application reads %s, want %s", reading, fixed)
	}
}
