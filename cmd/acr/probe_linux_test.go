//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"
	"testing"
	"unsafe"
)

// openTerminal allocates a pseudo-terminal and returns its slave side, which is
// a real terminal without the test needing a controlling one. Linux unlocks the
// pair with TIOCSPTLCK and names the slave with TIOCGPTN.
func openTerminal(t *testing.T) *os.File {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open /dev/ptmx: %v", err)
	}
	t.Cleanup(func() { master.Close() })
	unlock := int32(0)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		t.Fatalf("ioctl TIOCSPTLCK: %v", errno)
	}
	number := uint32(0)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&number))); errno != 0 {
		t.Fatalf("ioctl TIOCGPTN: %v", errno)
	}
	path := fmt.Sprintf("/dev/pts/%d", number)
	slave, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { slave.Close() })
	return slave
}
