//go:build darwin

package main

import (
	"bytes"
	"os"
	"syscall"
	"testing"
	"unsafe"
)

// openTerminal allocates a pseudo-terminal and returns its slave side, which is
// a real terminal without the test needing a controlling one. Darwin's /dev/ptmx
// master answers no termios query until the pair is granted and unlocked, so the
// master is not a substitute for the slave here.
func openTerminal(t *testing.T) *os.File {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open /dev/ptmx: %v", err)
	}
	t.Cleanup(func() { master.Close() })
	for _, request := range []uintptr{syscall.TIOCPTYGRANT, syscall.TIOCPTYUNLK} {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), request, 0); errno != 0 {
			t.Fatalf("ioctl %#x on /dev/ptmx: %v", request, errno)
		}
	}
	var name [128]byte
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); errno != 0 {
		t.Fatalf("ioctl TIOCPTYGNAME: %v", errno)
	}
	path := string(name[:bytes.IndexByte(name[:], 0)])
	slave, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { slave.Close() })
	return slave
}
