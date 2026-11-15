//go:build !unix

package main

import "os"

// ownGroup cannot tell the file's group here, so group access is reported.
func ownGroup(os.FileInfo) bool { return false }
