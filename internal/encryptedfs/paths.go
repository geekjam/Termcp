package encryptedfs

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// DirectoryLock is an OS-owned process lock. The empty file is not a PID file;
// its existence or age says nothing about ownership. Never unlink a live lock.
type DirectoryLock struct {
	file *os.File
	once sync.Once
	err  error
}

func AcquireDirectoryLock(root string) (*DirectoryLock, error) {
	if err := MkdirAllDurable(root); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(root, ".store.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := lockWriter(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &DirectoryLock{file: f}, nil
}

func (l *DirectoryLock) Close() error {
	l.once.Do(func() { l.err = errors.Join(unlockWriter(l.file), l.file.Close()) })
	return l.err
}

// MkdirAllDurable synchronizes each newly created directory entry, including
// ancestors. Syncing only the final file's parent does not persist ancestors.
func MkdirAllDurable(path string) error {
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return &os.PathError{Op: "mkdir", Path: path, Err: os.ErrExist}
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return err
	}
	if err := MkdirAllDurable(parent); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			return os.ErrExist
		}
	}
	return syncParent(path)
}

// PublishCiphertext publishes a previously synchronized ciphertext temp file.
// It is for the migration publisher; it performs no encryption of its own.
func PublishCiphertext(from, to string) error {
	published, err := publishFile(from, to)
	if published && err != nil {
		return errors.Join(ErrDurability, err)
	}
	return err
}

func SyncParent(path string) error { return syncParent(path) }
