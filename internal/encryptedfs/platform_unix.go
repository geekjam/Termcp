//go:build !windows

package encryptedfs

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func lockWriter(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return ErrBusy
	}
	return err
}

func unlockWriter(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}

func syncParent(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func replaceFile(from, to string) error {
	_, err := publishFile(from, to)
	return err
}

func publishFile(from, to string) (bool, error) {
	if err := os.Rename(from, to); err != nil {
		return false, err
	}
	return true, syncParent(to)
}
