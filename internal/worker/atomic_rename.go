//go:build !windows

package worker

// This file provides the POSIX implementation for replacing checkpoint manifest
// files after they have been written to a sibling temporary path.

import "os"

// replaceFileWithRename atomically swaps tmpPath into targetPath on POSIX-like systems, replacing any existing target.
func replaceFileWithRename(tmpPath, targetPath string) error {
	// writeCheckpointManifest creates tmpPath beside targetPath, so POSIX rename
	// stays within one filesystem and provides an atomic replacement.
	return os.Rename(tmpPath, targetPath)
}
