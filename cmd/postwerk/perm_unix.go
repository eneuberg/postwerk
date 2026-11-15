//go:build unix

package main

import (
	"os"
	"slices"
	"syscall"
)

// ownGroup reports whether the file's group is one of the process's groups.
func ownGroup(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	gid := int(st.Gid)
	if gid == os.Getgid() {
		return true
	}
	groups, err := os.Getgroups()
	return err == nil && slices.Contains(groups, gid)
}
