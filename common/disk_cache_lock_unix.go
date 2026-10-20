//go:build !windows

package common

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func openDiskCacheLockFile(path string, create bool) (*os.File, error) {
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if create {
		flags |= unix.O_CREAT
	}
	fd, err := unix.Open(path, flags, 0600)
	if err != nil {
		return nil, &os.PathError{Op: "open cache lock", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

func lockDiskCacheFile(file *os.File, nonblocking bool) (bool, error) {
	operation := unix.LOCK_EX
	if nonblocking {
		operation |= unix.LOCK_NB
	}
	for {
		err := unix.Flock(int(file.Fd()), operation)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.EWOULDBLOCK) {
			return false, nil
		}
		return err == nil, err
	}
}

func unlockDiskCacheFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
