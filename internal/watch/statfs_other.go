//go:build !(linux || darwin || freebsd)

package watch

// statfs is not available here; the free-space check turns itself off.
func statfs(string) (free, total uint64, err error) { return 0, 0, ErrNoStatfs }
