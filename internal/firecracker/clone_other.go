//go:build !linux

package firecracker

import "os"

func tryCloneFile(out *os.File, in *os.File) (bool, error) {
	return false, nil
}
