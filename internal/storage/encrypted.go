package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
	"github.com/open-mcp-ai/termcp/internal/sessionlog"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

type encryptedHandle struct {
	log      *sessionlog.Log
	writable bool
	dirty    int
}

type encryptedBackend struct {
	mu      sync.Mutex
	store   *Store
	keys    *encryptedfs.KeyManager
	handles map[string]*encryptedHandle
	deleted map[string]bool // shell tombstones; session tombstones live on Store
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
	closed  bool
}

// NewEncrypted uses only meta.enc, events.enc and events.idx.enc. It never
// falls back to legacy files. The caller owns the data-directory process lock
// and must finish migration/recovery before exposing this store to business APIs.
func NewEncrypted(dataDir string, keys *encryptedfs.KeyManager) (*Store, error) {
	if keys == nil {
		return nil, encryptedfs.ErrProviderUnavailable
	}
	s := &Store{dataDir: dataDir, deleted: make(map[string]bool), manifestHash: make(map[string][32]byte)}
	s.enc = &encryptedBackend{store: s, keys: keys, handles: make(map[string]*encryptedHandle), deleted: make(map[string]bool), stop: make(chan struct{}), done: make(chan struct{})}
	go s.enc.run()
	return s, nil
}

func (e *encryptedBackend) run() {
	defer close(e.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-ticker.C:
			e.mu.Lock()
			for key, handle := range e.handles {
				if handle.dirty != 0 {
					if err := e.syncHandle(key, handle); err != nil {
						slog.Error("encrypted history synchronization failed; will retry")
					}
				}
			}
			e.mu.Unlock()
		}
	}
}

func historyIdentity(sessionID, shellID string) string {
	return "sessions/" + sessionID + "/" + shellID + "/events"
}

func (e *encryptedBackend) handle(sessionID, shellID string, write bool) (*encryptedHandle, error) {
	if e.closed {
		return nil, os.ErrClosed
	}
	if err := validateID(sessionID); err != nil {
		return nil, err
	}
	if err := validateID(shellID); err != nil {
		return nil, err
	}
	k := logKey(sessionID, shellID)
	if e.deleted[k] {
		if write {
			return nil, ErrSessionGone
		}
		return nil, nil
	}
	if cached := e.handles[k]; cached != nil {
		if !write || cached.writable {
			return cached, nil
		}
		if err := cached.log.Close(); err != nil {
			return nil, err
		}
		delete(e.handles, k)
	}
	identity := historyIdentity(sessionID, shellID)
	key, err := e.keys.Derive("events", identity)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(e.store.shellDir(sessionID, shellID), "events.enc")
	open := sessionlog.Open
	if write {
		if err := e.store.initLogDir(sessionID, shellID); err != nil {
			return nil, err
		}
		open = sessionlog.OpenAppend
	}
	log, err := open(path, key, identity)
	if errors.Is(err, os.ErrNotExist) && write {
		log, err = sessionlog.Create(path, key, identity)
	}
	if errors.Is(err, os.ErrNotExist) && !write {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	h := &encryptedHandle{log: log, writable: write}
	e.handles[k] = h
	return h, nil
}

func (e *encryptedBackend) withLog(sessionID, shellID string, write bool, use func(*encryptedHandle) error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	h, err := e.handle(sessionID, shellID, write)
	if err != nil || h == nil {
		return err
	}
	return use(h)
}

func (e *encryptedBackend) append(sessionID, shellID string, data []byte, stream StdioStream) (int64, error) {
	var offset int64
	err := e.withLog(sessionID, shellID, true, func(h *encryptedHandle) error {
		var err error
		offset, err = h.log.Size()
		if err != nil {
			return err
		}
		if stream != StdioOut && stream != StdioErr {
			return sessionlog.ErrEvent
		}
		for len(data) > 0 {
			n := min(len(data), sessionlog.MaxOutput)
			if _, err := h.log.RecordOutput(time.Now().UnixMilli(), data[:n], sessionlog.Stream(stream)); err != nil {
				return err
			}
			h.dirty += n
			data = data[n:]
			if h.dirty >= 256<<10 {
				if err := e.syncHandle(logKey(sessionID, shellID), h); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return offset, err
}

func (e *encryptedBackend) syncHandle(key string, h *encryptedHandle) error {
	if !h.writable {
		return nil
	}
	if err := h.log.Sync(); err != nil {
		return err
	}
	parts := strings.Split(key, "\x00")
	identity := historyIdentity(parts[0], parts[1])
	indexKey, err := e.keys.Derive("index", identity)
	if err != nil {
		return err
	}
	path := filepath.Join(e.store.shellDir(parts[0], parts[1]), "events.idx.enc")
	if err := h.log.SaveIndex(path, indexKey); err != nil {
		// The authoritative data Sync above succeeded. Cache failures must not
		// fail an otherwise durable output append; keep it dirty to retry later.
		slog.Warn("encrypted history index could not be saved; history remains available")
		return nil
	}
	h.dirty = 0
	return nil
}

func (e *encryptedBackend) close() error {
	e.once.Do(func() { close(e.stop) })
	<-e.done
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	var result error
	for key, h := range e.handles {
		if h.dirty != 0 {
			result = errors.Join(result, e.syncHandle(key, h))
		}
		result = errors.Join(result, h.log.Close())
		delete(e.handles, key)
	}
	return result
}

func (s *Store) metadataIdentity(dir string) (string, error) {
	rel, err := filepath.Rel(s.sessionsDir(), dir)
	if err != nil || !filepath.IsLocal(rel) || rel == "." {
		return "", ErrInvalidID
	}
	for _, id := range strings.Split(filepath.ToSlash(rel), "/") {
		if err := validateID(id); err != nil {
			return "", err
		}
	}
	return "sessions/" + filepath.ToSlash(rel) + "/meta", nil
}

func (s *Store) writeEncryptedMetadata(dir string, data []byte) error {
	identity, err := s.metadataIdentity(dir)
	if err != nil {
		return err
	}
	key, err := s.enc.keys.Derive("metadata", identity)
	if err != nil {
		return err
	}
	return encryptedfs.WriteFileAtomic(filepath.Join(dir, "meta.enc"), key, identity, data)
}

func (s *Store) readEncryptedMetadata(dir string) ([]byte, error) {
	identity, err := s.metadataIdentity(dir)
	if err != nil {
		return nil, err
	}
	key, err := s.enc.keys.Derive("metadata", identity)
	if err != nil {
		return nil, err
	}
	return encryptedfs.ReadFile(filepath.Join(dir, "meta.enc"), key, identity)
}

func (s *Store) loadEncryptedSessions() ([]api.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	dirs, err := os.ReadDir(s.sessionsDir())
	if errors.Is(err, os.ErrNotExist) {
		return []api.Session{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]api.Session, 0, len(dirs))
	for _, d := range dirs {
		if !d.IsDir() || validateID(d.Name()) != nil {
			continue
		}
		sess, err := s.readManifest(s.sessionDir(d.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue // an uncommitted/orphan log is not a published session
		}
		if err != nil {
			return nil, err
		}
		if sess.ID != d.Name() {
			return nil, encryptedfs.ErrFormat
		}
		sess.Shells = nil
		children, err := os.ReadDir(s.sessionDir(d.Name()))
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			if !child.IsDir() || validateID(child.Name()) != nil {
				continue
			}
			sh, err := s.readManifest(s.shellDir(d.Name(), child.Name()))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if sh.ID != child.Name() {
				return nil, encryptedfs.ErrFormat
			}
			sh.Shells = nil
			sess.Shells = append(sess.Shells, *sh)
		}
		sortShellsByCreation(sess.Shells)
		out = append(out, *sess)
	}
	return out, nil
}

func (s *Store) deleteEncrypted(sessionID, shellID string) error {
	e := s.enc
	e.mu.Lock()
	defer e.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.closed {
		return os.ErrClosed
	}
	dir := s.sessionDir(sessionID)
	if shellID == "" {
		s.deleted[sessionID] = true
	} else {
		dir = s.shellDir(sessionID, shellID)
		e.deleted[logKey(sessionID, shellID)] = true
	}
	var closeErr error
	for key, h := range e.handles {
		if key == logKey(sessionID, shellID) || shellID == "" && strings.HasPrefix(key, sessionID+"\x00") {
			closeErr = errors.Join(closeErr, h.log.Close())
			delete(e.handles, key)
		}
	}
	if closeErr != nil {
		return closeErr
	}
	for path := range s.manifestHash {
		if path == filepath.Join(dir, "meta.enc") || strings.HasPrefix(path, dir+string(filepath.Separator)) {
			delete(s.manifestHash, path)
		}
	}
	return os.RemoveAll(dir)
}

// ImportMetadata preserves the legacy JSON bytes, including unknown fields.
// Only the migration writer should use it, before publishing the new store.
func (s *Store) ImportMetadata(sessionID, shellID string, data []byte) error {
	if s.enc == nil || validateID(sessionID) != nil || shellID != "" && validateID(shellID) != nil {
		return ErrInvalidID
	}
	var metadata api.Session
	if err := json.Unmarshal(data, &metadata); err != nil {
		return err
	}
	expected, dir := sessionID, s.sessionDir(sessionID)
	if shellID != "" {
		expected, dir = shellID, s.shellDir(sessionID, shellID)
	}
	if metadata.ID != expected {
		return fmt.Errorf("%w: metadata identity mismatch", encryptedfs.ErrFormat)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := encryptedfs.MkdirAllDurable(dir); err != nil {
		return err
	}
	return s.writeEncryptedMetadata(dir, data)
}

func (s *Store) ReadEncryptedMetadata(sessionID, shellID string) ([]byte, error) {
	if s.enc == nil || validateID(sessionID) != nil || shellID != "" && validateID(shellID) != nil {
		return nil, ErrInvalidID
	}
	dir := s.sessionDir(sessionID)
	if shellID != "" {
		dir = s.shellDir(sessionID, shellID)
	}
	return s.readEncryptedMetadata(dir)
}

var _ io.ReaderAt = (*sessionlog.Log)(nil)

func (s *Store) RecordResize(sessionID, shellID string, rows, cols int) error {
	if s.enc == nil {
		return nil
	}
	if rows < 1 || rows > 65535 || cols < 1 || cols > 65535 {
		return sessionlog.ErrEvent
	}
	return s.enc.withLog(sessionID, shellID, true, func(h *encryptedHandle) error {
		if err := h.log.RecordResize(time.Now().UnixMilli(), uint16(cols), uint16(rows)); err != nil {
			return err
		}
		h.dirty++
		return nil
	})
}
