// Package encryptedfs provides versioned authenticated files. It is independent
// of session recording and never decides when existing data should be migrated.
package encryptedfs

import (
	"bytes"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

var (
	ErrFormat              = errors.New("encryptedfs: invalid format")
	ErrVersion             = errors.New("encryptedfs: unsupported version")
	ErrAuthentication      = errors.New("encryptedfs: authentication failed")
	ErrProviderUnavailable = errors.New("encryptedfs: key provider unavailable")
	ErrConfirmation        = errors.New("encryptedfs: new password confirmation does not match")
)

const (
	formatVersion = 1
	maxSlots      = 8
	maxEnvelope   = 64 << 10
	maxContext    = 4096
	EnvStorageKEK = "TERMCP_STORAGE_KEK"
)

// Key is a 256-bit encryption key. Callers must never log or serialize its value.
type Key [chacha20poly1305.KeySize]byte

func GenerateKey() (Key, error) {
	var key Key
	_, err := rand.Read(key[:])
	return key, err
}

// Argon2Parameters are persisted with a password slot. Memory is in KiB.
// Bounds are checked before running the KDF, including when reading disk data.
type Argon2Parameters struct {
	Time    uint32 `json:"time"`
	Memory  uint32 `json:"memory_kib"`
	Threads uint8  `json:"threads"`
}

func DefaultArgon2Parameters() Argon2Parameters {
	return Argon2Parameters{Time: 3, Memory: 64 * 1024, Threads: 1}
}

func (p Argon2Parameters) validate() error {
	if p.Time < 1 || p.Time > 10 || p.Memory < 8*1024 || p.Memory > 256*1024 || p.Threads < 1 || p.Threads > 8 {
		return fmt.Errorf("%w: Argon2 parameters outside supported limits", ErrFormat)
	}
	return nil
}

type KeyParameters struct {
	Salt      []byte           `json:"salt,omitempty"`
	KDF       Argon2Parameters `json:"argon2id"`
	Protected []byte           `json:"protected,omitempty"`
}

// KeyProvider supplies a KEK; it does not supply or replace the master key.
// Additional system providers can implement this without changing file formats.
type KeyProvider interface {
	Name() string
	NewParameters() (KeyParameters, error)
	Key(KeyParameters) (Key, error)
}

type EnvProvider struct{ Variable string }

func (EnvProvider) Name() string                          { return "env" }
func (EnvProvider) NewParameters() (KeyParameters, error) { return KeyParameters{}, nil }
func (p EnvProvider) Key(params KeyParameters) (Key, error) {
	var key Key
	if len(params.Salt) != 0 || params.KDF != (Argon2Parameters{}) || len(params.Protected) != 0 {
		return key, ErrFormat
	}
	variable := p.Variable
	if variable == "" {
		variable = EnvStorageKEK
	}
	value, ok := os.LookupEnv(variable)
	if !ok || value == "" {
		return key, ErrProviderUnavailable
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(value)
	defer clear(decoded)
	if err != nil || len(decoded) != len(key) {
		return key, fmt.Errorf("%w: expected a base64-encoded 32-byte KEK", ErrProviderUnavailable)
	}
	copy(key[:], decoded)
	return key, nil
}

type PasswordProvider struct {
	mu        sync.RWMutex
	password  []byte
	cost      Argon2Parameters
	destroyed bool
}

// NewPasswordProvider copies the password. A zero cost selects the default.
// Passing a provider to Unlock uses the parameters stored in its slot.
func NewPasswordProvider(password []byte, cost Argon2Parameters) (*PasswordProvider, error) {
	if len(password) == 0 || len(password) > 1<<20 {
		return nil, fmt.Errorf("%w: password length outside supported limits", ErrProviderUnavailable)
	}
	if cost == (Argon2Parameters{}) {
		cost = DefaultArgon2Parameters()
	}
	if err := cost.validate(); err != nil {
		return nil, err
	}
	return &PasswordProvider{password: bytes.Clone(password), cost: cost}, nil
}

func (*PasswordProvider) Name() string { return "password" }
func (p *PasswordProvider) NewParameters() (KeyParameters, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.destroyed {
		return KeyParameters{}, ErrProviderUnavailable
	}
	params := KeyParameters{Salt: make([]byte, 16), KDF: p.cost}
	_, err := rand.Read(params.Salt)
	return params, err
}
func (p *PasswordProvider) Key(params KeyParameters) (Key, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var key Key
	if p.destroyed {
		return key, ErrProviderUnavailable
	}
	if len(params.Salt) != 16 || len(params.Protected) != 0 {
		return key, ErrFormat
	}
	if err := params.KDF.validate(); err != nil {
		return key, err
	}
	derived := argon2.IDKey(p.password, params.Salt, params.KDF.Time, params.KDF.Memory, params.KDF.Threads, uint32(len(key)))
	copy(key[:], derived)
	clear(derived)
	return key, nil
}

// Destroy clears the provider's retained password copy. It does not lock the
// global store or invalidate an already-unlocked KeyManager. Go does not promise
// complete process-memory erasure; callers must also release their input buffers.
func (p *PasswordProvider) Destroy() {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.password)
	p.password = nil
	p.destroyed = true
}

type keySlot struct {
	ID         []byte        `json:"id"`
	Provider   string        `json:"provider"`
	Parameters KeyParameters `json:"parameters"`
	Nonce      []byte        `json:"nonce"`
	Wrapped    []byte        `json:"wrapped"`
}

type keyEnvelope struct {
	Version    uint16     `json:"version"`
	StoreID    []byte     `json:"store_id"`
	Slots      []keySlot  `json:"slots"`
	UnlockMode UnlockMode `json:"unlock_mode,omitempty"`
}

func (e keyEnvelope) header() keyEnvelope {
	return keyEnvelope{Version: e.Version, StoreID: e.StoreID, UnlockMode: e.UnlockMode}
}

func slotAAD(envelope keyEnvelope, slot keySlot) []byte {
	// Struct field order gives a canonical encoding; no maps are involved.
	data, _ := json.Marshal(struct {
		Version    uint16
		StoreID    []byte
		SlotID     []byte
		Provider   string
		Parameters KeyParameters
		UnlockMode UnlockMode `json:",omitempty"`
	}{envelope.Version, envelope.StoreID, slot.ID, slot.Provider, slot.Parameters, envelope.UnlockMode})
	return data
}

// KeyManager retains an unlocked master key. Marshal exports only wrapped key
// slots; Rewrap changes KEKs without changing the master key or derived keys.
// Persistence and the decision to initialize a new store belong to the caller.
type KeyManager struct {
	mu       sync.RWMutex
	master   Key
	envelope keyEnvelope
}

func NewKeyManager(providers ...KeyProvider) (*KeyManager, error) {
	master, err := GenerateKey()
	if err != nil {
		return nil, err
	}
	m := &KeyManager{master: master, envelope: keyEnvelope{Version: formatVersion, StoreID: make([]byte, 16)}}
	if _, err := rand.Read(m.envelope.StoreID); err != nil {
		return nil, err
	}
	if err := m.Rewrap(providers...); err != nil {
		return nil, err
	}
	return m, nil
}

// NewPasswordKeyManager is the first-setup operation used by a password form.
// User input protects a random master key; it never becomes the data key itself.
func NewPasswordKeyManager(password, confirmation []byte, cost Argon2Parameters) (*KeyManager, error) {
	if subtle.ConstantTimeCompare(password, confirmation) != 1 {
		return nil, ErrConfirmation
	}
	provider, err := NewPasswordProvider(password, cost)
	if err != nil {
		return nil, err
	}
	defer provider.Destroy()
	return NewKeyManager(provider)
}

func UnlockKeyManager(data []byte, provider KeyProvider) (*KeyManager, error) {
	if provider == nil {
		return nil, ErrFormat
	}
	envelope, err := parseEnvelope(data)
	if err != nil {
		return nil, err
	}
	return unlockEnvelope(envelope, provider)
}

func parseEnvelope(data []byte) (keyEnvelope, error) {
	var envelope keyEnvelope
	if len(data) > maxEnvelope {
		return envelope, ErrFormat
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return keyEnvelope{}, ErrFormat
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return keyEnvelope{}, ErrFormat
	}
	if envelope.Version != formatVersion && envelope.Version != keyPolicyVersion {
		return keyEnvelope{}, ErrVersion
	}
	if envelope.Version == formatVersion && envelope.UnlockMode != "" || envelope.Version == keyPolicyVersion && !envelope.UnlockMode.valid() {
		return keyEnvelope{}, ErrFormat
	}
	if len(envelope.StoreID) != 16 || len(envelope.Slots) == 0 || len(envelope.Slots) > maxSlots {
		return keyEnvelope{}, ErrFormat
	}
	seen := make(map[string]bool)
	for _, slot := range envelope.Slots {
		if envelope.UnlockMode == UnlockPassword && slot.Provider != "password" {
			return keyEnvelope{}, ErrFormat
		}
		if len(slot.ID) != 16 || seen[string(slot.ID)] || slot.Provider == "" || len(slot.Provider) > 64 || len(slot.Nonce) != chacha20poly1305.NonceSizeX || len(slot.Wrapped) != len(Key{})+chacha20poly1305.Overhead {
			return keyEnvelope{}, ErrFormat
		}
		seen[string(slot.ID)] = true
	}
	return envelope, nil
}

func unlockEnvelope(envelope keyEnvelope, provider KeyProvider) (*KeyManager, error) {
	matched := false
	for _, slot := range envelope.Slots {
		if slot.Provider != provider.Name() {
			continue
		}
		matched = true
		kek, err := provider.Key(slot.Parameters)
		if err != nil {
			return nil, err
		}
		aead, err := chacha20poly1305.NewX(kek[:])
		clear(kek[:])
		if err != nil {
			return nil, err
		}
		master, err := aead.Open(nil, slot.Nonce, slot.Wrapped, slotAAD(envelope, slot))
		if err != nil {
			continue
		}
		m := &KeyManager{envelope: envelope}
		copy(m.master[:], master)
		clear(master)
		return m, nil
	}
	if !matched {
		return nil, ErrProviderUnavailable
	}
	return nil, ErrAuthentication
}

func (m *KeyManager) Marshal() ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return json.Marshal(m.envelope)
}

func (m *KeyManager) HasProvider(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, slot := range m.envelope.Slots {
		if slot.Provider == name {
			return true
		}
	}
	return false
}

func (m *KeyManager) Rewrap(providers ...KeyProvider) error {
	if len(providers) == 0 || len(providers) > maxSlots {
		return ErrFormat
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	envelope := m.envelope.header()
	for _, provider := range providers {
		if envelope.UnlockMode == UnlockPassword && provider != nil && provider.Name() != "password" {
			return ErrUnlockMode
		}
		slot, err := wrapKeySlot(envelope, m.master, provider)
		if err != nil {
			return err
		}
		envelope.Slots = append(envelope.Slots, slot)
	}
	m.envelope = envelope
	return nil
}

func wrapKeySlot(envelope keyEnvelope, master Key, provider KeyProvider) (keySlot, error) {
	if provider == nil || provider.Name() == "" || len(provider.Name()) > 64 || !utf8.ValidString(provider.Name()) {
		return keySlot{}, ErrProviderUnavailable
	}
	params, err := provider.NewParameters()
	if err != nil {
		return keySlot{}, err
	}
	kek, err := provider.Key(params)
	if err != nil {
		return keySlot{}, err
	}
	aead, err := chacha20poly1305.NewX(kek[:])
	clear(kek[:])
	if err != nil {
		return keySlot{}, err
	}
	slot := keySlot{ID: make([]byte, 16), Provider: provider.Name(), Parameters: params, Nonce: make([]byte, aead.NonceSize())}
	if _, err := rand.Read(slot.ID); err != nil {
		return keySlot{}, err
	}
	if _, err := rand.Read(slot.Nonce); err != nil {
		return keySlot{}, err
	}
	slot.Wrapped = aead.Seal(nil, slot.Nonce, master[:], slotAAD(envelope, slot))
	return slot, nil
}

// ChangePassword reauthenticates even an already-unlocked manager. It retains
// non-password slots and the data master key. Save must succeed before an API
// acknowledges the change or retires the previous password; this method performs
// no filesystem writes and cannot initialize a previously absent password slot.
func (m *KeyManager) ChangePassword(current, password, confirmation []byte, cost Argon2Parameters) error {
	if subtle.ConstantTimeCompare(password, confirmation) != 1 {
		return ErrConfirmation
	}
	currentProvider, err := NewPasswordProvider(current, Argon2Parameters{})
	if err != nil {
		return ErrAuthentication
	}
	defer currentProvider.Destroy()
	nextProvider, err := NewPasswordProvider(password, cost)
	if err != nil {
		return err
	}
	defer nextProvider.Destroy()
	m.mu.Lock()
	defer m.mu.Unlock()
	blob, err := json.Marshal(m.envelope)
	if err != nil {
		return err
	}
	verified, err := UnlockKeyManager(blob, currentProvider)
	if err != nil {
		return ErrAuthentication
	}
	matched := subtle.ConstantTimeCompare(verified.master[:], m.master[:]) == 1
	clear(verified.master[:])
	if !matched {
		return ErrAuthentication
	}
	slot, err := wrapKeySlot(m.envelope, m.master, nextProvider)
	if err != nil {
		return err
	}
	envelope := m.envelope.header()
	for _, existing := range m.envelope.Slots {
		if existing.Provider != "password" {
			envelope.Slots = append(envelope.Slots, existing)
		}
	}
	envelope.Slots = append(envelope.Slots, slot)
	m.envelope = envelope
	return nil
}

func (m *KeyManager) Derive(purpose, identity string) (Key, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return deriveKey(m.master, m.envelope.StoreID, purpose, identity)
}

func deriveKey(master Key, salt []byte, purpose, identity string) (Key, error) {
	var key Key
	if purpose == "" || identity == "" || len(purpose) > maxContext || len(identity) > maxContext {
		return key, ErrFormat
	}
	var context bytes.Buffer
	context.WriteString("termcp/encryptedfs/v1")
	for _, value := range []string{purpose, identity} {
		_ = binary.Write(&context, binary.LittleEndian, uint32(len(value)))
		context.WriteString(value)
	}
	derived, err := hkdf.Key(sha256.New, master[:], salt, context.String(), len(key))
	if err != nil {
		return key, err
	}
	copy(key[:], derived)
	clear(derived)
	return key, nil
}
