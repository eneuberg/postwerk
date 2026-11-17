//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package store

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// LockSupported reports whether Open takes the data lock on this platform.
const LockSupported = true

// tryLock takes an exclusive flock on f without waiting. busy is true when
// another open file description holds it, in this process or another.
func tryLock(f *os.File) (busy bool, err error) {
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if errors.Is(err, unix.EWOULDBLOCK) {
		return true, nil
	}
	return false, err
}
