package adaptertest

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// RunCommand runs a fresh command on each attempt, retrying only ETXTBSY.
// A parallel test's fork can inherit a writer to a newly realized executable
// until that child execs (https://go.dev/issue/22315). Like cmd/go, wait briefly
// for that descriptor to close without changing the invocation being tested.
// This helper is test support; production commands must not use it.
func RunCommand(command func() *exec.Cmd) error {
	for attempt := 0; ; attempt++ {
		err := command().Run()
		if !errors.Is(err, syscall.ETXTBSY) || attempt == 2 {
			return err
		}
		time.Sleep(100 * time.Millisecond << attempt)
	}
}
