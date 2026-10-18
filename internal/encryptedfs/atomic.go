package encryptedfs

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const maxSmallFile = 64 << 20

// ErrDurability means replacement succeeded but its directory synchronization
// failed. Memory follows the published file; callers must not acknowledge
// durable success. For a password change, retain both passwords until a retry
// or restart establishes which envelope survived.
var ErrDurability = errors.New("encryptedfs: file replacement published but durability was not confirmed")

// Save persists the envelope, whose master key is already AEAD-wrapped by KEKs.
// Bootstrap must establish exclusive store ownership before its first Save;
// callers must persist a rewrap before retiring any previously usable KEK.
func (m *KeyManager) Save(path string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, err := saveEnvelope(path, m.envelope)
	return err
}

func saveEnvelope(path string, envelope keyEnvelope) (bool, error) {
	data, err := json.Marshal(envelope)
	if err != nil {
		return false, err
	}
	if len(data) > maxEnvelope {
		return false, ErrTooLarge
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-key-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		return false, errors.Join(err, tmp.Close())
	}
	if err := errors.Join(tmp.Sync(), tmp.Close()); err != nil {
		return false, err
	}
	return publishFile(tmp.Name(), path)
}

// ChangePasswordAndSave is the persistent operation for an authenticated API.
// It serializes reauthentication, replacement and publication. A failure before
// rename leaves both the active and persisted slots unchanged.
func (m *KeyManager) ChangePasswordAndSave(path string, current, password, confirmation []byte, cost Argon2Parameters) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	candidate := &KeyManager{master: m.master, envelope: m.envelope}
	defer clear(candidate.master[:])
	if err := candidate.ChangePassword(current, password, confirmation, cost); err != nil {
		return err
	}
	published, err := saveEnvelope(path, candidate.envelope)
	if published {
		m.envelope = candidate.envelope
		if err != nil {
			return errors.Join(ErrDurability, err)
		}
	}
	return err
}

// AddPasswordAndSave sets the first optional recovery password after a system
// or ENV provider has unlocked the store. It cannot replace an existing password
// and never changes the data key or any automatic-unlock slot.
func (m *KeyManager) AddPasswordAndSave(path string, password, confirmation []byte, cost Argon2Parameters) error {
	if subtle.ConstantTimeCompare(password, confirmation) != 1 {
		return ErrConfirmation
	}
	provider, err := NewPasswordProvider(password, cost)
	if err != nil {
		return err
	}
	defer provider.Destroy()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, slot := range m.envelope.Slots {
		if slot.Provider == "password" {
			return ErrAuthentication
		}
	}
	if len(m.envelope.Slots) >= maxSlots {
		return ErrFormat
	}
	slot, err := wrapKeySlot(m.envelope, m.master, provider)
	if err != nil {
		return err
	}
	envelope := m.envelope.header()
	envelope.Slots = append(append([]keySlot(nil), m.envelope.Slots...), slot)
	published, err := saveEnvelope(path, envelope)
	if published {
		m.envelope = envelope
		if err != nil {
			return errors.Join(ErrDurability, err)
		}
	}
	return err
}

// EnsureAutoUnlockAndSave retains all other slots, including password recovery.
// An existing usable slot is left unchanged. A missing or unavailable slot is
// replaced only on an already-unlocked manager; startup must never call this to
// repair credentials before authenticating. Password changes have their own API.
func (m *KeyManager) EnsureAutoUnlockAndSave(path string, provider KeyProvider) error {
	if provider == nil || provider.Name() == "password" {
		return ErrProviderUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.envelope.UnlockMode == UnlockPassword {
		return ErrUnlockMode
	}
	if verified, err := unlockEnvelope(m.envelope, provider); err == nil {
		matched := subtle.ConstantTimeCompare(verified.master[:], m.master[:]) == 1
		clear(verified.master[:])
		if matched {
			return nil
		}
	}
	envelope := m.envelope.header()
	for _, slot := range m.envelope.Slots {
		if slot.Provider != provider.Name() {
			envelope.Slots = append(envelope.Slots, slot)
		}
	}
	if len(envelope.Slots) >= maxSlots {
		return ErrFormat
	}
	slot, err := wrapKeySlot(envelope, m.master, provider)
	if err != nil {
		return err
	}
	envelope.Slots = append(envelope.Slots, slot)
	published, err := saveEnvelope(path, envelope)
	if published {
		m.envelope = envelope
		if err != nil {
			return errors.Join(ErrDurability, err)
		}
	}
	return err
}

// LoadKeyManager only unlocks existing data; it never initializes on failure.
func LoadKeyManager(path string, provider KeyProvider) (*KeyManager, error) {
	data, err := readEnvelopeFile(path)
	if err != nil {
		return nil, err
	}
	return UnlockKeyManager(data, provider)
}

// KeyProviders reads only public slot names. It does not unlock a master key,
// call a system credential service or run a password KDF.
func KeyProviders(path string) ([]string, error) {
	data, err := readEnvelopeFile(path)
	if err != nil {
		return nil, err
	}
	envelope, err := parseEnvelope(data)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(envelope.Slots))
	for _, slot := range envelope.Slots {
		names = append(names, slot.Provider)
	}
	return names, nil
}

func readEnvelopeFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxEnvelope+1))
	if err != nil {
		return nil, err
	}
	return data, nil
}

// WriteFileAtomic writes ciphertext from the first byte of its sibling temp
// file, synchronizes it, then publishes it. The old file remains on any failure
// before replacement; a parent-directory sync failure after rename is returned.
func WriteFileAtomic(path string, key Key, identity string, data []byte) error {
	if len(data) > maxSmallFile {
		return ErrTooLarge
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-enc-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	f, err := createHandle(tmp, key, identity)
	if err != nil {
		_ = tmp.Close()
		return err
	}
	for len(data) > 0 {
		n := min(len(data), MaxRecordSize)
		if _, err := f.Append(data[:n]); err != nil {
			return errors.Join(err, f.Close())
		}
		data = data[n:]
	}
	if err := f.Close(); err != nil {
		return err
	}
	published, err := publishFile(tmp.Name(), path)
	if published && err != nil {
		return errors.Join(ErrDurability, err)
	}
	return err
}

func ReadFile(path string, key Key, identity string) ([]byte, error) {
	f, err := Open(path, key, identity)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	size, err := f.Size()
	if err != nil {
		return nil, err
	}
	if size > maxSmallFile {
		return nil, ErrTooLarge
	}
	data := make([]byte, int(size))
	if _, err := io.ReadFull(f, data); err != nil {
		clear(data)
		return nil, err
	}
	return data, nil
}
