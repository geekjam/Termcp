package encryptedfs

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUnlockModeTransitionAuthenticatesAndPreservesData(t *testing.T) {
	for _, initialPassword := range []bool{false, true} {
		name := "first password"
		if initialPassword {
			name = "existing password"
		}
		t.Run(name, func(t *testing.T) {
			env := testEnvProvider(t, "TERMCP_TEST_UNLOCK_MODE_KEK")
			password := testPasswordProvider(t, "placeholder mode passphrase")
			t.Cleanup(password.Destroy)
			providers := []KeyProvider{env}
			if initialPassword {
				providers = append(providers, password)
			}
			keys, err := NewKeyManager(providers...)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "master.key")
			if err := keys.Save(path); err != nil {
				t.Fatal(err)
			}
			before, _ := keys.Marshal()
			want, _ := keys.Derive("events", "session-a")
			options := UnlockModeOptions{Mode: UnlockPassword, PasswordCost: password.cost}
			if initialPassword {
				options.CurrentPassword = []byte("incorrect placeholder")
			} else {
				options.NewPassword = []byte("placeholder mode passphrase")
				options.Confirmation = []byte("mismatch")
			}
			if err := keys.SetUnlockModeAndSave(path, options); err == nil {
				t.Fatal("manual mode bypassed password authentication or confirmation")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("failed mode change replaced the envelope")
			}
			if initialPassword {
				options.CurrentPassword = []byte("placeholder mode passphrase")
			} else {
				options.Confirmation = options.NewPassword
			}
			if err := keys.SetUnlockModeAndSave(path, options); err != nil {
				t.Fatal(err)
			}
			if keys.UnlockMode() != UnlockPassword || len(keys.envelope.Slots) != 1 || keys.HasProvider("env") {
				t.Fatal("manual mode retained an automatic-unlock slot")
			}
			if _, err := LoadKeyManager(path, env); !errors.Is(err, ErrProviderUnavailable) {
				t.Fatal("the environment still unlocks manual mode")
			}
			if err := keys.EnsureAutoUnlockAndSave(path, env); !errors.Is(err, ErrUnlockMode) {
				t.Fatal("automatic enrollment ignored the saved manual policy")
			}
			manual, err := LoadKeyManager(path, password)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := manual.Derive("events", "session-a")
			if got != want {
				t.Fatal("mode change replaced the master key or Store ID")
			}
			before, _ = keys.Marshal()
			var tampered keyEnvelope
			if err := json.Unmarshal(before, &tampered); err != nil {
				t.Fatal(err)
			}
			tampered.UnlockMode = UnlockAutomatic
			blob, _ := json.Marshal(tampered)
			if _, err := UnlockKeyManager(blob, password); !errors.Is(err, ErrAuthentication) {
				t.Fatal("unlock policy was not authenticated")
			}
			tampered.Version, tampered.UnlockMode = formatVersion, ""
			blob, _ = json.Marshal(tampered)
			if _, err := UnlockKeyManager(blob, password); !errors.Is(err, ErrAuthentication) {
				t.Fatal("downgrading policy to a legacy envelope bypassed authentication")
			}
			next := []byte("placeholder rotated mode password")
			if err := keys.ChangePasswordAndSave(path, []byte("placeholder mode passphrase"), next, next, password.cost); err != nil {
				t.Fatal(err)
			}
			if keys.UnlockMode() != UnlockPassword {
				t.Fatal("password rotation discarded manual mode")
			}
			before, _ = keys.Marshal()
			automatic := UnlockModeOptions{Mode: UnlockAutomatic, CurrentPassword: next, PasswordCost: password.cost, AutomaticProvider: env}
			if err := keys.SetUnlockModeAndSave(filepath.Join(t.TempDir(), "missing", "master.key"), automatic); err == nil {
				t.Fatal("mode change acknowledged an unpublished envelope")
			}
			after, _ = keys.Marshal()
			persisted, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) || !bytes.Equal(before, persisted) {
				t.Fatal("failed publication changed active or persisted policy")
			}
			if err := keys.SetUnlockModeAndSave(path, automatic); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadKeyManager(path, env)
			if err != nil || !loaded.HasProvider("password") || loaded.UnlockMode() != UnlockAutomatic {
				t.Fatal("automatic mode lost password recovery or failed to unlock")
			}
			got, _ = loaded.Derive("events", "session-a")
			if got != want {
				t.Fatal("automatic mode changed existing data keys")
			}
		})
	}
}
