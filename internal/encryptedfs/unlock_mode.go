package encryptedfs

import (
	"crypto/subtle"
	"errors"
)

// Only key envelopes use version 2. Encrypted data and HKDF identities retain
// their version-1 format, so changing startup policy never rewrites history.
const keyPolicyVersion = 2

type UnlockMode string

const (
	UnlockAutomatic UnlockMode = "automatic"
	UnlockPassword  UnlockMode = "password"
)

var ErrUnlockMode = errors.New("encryptedfs: invalid unlock mode")

func (mode UnlockMode) valid() bool { return mode == UnlockAutomatic || mode == UnlockPassword }

func (m *KeyManager) UnlockMode() UnlockMode {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.envelope.UnlockMode
}

// ReadUnlockMode inspects public startup policy without accessing any KEK.
// A legacy envelope returns an empty mode; callers retain legacy deployment
// defaults until an authenticated user explicitly saves a choice.
func ReadUnlockMode(path string) (UnlockMode, error) {
	data, err := readEnvelopeFile(path)
	if err != nil {
		return "", err
	}
	envelope, err := parseEnvelope(data)
	return envelope.UnlockMode, err
}

type UnlockModeOptions struct {
	Mode              UnlockMode
	CurrentPassword   []byte
	NewPassword       []byte
	Confirmation      []byte
	PasswordCost      Argon2Parameters
	AutomaticProvider KeyProvider
}

// SetUnlockModeAndSave authenticates an existing password or sets the first
// password when entering manual mode. Policy and all required slots are
// authenticated and published in one envelope. Manual mode retains no automatic
// slot; the random data key and Store ID remain unchanged.
func (m *KeyManager) SetUnlockModeAndSave(path string, options UnlockModeOptions) error {
	if !options.Mode.valid() {
		return ErrUnlockMode
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	hasPassword := false
	for _, slot := range m.envelope.Slots {
		hasPassword = hasPassword || slot.Provider == "password"
	}
	var password *PasswordProvider
	if hasPassword {
		if len(options.NewPassword) != 0 || len(options.Confirmation) != 0 {
			return ErrFormat
		}
		var err error
		password, err = NewPasswordProvider(options.CurrentPassword, options.PasswordCost)
		if err != nil {
			return ErrAuthentication
		}
		defer password.Destroy()
		verified, err := unlockEnvelope(m.envelope, password)
		if err != nil {
			return ErrAuthentication
		}
		matched := subtle.ConstantTimeCompare(m.master[:], verified.master[:]) == 1
		clear(verified.master[:])
		if !matched {
			return ErrAuthentication
		}
	} else {
		if len(options.CurrentPassword) != 0 {
			return ErrFormat
		}
		if options.Mode == UnlockPassword {
			if subtle.ConstantTimeCompare(options.NewPassword, options.Confirmation) != 1 {
				return ErrConfirmation
			}
			var err error
			password, err = NewPasswordProvider(options.NewPassword, options.PasswordCost)
			if err != nil {
				return err
			}
			defer password.Destroy()
		} else if len(options.NewPassword) != 0 || len(options.Confirmation) != 0 {
			return ErrFormat
		}
	}
	var providers []KeyProvider
	if password != nil {
		providers = append(providers, password)
	}
	if options.Mode == UnlockAutomatic {
		if options.AutomaticProvider == nil || options.AutomaticProvider.Name() == "password" {
			return ErrProviderUnavailable
		}
		providers = append(providers, options.AutomaticProvider)
	}
	candidate := &KeyManager{master: m.master, envelope: keyEnvelope{
		Version: keyPolicyVersion, StoreID: m.envelope.StoreID, UnlockMode: options.Mode,
	}}
	defer clear(candidate.master[:])
	if err := candidate.Rewrap(providers...); err != nil {
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
