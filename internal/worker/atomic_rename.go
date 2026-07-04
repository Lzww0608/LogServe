//go:build !windows

package worker

// This file provides the POSIX implementation for replacing checkpoint manifest
// files after they have been written to a sibling temporary path.

import "os"

// replaceFileWithRename atomically swaps tmpPath into targetPath on POSIX-like systems, replacing any existing target.
func replaceFileWithRename(tmpPath, targetPath string) error {
	return os.Rename(tmpPath, targetPath)
}
