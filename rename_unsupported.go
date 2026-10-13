//go:build !linux || (!amd64 && !arm64)

package main

import "fmt"

func renameNoReplace(source, destination string) error {
	return fmt.Errorf("atomic file-bundle extraction is supported on Linux amd64/arm64")
}
