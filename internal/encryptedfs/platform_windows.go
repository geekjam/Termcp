//go:build windows

package encryptedfs

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func lockWriter(f *os.File) error {
	// Lock beyond any supported data offset so independent readers can still
	// read the header and records. Closing the handle releases the writer lock.
	overlapped := windows.Overlapped{Offset: 0xffffffff, OffsetHigh: 0x7fffffff}
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrBusy
	}
	return err
}

func unlockWriter(f *os.File) error {
	overlapped := windows.Overlapped{Offset: 0xffffffff, OffsetHigh: 0x7fffffff}
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &overlapped)
}

// Windows does not expose POSIX directory fsync through os.File. Atomic
// replacement below requests MOVEFILE_WRITE_THROUGH instead.
func syncParent(string) error { return nil }

func replaceFile(from, to string) error {
	_, err := publishFile(from, to)
	return err
}

func publishFile(from, to string) (bool, error) {
	source, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return false, err
	}
	target, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return false, err
	}
	for attempt := 0; attempt < 6; attempt++ {
		err = windows.MoveFileEx(source, target, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
		if err == nil || (!errors.Is(err, windows.ERROR_SHARING_VIOLATION) && !errors.Is(err, windows.ERROR_ACCESS_DENIED)) {
			return err == nil, err
		}
		if attempt < 5 {
			time.Sleep(time.Millisecond << attempt)
		}
	}
	return false, err
}
