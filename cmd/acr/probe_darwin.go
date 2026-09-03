//go:build darwin

package main

import (
	"syscall"
	"unsafe"
)

// isTerminal reports whether the descriptor answers Darwin's terminal-attribute
// ioctl, TIOCGETA. A pipe, a regular file, /dev/null, and the /dev/null the Go
// runtime opens into a closed standard descriptor all fail it with ENOTTY;
// that failure is the answer the probe wants, never a fault to report.
func isTerminal(descriptor uintptr) bool {
	var attributes syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, descriptor, syscall.TIOCGETA, uintptr(unsafe.Pointer(&attributes)))
	return errno == 0
}
