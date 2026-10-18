package sshconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
)

type hostDocument struct {
	Version  int               `json:"version"`
	Profiles map[string][]byte `json:"profiles"`
	Internal []byte            `json:"internal,omitempty"`
}

type encryptedProfiles struct {
	key encryptedfs.Key
	doc hostDocument
}

// NewEncryptedStore reads only config/hosts.enc. A missing container is empty
// only before the store is committed. Missing/corrupt committed configurations
// are errors, never an empty profile list or plaintext fallback.
func NewEncryptedStore(dataDir string, keys *encryptedfs.KeyManager) (*Store, error) {
	if keys == nil {
		return nil, encryptedfs.ErrProviderUnavailable
	}
	s := NewStore(dataDir)
	key, err := keys.Derive("config", "hosts")
	if err != nil {
		return nil, err
	}
	e := &encryptedProfiles{key: key, doc: hostDocument{Version: 1, Profiles: make(map[string][]byte)}}
	data, err := encryptedfs.ReadFile(filepath.Join(s.dataDir, "config", "hosts.enc"), key, "config/hosts")
	if errors.Is(err, os.ErrNotExist) {
		if _, commitErr := os.Lstat(filepath.Join(s.dataDir, "store.enc")); commitErr == nil {
			return nil, err
		} else if !errors.Is(commitErr, os.ErrNotExist) {
			return nil, commitErr
		}
		s.enc = e
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	defer clear(data)
	if err := json.Unmarshal(data, &e.doc); err != nil || e.doc.Version != 1 || e.doc.Profiles == nil {
		return nil, encryptedfs.ErrFormat
	}
	seen := map[string]bool{"internal": true}
	for name, raw := range e.doc.Profiles {
		folded := strings.ToLower(name)
		if ValidateName(name) != nil || IsInternalName(name) || seen[folded] {
			return nil, encryptedfs.ErrFormat
		}
		seen[folded] = true
		if _, err := ParseAndValidate(raw); err != nil {
			return nil, encryptedfs.ErrFormat
		}
	}
	if len(e.doc.Internal) != 0 {
		if _, err := ParseInternalOverride(e.doc.Internal, InternalEntry()); err != nil {
			return nil, encryptedfs.ErrFormat
		}
	}
	s.enc = e
	return s, nil
}

// EnsureEncryptedDocument publishes even an empty configuration before the
// migration commit. Afterwards a missing hosts.enc indicates data loss.
func (s *Store) EnsureEncryptedDocument() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enc == nil {
		return encryptedfs.ErrFormat
	}
	_, err := s.saveDocumentLocked(s.cloneDocumentLocked())
	return err
}

func (s *Store) cloneDocumentLocked() hostDocument {
	next := hostDocument{Version: 1, Profiles: make(map[string][]byte, len(s.enc.doc.Profiles)), Internal: s.enc.doc.Internal}
	for name, raw := range s.enc.doc.Profiles {
		next.Profiles[name] = raw // immutable until a cloned document is published
	}
	return next
}

func (s *Store) saveDocumentLocked(next hostDocument) (bool, error) {
	data, err := json.Marshal(next)
	if err != nil {
		return false, err
	}
	defer clear(data)
	dir := filepath.Join(s.dataDir, "config")
	if err := encryptedfs.MkdirAllDurable(dir); err != nil {
		return false, err
	}
	err = encryptedfs.WriteFileAtomic(filepath.Join(dir, "hosts.enc"), s.enc.key, "config/hosts", data)
	published := err == nil || errors.Is(err, encryptedfs.ErrDurability)
	if published {
		s.enc.doc = next
	}
	return published, err
}

// readPersistentLocked deliberately excludes process-local temporary profiles.
func (s *Store) readPersistentLocked(name string) ([]byte, error) {
	if s.enc != nil {
		raw, ok := s.enc.doc.Profiles[name]
		if !ok {
			return nil, ErrNotFound
		}
		return bytes.Clone(raw), nil
	}
	path, err := s.configPath(name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func (s *Store) readEncryptedRaw(name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if IsInternalName(name) {
		if len(s.enc.doc.Internal) != 0 {
			return bytes.Clone(s.enc.doc.Internal), nil
		}
		return InternalTemplate(), nil
	}
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	if raw, ok := s.temporary[name]; ok {
		return bytes.Clone(raw), nil
	}
	return s.readPersistentLocked(name)
}

func (s *Store) listEncryptedLocked() []string {
	names := []string{"internal"}
	seen := map[string]bool{"internal": true}
	for name := range s.temporary {
		names = append(names, name)
		seen[name] = true
	}
	for name := range s.enc.doc.Profiles {
		if !seen[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (s *Store) saveEncrypted(name string, raw []byte, temporary bool) error {
	if IsInternalName(name) {
		if temporary {
			return ErrReserved
		}
		if _, err := ParseInternalOverride(raw, InternalEntry()); err != nil {
			return err
		}
	} else {
		if err := ValidateName(name); err != nil {
			return err
		}
		if _, err := ParseAndValidate(raw); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cloneDocumentLocked()
	var err error
	if IsInternalName(name) {
		next.Internal = bytes.Clone(raw)
	} else {
		if err := s.checkNameCollisionLocked(name, name); err != nil {
			return err
		}
		if temporary {
			delete(next.Profiles, name)
		} else {
			next.Profiles[name] = bytes.Clone(raw)
		}
	}
	published := true
	// A newly created temporary profile must not trigger any persistent write.
	if !temporary || s.enc.doc.Profiles[name] != nil {
		published, err = s.saveDocumentLocked(next)
	}
	if !published {
		return err
	}
	if temporary {
		if s.temporary == nil {
			s.temporary = make(map[string][]byte)
		}
		s.temporary[name] = bytes.Clone(raw)
	} else {
		delete(s.temporary, name)
	}
	s.notifyChange()
	return err
}

func (s *Store) renameEncryptedLocked(oldName, newName string) error {
	if err := s.checkNameCollisionLocked(newName, oldName); err != nil {
		return err
	}
	if raw, ok := s.temporary[oldName]; ok {
		s.temporary[newName] = raw
		delete(s.temporary, oldName)
		s.notifyChange()
		return nil
	}
	raw, ok := s.enc.doc.Profiles[oldName]
	if !ok {
		return ErrNotFound
	}
	next := s.cloneDocumentLocked()
	delete(next.Profiles, oldName)
	next.Profiles[newName] = raw
	published, err := s.saveDocumentLocked(next)
	if published {
		s.notifyChange()
	}
	return err
}

func (s *Store) deleteEncryptedLocked(name string) error {
	if _, ok := s.enc.doc.Profiles[name]; ok {
		next := s.cloneDocumentLocked()
		delete(next.Profiles, name)
		published, err := s.saveDocumentLocked(next)
		if !published {
			return err
		}
		delete(s.temporary, name)
		s.notifyChange()
		return err
	}
	delete(s.temporary, name)
	s.notifyChange()
	return nil
}
