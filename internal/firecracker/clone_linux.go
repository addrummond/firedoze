package firecracker

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func tryCloneFile(out *os.File, in *os.File) (bool, error) {
	info, err := in.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err != nil {
		if isCloneUnsupported(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func isCloneUnsupported(err error) bool {
	return errors.Is(err, unix.EINVAL) ||
		errors.Is(err, unix.ENOTTY) ||
		errors.Is(err, unix.EOPNOTSUPP) ||
		errors.Is(err, unix.ENOSYS) ||
		errors.Is(err, unix.EXDEV)
}
