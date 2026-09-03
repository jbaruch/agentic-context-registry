//go:build linux

package main

import (
	"syscall"
	"unsafe"
)

// isTerminal reports whether the descriptor answers Linux's terminal-attribute
// ioctl, TCGETS. A pipe, a regular file, /dev/null, and the /dev/null the Go
// runtime opens into a closed standard descriptor all fail it with ENOTTY;
// that failure is the answer the probe wants, never a fault to report.
func isTerminal(descriptor uintptr) bool {
	var attributes syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, descriptor, syscall.TCGETS, uintptr(unsafe.Pointer(&attributes)))
	return errno == 0
}
