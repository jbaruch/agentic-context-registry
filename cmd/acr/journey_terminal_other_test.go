//go:build !darwin && !linux

package main

// terminalJourneys is empty where this package has no pseudo-terminal helper.
// The supported platforms are darwin and linux, and both run the real thing.
func terminalJourneys() []journeyCase { return nil }
