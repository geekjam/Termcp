package sshconfig

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"

	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
	"golang.org/x/crypto/chacha20poly1305"
)

const (
	transferHeaderSize = 72
	MaxTransferSize    = maxBatchSize + transferHeaderSize + chacha20poly1305.Overhead
)

var (
	transferMagic                 = []byte("TCPCFG\r\n")
	ErrTransferFormat             = errors.New("sshconfig: invalid encrypted export format")
	ErrTransferPasswordRequired   = errors.New("sshconfig: export password required")
	ErrTransferAuthentication     = errors.New("sshconfig: incorrect export password or damaged file")
	ErrTransferUnexpectedPassword = errors.New("sshconfig: plaintext import does not require a password")
)

// IsEncryptedBatch recognizes the portable export envelope, independent of file
// extensions, the local data master key, and the local key provider.
func IsEncryptedBatch(data []byte) bool { return bytes.HasPrefix(data, transferMagic) }

// ExportBatchWithPassword preserves the legacy TOML bytes when password is empty.
// Protected exports use a fresh salt and nonce and never write temporary files.
func (s *Store) ExportBatchWithPassword(only []string, password []byte) ([]byte, error) {
	data, err := s.ExportBatch(only)
	if err != nil {
		return nil, err
	}
	if len(password) == 0 {
		return data, nil
	}
	defer clear(data)
	return encryptBatch(data, password, encryptedfs.DefaultArgon2Parameters())
}

func encryptBatch(data, password []byte, cost encryptedfs.Argon2Parameters) ([]byte, error) {
	if len(data) > maxBatchSize || len(password) > 16<<10 {
		return nil, ErrTransferFormat
	}
	provider, err := encryptedfs.NewPasswordProvider(password, cost)
	if err != nil {
		return nil, err
	}
	defer provider.Destroy()
	params, err := provider.NewParameters()
	if err != nil {
		return nil, err
	}
	key, err := provider.Key(params)
	if err != nil {
		return nil, err
	}
	defer clear(key[:])
	aead, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return nil, err
	}
	header := make([]byte, transferHeaderSize, transferHeaderSize+len(data)+aead.Overhead())
	copy(header, transferMagic)
	binary.LittleEndian.PutUint16(header[8:], 1)
	binary.LittleEndian.PutUint32(header[12:], params.KDF.Time)
	binary.LittleEndian.PutUint32(header[16:], params.KDF.Memory)
	header[20] = params.KDF.Threads
	copy(header[24:40], params.Salt)
	if _, err := rand.Read(header[40:64]); err != nil {
		return nil, err
	}
	binary.LittleEndian.PutUint64(header[64:], uint64(len(data)))
	return aead.Seal(header, header[40:64], data, header[:transferHeaderSize]), nil
}

// DecryptBatch authenticates the entire file before any profile can be imported.
// The returned plaintext is owned by the caller. Header lengths and KDF bounds
// are checked before allocating plaintext or running the password KDF.
func DecryptBatch(data, password []byte) ([]byte, error) {
	if !IsEncryptedBatch(data) {
		if len(data) > maxBatchSize {
			return nil, ErrTransferFormat
		}
		if len(password) != 0 {
			return nil, ErrTransferUnexpectedPassword
		}
		return bytes.Clone(data), nil
	}
	if len(data) < transferHeaderSize+chacha20poly1305.Overhead || len(data) > MaxTransferSize {
		return nil, ErrTransferFormat
	}
	header := data[:transferHeaderSize]
	if binary.LittleEndian.Uint16(header[8:]) != 1 || !bytes.Equal(header[10:12], []byte{0, 0}) || !bytes.Equal(header[21:24], []byte{0, 0, 0}) || binary.LittleEndian.Uint64(header[64:]) != uint64(len(data)-transferHeaderSize-chacha20poly1305.Overhead) {
		return nil, ErrTransferFormat
	}
	params := encryptedfs.KeyParameters{Salt: header[24:40], KDF: encryptedfs.Argon2Parameters{Time: binary.LittleEndian.Uint32(header[12:]), Memory: binary.LittleEndian.Uint32(header[16:]), Threads: header[20]}}
	// v1 uses at most the default cost, even when an attacker edits its header.
	if params.KDF.Time < 1 || params.KDF.Time > 3 || params.KDF.Memory < 8*1024 || params.KDF.Memory > 64*1024 || params.KDF.Threads != 1 {
		return nil, ErrTransferFormat
	}
	if len(password) == 0 {
		return nil, ErrTransferPasswordRequired
	}
	if len(password) > 16<<10 {
		return nil, ErrTransferAuthentication
	}
	provider, err := encryptedfs.NewPasswordProvider(password, encryptedfs.Argon2Parameters{})
	if err != nil {
		return nil, ErrTransferAuthentication
	}
	defer provider.Destroy()
	key, err := provider.Key(params)
	if err != nil {
		return nil, err
	}
	defer clear(key[:])
	aead, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, header[40:64], data[transferHeaderSize:], header)
	if err != nil {
		return nil, ErrTransferAuthentication
	}
	return plain, nil
}

func (s *Store) ImportBatchWithPassword(data, password []byte, temporary bool) (*BatchImportResult, error) {
	plain, err := DecryptBatch(data, password)
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	return s.ImportBatch(plain, temporary)
}
