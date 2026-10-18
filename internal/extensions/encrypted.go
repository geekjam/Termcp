package extensions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
)

// NewEncryptedStore reads and writes only authenticated config.enc templates.
// Legacy TOML conversion is owned by securestore before this store is exposed.
func NewEncryptedStore(dataDir string, keys *encryptedfs.KeyManager) (*Store, error) {
	if keys == nil {
		return nil, encryptedfs.ErrProviderUnavailable
	}
	if _, err := keys.Derive("config", "extensions"); err != nil {
		return nil, err
	}
	s := NewStore(dataDir)
	s.keys = keys
	return s, nil
}

func (s *Store) configFilename() string {
	if s.keys != nil {
		return "config.enc"
	}
	return "config.toml"
}

func (s *Store) readConfig(name, path string) ([]byte, error) {
	if s.keys == nil {
		return os.ReadFile(path)
	}
	identity := "extensions/" + name
	key, err := s.keys.Derive("config", identity)
	if err != nil {
		return nil, err
	}
	return encryptedfs.ReadFile(path, key, identity)
}

func (s *Store) writeEncrypted(name, path string, data []byte) error {
	identity := "extensions/" + name
	key, err := s.keys.Derive("config", identity)
	if err != nil {
		return err
	}
	if err := encryptedfs.MkdirAllDurable(filepath.Dir(path)); err != nil {
		return err
	}
	return encryptedfs.WriteFileAtomic(path, key, identity, data)
}

// Refuse case aliases before writing. On Windows an alias would replace the
// existing file using a different authenticated identity and make it unreadable.
func (s *Store) checkEncryptedNameLocked(name string) error {
	names, err := s.List()
	if err != nil {
		return err
	}
	for _, existing := range names {
		if existing != name && strings.EqualFold(existing, name) {
			return fmt.Errorf("%w: name differs only in case", ErrExists)
		}
	}
	return nil
}

// ImportRaw preserves a legacy template's exact bytes, including unknown TOML
// fields, after validating it. Conversion must not silently discard fields.
func (s *Store) ImportRaw(name string, data []byte) error {
	if _, err := ParseTemplate(data, name); err != nil {
		return err
	}
	path, err := s.configPath(name)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys == nil {
		return encryptedfs.ErrFormat
	}
	if err := s.checkEncryptedNameLocked(name); err != nil {
		return err
	}
	if err := s.writeEncrypted(name, path, data); err != nil {
		return err
	}
	s.notifyChange()
	return nil
}
