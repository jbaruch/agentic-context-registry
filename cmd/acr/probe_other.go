//go:build !darwin && !linux

package main

// isTerminal reports non-interactive on every platform without a termios ioctl
// this binary knows how to issue. Refusing with a remedy flag named is the safe
// answer; asking a question nobody can answer is not.
func isTerminal(uintptr) bool {
	return false
}
