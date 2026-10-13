//go:build linux && (amd64 || arm64)

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// Linux renameat2(RENAME_NOREPLACE) makes destination existence checks atomic.
// Use syscall number by architecture; unsupported platforms fail explicitly.
func renameNoReplace(source, destination string) error {
	src, err := syscall.BytePtrFromString(source)
	if err != nil {
		return err
	}
	dst, err := syscall.BytePtrFromString(destination)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall6(renameat2Number, ^uintptr(99), uintptr(unsafe.Pointer(src)), ^uintptr(99), uintptr(unsafe.Pointer(dst)), 1, 0)
	if errno != 0 {
		return &os.LinkError{Op: "rename without replace", Old: source, New: destination, Err: errno}
	}
	return nil
}
