//go:build !windows

package worker

import "os"

func replaceFileWithRename(tmpPath, targetPath string) error {
	return os.Rename(tmpPath, targetPath)
}
