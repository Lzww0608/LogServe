//go:build windows

package worker

// This file provides the Windows implementation for replacing checkpoint
// manifest files, where os.Rename does not reliably overwrite an existing file.

import (
	"syscall"
	"unsafe"
)

// MoveFileEx flags request replacement plus write-through durability for manifest rewrites on Windows.
const (
	movefileReplaceExisting = 0x1
	movefileWriteThrough    = 0x8
)

// moveFileExW is loaded lazily so non-calling tests can link without resolving the Windows API early.
var moveFileExW = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

// replaceFileWithRename replaces targetPath with tmpPath using MoveFileExW because os.Rename cannot overwrite reliably on Windows.
func replaceFileWithRename(tmpPath, targetPath string) error {
	// Win32 file APIs expect nul-terminated UTF-16 paths; conversion errors are
	// returned before calling MoveFileExW so invalid Go strings are not truncated.
	from, err := syscall.UTF16PtrFromString(tmpPath)
	if err != nil {
		return err
	}
	to, err := syscall.UTF16PtrFromString(targetPath)
	if err != nil {
		return err
	}
	// MOVEFILE_REPLACE_EXISTING gives os.Rename-like overwrite semantics, while
	// MOVEFILE_WRITE_THROUGH asks Windows to flush the metadata update before returning.
	r1, _, callErr := moveFileExW.Call(
		uintptr(unsafe.Pointer(from)),
		uintptr(unsafe.Pointer(to)),
		uintptr(movefileReplaceExisting|movefileWriteThrough),
	)

	// MoveFileExW reports success as non-zero; syscall.Errno(0) is converted into a concrete EINVAL.
	if r1 == 0 {
		if callErr != syscall.Errno(0) {
			return callErr
		}
		return syscall.EINVAL
	}
	return nil
}
