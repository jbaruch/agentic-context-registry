package freshnessapp

import (
	"reflect"
	"testing"
	"time"
)

// TestWithClockReplacesOnlyAnExplicitClock keeps the injected seam from
// changing what the shipped binary composes. The default is checked by identity
// rather than by reading it: what matters is that the runner is still wired to
// time.Now, and comparing a reading against the machine's clock would assert on
// wall-clock time to prove a wiring fact.
func TestWithClockReplacesOnlyAnExplicitClock(t *testing.T) {
	t.Setenv("ACR_STATE_HOME", t.TempDir())

	production := reflect.ValueOf(time.Now).Pointer()
	if wired := reflect.ValueOf(NewApplication(nil).runner.clock).Pointer(); wired != production {
		t.Fatal("the shipped application is no longer wired to time.Now")
	}
	if wired := reflect.ValueOf(NewApplication(nil, WithClock(nil)).runner.clock).Pointer(); wired != production {
		t.Fatal("a nil clock replaced the default instead of being ignored")
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
