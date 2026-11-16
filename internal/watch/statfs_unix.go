//go:build linux || darwin || freebsd

package watch

import "syscall"

// statfs returns the bytes available to an unprivileged process and the
// size of the file system that holds dir.
func statfs(dir string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	// The field types differ between systems; uint64 fits all of them.
	bsize := uint64(st.Bsize)
	return uint64(st.Bavail) * bsize, uint64(st.Blocks) * bsize, nil
}
