package encryptedfs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestAutoUnlockEnrollmentPreservesPasswordAndDataKey(t *testing.T) {
	password := testPasswordProvider(t, "placeholder recovery passphrase")
	t.Cleanup(password.Destroy)
	keys, err := NewKeyManager(password)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "master.key")
	if err := keys.Save(path); err != nil {
		t.Fatal(err)
	}
	before, _ := keys.Marshal()
	want, _ := keys.Derive("events", "session-a")
	credentials := &memoryCredentials{values: map[string]string{}}
	provider := credentialProvider{name: "test-system", store: credentials}
	// Neither an unavailable service nor a publication failure may retire the
	// recovery password or acknowledge an unpersisted slot in memory.
	credentials.offline = true
	if err := keys.EnsureAutoUnlockAndSave(path, provider); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatal("unavailable service did not fail")
	}
	credentials.offline = false
	if err := keys.EnsureAutoUnlockAndSave(filepath.Join(t.TempDir(), "missing", "master.key"), provider); err == nil {
		t.Fatal("enrollment acknowledged a failed save")
	}
	after, _ := keys.Marshal()
	persisted, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) || !bytes.Equal(before, persisted) {
		t.Fatal("failed enrollment changed the active or persisted envelope")
	}
	if err := keys.EnsureAutoUnlockAndSave(path, provider); err != nil {
		t.Fatal(err)
	}
	for _, unlock := range []KeyProvider{password, provider} {
		loaded, err := LoadKeyManager(path, unlock)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := loaded.Derive("events", "session-a")
		if got != want {
			t.Fatal("enrollment changed an existing data key")
		}
	}
	before, _ = os.ReadFile(path)
	count := len(credentials.values)
	if err := keys.EnsureAutoUnlockAndSave(path, provider); err != nil {
		t.Fatal(err)
	}
	after, _ = os.ReadFile(path)
	if !bytes.Equal(before, after) || len(credentials.values) != count {
		t.Fatal("a usable slot was unnecessarily replaced")
	}
	// Recovery after a lost native credential is permitted on this unlocked
	// manager. Only the corresponding slot changes, and no duplicate is added.
	credentials.values = map[string]string{}
	if err := keys.EnsureAutoUnlockAndSave(path, provider); err != nil {
		t.Fatal(err)
	}
	if len(keys.envelope.Slots) != 2 || len(credentials.values) != 1 {
		t.Fatal("repair did not replace just the unavailable provider")
	}
	loaded, err := LoadKeyManager(path, password)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := loaded.Derive("events", "session-a")
	if got != want {
		t.Fatal("credential repair lost existing data")
	}
	if err := keys.EnsureAutoUnlockAndSave(path, password); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatal("automatic enrollment bypassed password-change authentication")
	}
}

func TestFirstRecoveryPasswordSaveFailureAndConcurrentSetup(t *testing.T) {
	provider := testEnvProvider(t, "TERMCP_TEST_FIRST_PASSWORD_KEK")
	keys, err := NewKeyManager(provider)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "master.key")
	if err := keys.Save(path); err != nil {
		t.Fatal(err)
	}
	before, _ := keys.Marshal()
	passwords := [][]byte{[]byte("placeholder recovery one"), []byte("placeholder recovery two")}
	cost := Argon2Parameters{Time: 1, Memory: 8 * 1024, Threads: 1}
	if err := keys.AddPasswordAndSave(filepath.Join(t.TempDir(), "missing", "master.key"), passwords[0], passwords[0], cost); err == nil {
		t.Fatal("first password setup acknowledged a failed save")
	}
	after, _ := keys.Marshal()
	persisted, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) || !bytes.Equal(before, persisted) {
		t.Fatal("failed first setup changed active or persisted slots")
	}
	want, _ := keys.Derive("config", "hosts")
	results := make([]error, 2)
	start := make(chan struct{})
	var writers sync.WaitGroup
	for i := range passwords {
		writers.Add(1)
		go func() {
			defer writers.Done()
			<-start
			results[i] = keys.AddPasswordAndSave(path, passwords[i], passwords[i], cost)
		}()
	}
	close(start)
	writers.Wait()
	winners := 0
	for i, result := range results {
		password, err := NewPasswordProvider(passwords[i], cost)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(password.Destroy)
		loaded, err := LoadKeyManager(path, password)
		if result == nil {
			winners++
			if err != nil {
				t.Fatal("acknowledged password was not persisted")
			}
			got, _ := loaded.Derive("config", "hosts")
			if got != want {
				t.Fatal("first password setup replaced the data key")
			}
		} else if !errors.Is(result, ErrAuthentication) || !errors.Is(err, ErrAuthentication) {
			t.Fatal("concurrent first setup replaced the winning password")
		}
	}
	if winners != 1 {
		t.Fatal("concurrent first setup did not select exactly one password")
	}
	if _, err := LoadKeyManager(path, provider); err != nil {
		t.Fatal("first password setup discarded automatic unlock")
	}
}
