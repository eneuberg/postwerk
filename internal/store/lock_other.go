//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package store

import "os"

// LockSupported reports whether Open takes the data lock on this platform.
// Here it does not: the lock file is created, but nothing stops a second
// process on the same database (D61).
const LockSupported = false

func tryLock(*os.File) (busy bool, err error) { return false, nil }
