package encryptedfs

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func testEnvProvider(t *testing.T, variable string) EnvProvider {
	t.Helper()
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(variable, base64.StdEncoding.EncodeToString(key[:]))
	return EnvProvider{Variable: variable}
}

func testPasswordProvider(t *testing.T, password string) *PasswordProvider {
	t.Helper()
	provider, err := NewPasswordProvider([]byte(password), Argon2Parameters{Time: 1, Memory: 8 * 1024, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func TestKeyManagerSlotsPersistenceAndRewrap(t *testing.T) {
	env := testEnvProvider(t, "TERMCP_TEST_STORAGE_KEK")
	password := testPasswordProvider(t, "placeholder passphrase")
	m, err := NewKeyManager(env, password)
	if err != nil {
		t.Fatal(err)
	}
	want, err := m.Derive("events", "session-a/shell-a")
	if err != nil {
		t.Fatal(err)
	}
	otherPurpose, _ := m.Derive("index", "session-a/shell-a")
	otherShell, _ := m.Derive("events", "session-a/shell-b")
	if want == otherPurpose || want == otherShell || otherPurpose == otherShell {
		t.Fatal("key derivation did not separate purposes and identities")
	}
	blob, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte(base64.StdEncoding.EncodeToString(m.master[:]))) || bytes.Contains(blob, []byte("placeholder passphrase")) {
		t.Fatal("key envelope exposed plaintext key material")
	}
	for _, provider := range []KeyProvider{env, password} {
		unlocked, err := UnlockKeyManager(blob, provider)
		if err != nil {
			t.Fatal(err)
		}
		got, err := unlocked.Derive("events", "session-a/shell-a")
		if err != nil || got != want {
			t.Fatal("unlock changed the derived data key")
		}
	}
	path := filepath.Join(t.TempDir(), "master.key")
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadKeyManager(path, env)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := loaded.Derive("events", "session-a/shell-a")
	if got != want {
		t.Fatal("saved envelope did not retain the master key")
	}
	rotated := testEnvProvider(t, "TERMCP_TEST_ROTATED_KEK")
	if err := m.Rewrap(rotated); err != nil {
		t.Fatal(err)
	}
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyManager(path, env); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("old KEK still accepted after rewrap: %v", err)
	}
	loaded, err = LoadKeyManager(path, rotated)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = loaded.Derive("events", "session-a/shell-a")
	if got != want {
		t.Fatal("KEK rotation changed the data encryption key")
	}
	if _, err := UnlockKeyManager(blob, env); err != nil {
		t.Fatal("rewrapping must not invalidate a separately retained old envelope")
	}
}

func TestKeyEnvelopeRejectsTamperingAndUnboundedKDF(t *testing.T) {
	provider := testPasswordProvider(t, "placeholder passphrase")
	m, err := NewKeyManager(provider)
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := m.Marshal()
	for _, tc := range []struct {
		name string
		edit func(*keyEnvelope)
		want error
	}{
		{"store identity", func(e *keyEnvelope) { e.StoreID[0] ^= 1 }, ErrAuthentication},
		{"slot identity", func(e *keyEnvelope) { e.Slots[0].ID[0] ^= 1 }, ErrAuthentication},
		{"salt", func(e *keyEnvelope) { e.Slots[0].Parameters.Salt[0] ^= 1 }, ErrAuthentication},
		{"nonce", func(e *keyEnvelope) { e.Slots[0].Nonce[0] ^= 1 }, ErrAuthentication},
		{"wrapped key", func(e *keyEnvelope) { e.Slots[0].Wrapped[0] ^= 1 }, ErrAuthentication},
		{"version", func(e *keyEnvelope) { e.Version = 99 }, ErrVersion},
		{"memory bound", func(e *keyEnvelope) { e.Slots[0].Parameters.KDF.Memory = ^uint32(0) }, ErrFormat},
		{"CPU bound", func(e *keyEnvelope) { e.Slots[0].Parameters.KDF.Time = ^uint32(0) }, ErrFormat},
		{"parallelism bound", func(e *keyEnvelope) { e.Slots[0].Parameters.KDF.Threads = 255 }, ErrFormat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var envelope keyEnvelope
			if err := json.Unmarshal(blob, &envelope); err != nil {
				t.Fatal(err)
			}
			tc.edit(&envelope)
			changed, _ := json.Marshal(envelope)
			if _, err := UnlockKeyManager(changed, provider); !errors.Is(err, tc.want) {
				t.Fatalf("unlock = %v, want %v", err, tc.want)
			}
		})
	}
	if _, err := UnlockKeyManager(blob, testPasswordProvider(t, "different placeholder")); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("wrong password = %v", err)
	}
	for _, malformed := range [][]byte{append(bytes.Clone(blob), []byte(" {}")...), bytes.Repeat([]byte(" "), maxEnvelope+1), []byte("{")} {
		if _, err := UnlockKeyManager(malformed, provider); !errors.Is(err, ErrFormat) {
			t.Fatalf("malformed envelope = %v", err)
		}
	}
}

func TestKeyProviderFailuresPreserveExistingEnvelope(t *testing.T) {
	provider := testEnvProvider(t, "TERMCP_TEST_STORAGE_KEK")
	m, err := NewKeyManager(provider)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := m.Marshal()
	t.Setenv("TERMCP_TEST_UNAVAILABLE_KEK", "")
	if err := m.Rewrap(EnvProvider{Variable: "TERMCP_TEST_UNAVAILABLE_KEK"}); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("rewrap with unavailable provider = %v", err)
	}
	after, _ := m.Marshal()
	if !bytes.Equal(before, after) {
		t.Fatal("failed rewrap changed the working envelope")
	}
	path := filepath.Join(t.TempDir(), "master.key")
	broken := []byte("incomplete envelope")
	if err := os.WriteFile(path, broken, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyManager(path, provider); !errors.Is(err, ErrFormat) {
		t.Fatalf("corrupt envelope = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, broken) {
		t.Fatal("failed unlock replaced existing key material")
	}
}

func TestEnvProviderRequiresExactEncodedKey(t *testing.T) {
	for _, value := range []string{"", "not-base64", base64.StdEncoding.EncodeToString(make([]byte, 31)), base64.StdEncoding.EncodeToString(make([]byte, 33))} {
		t.Setenv("TERMCP_TEST_STORAGE_KEK", value)
		if _, err := (EnvProvider{Variable: "TERMCP_TEST_STORAGE_KEK"}).Key(KeyParameters{}); !errors.Is(err, ErrProviderUnavailable) {
			t.Fatalf("invalid ENV key accepted: %v", err)
		}
	}
}

func TestPasswordInitializationAndChangeRequireConfirmationAndCurrentPassword(t *testing.T) {
	cost := Argon2Parameters{Time: 1, Memory: 8 * 1024, Threads: 1}
	oldPassword := []byte("placeholder original password")
	newPassword := []byte("placeholder replacement password")
	if _, err := NewPasswordKeyManager(oldPassword, newPassword, cost); !errors.Is(err, ErrConfirmation) {
		t.Fatalf("initialization without matching confirmation = %v", err)
	}
	if _, err := NewPasswordKeyManager(nil, nil, cost); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("initialization with empty password = %v", err)
	}
	m, err := NewPasswordKeyManager(oldPassword, oldPassword, cost)
	if err != nil {
		t.Fatal(err)
	}
	key, err := m.Derive("events", "session-a/shell-a")
	if err != nil {
		t.Fatal(err)
	}
	env := testEnvProvider(t, "TERMCP_TEST_STORAGE_KEK")
	if err := m.Rewrap(testPasswordProvider(t, string(oldPassword)), env); err != nil {
		t.Fatal(err)
	}
	before, _ := m.Marshal()
	for _, tc := range []struct {
		name    string
		current []byte
		next    []byte
		confirm []byte
		want    error
	}{
		{"wrong current while unlocked", []byte("placeholder incorrect password"), newPassword, newPassword, ErrAuthentication},
		{"missing current while unlocked", nil, newPassword, newPassword, ErrAuthentication},
		{"mismatched confirmation", oldPassword, newPassword, oldPassword, ErrConfirmation},
		{"empty replacement", oldPassword, nil, nil, ErrProviderUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := m.ChangePassword(tc.current, tc.next, tc.confirm, cost); !errors.Is(err, tc.want) {
				t.Fatalf("password change = %v, want %v", err, tc.want)
			}
			after, _ := m.Marshal()
			if !bytes.Equal(before, after) {
				t.Fatal("rejected password change modified key slots")
			}
		})
	}
	path := filepath.Join(t.TempDir(), "master.key")
	if err := m.ChangePassword(oldPassword, newPassword, newPassword, cost); err != nil {
		t.Fatal(err)
	}
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyManager(path, testPasswordProvider(t, string(oldPassword))); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("old password still accepted after saving replacement: %v", err)
	}
	for _, provider := range []KeyProvider{testPasswordProvider(t, string(newPassword)), env} {
		loaded, err := LoadKeyManager(path, provider)
		if err != nil {
			t.Fatal(err)
		}
		got, err := loaded.Derive("events", "session-a/shell-a")
		if err != nil || got != key {
			t.Fatal("password change altered the data key or removed the environment slot")
		}
	}
}

func TestPasswordChangeCannotInitializeAnAbsentPasswordSlot(t *testing.T) {
	env := testEnvProvider(t, "TERMCP_TEST_STORAGE_KEK")
	m, err := NewKeyManager(env)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := m.Marshal()
	password := []byte("placeholder password")
	if err := m.ChangePassword(password, password, password, Argon2Parameters{}); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("password change without an existing password slot = %v", err)
	}
	after, _ := m.Marshal()
	if !bytes.Equal(before, after) {
		t.Fatal("unauthenticated password change replaced the environment slot")
	}
}

func TestPersistentPasswordChangePreservesStateOnFailureAndDoesNotRewriteData(t *testing.T) {
	cost := Argon2Parameters{Time: 1, Memory: 8 * 1024, Threads: 1}
	oldPassword := []byte("placeholder original password")
	newPassword := []byte("placeholder replacement password")
	m, err := NewPasswordKeyManager(oldPassword, oldPassword, cost)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	key, _ := m.Derive("config", "hosts")
	dataPath := filepath.Join(dir, "hosts.enc")
	if err := WriteFileAtomic(dataPath, key, "config/hosts", []byte("placeholder configuration")); err != nil {
		t.Fatal(err)
	}
	ciphertext, _ := os.ReadFile(dataPath)
	before, _ := m.Marshal()
	if err := m.ChangePasswordAndSave(filepath.Join(dir, "absent", "master.key"), oldPassword, newPassword, newPassword, cost); err == nil {
		t.Fatal("password change acknowledged a failed save")
	}
	after, _ := m.Marshal()
	if !bytes.Equal(before, after) {
		t.Fatal("save failure changed the active password slots")
	}
	if _, err := LoadKeyManager(path, testPasswordProvider(t, string(oldPassword))); err != nil {
		t.Fatal("save failure retired the usable password")
	}
	if err := m.ChangePasswordAndSave(path, oldPassword, newPassword, newPassword, cost); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadKeyManager(path, testPasswordProvider(t, string(newPassword)))
	if err != nil {
		t.Fatal(err)
	}
	key, _ = loaded.Derive("config", "hosts")
	got, err := ReadFile(dataPath, key, "config/hosts")
	if err != nil || string(got) != "placeholder configuration" {
		t.Fatal("password change made existing ciphertext unreadable")
	}
	afterCiphertext, err := os.ReadFile(dataPath)
	if err != nil || !bytes.Equal(ciphertext, afterCiphertext) {
		t.Fatal("password change rewrote data instead of only the key envelope")
	}
}

func TestConcurrentPersistentPasswordChangesReauthenticateLatestEnvelope(t *testing.T) {
	cost := Argon2Parameters{Time: 1, Memory: 8 * 1024, Threads: 1}
	current := []byte("placeholder original password")
	m, err := NewPasswordKeyManager(current, current, cost)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "master.key")
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	type result struct {
		password string
		err      error
	}
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for _, password := range []string{"placeholder candidate one", "placeholder candidate two"} {
		workers.Add(1)
		go func(password string) {
			defer workers.Done()
			err := m.ChangePasswordAndSave(path, current, []byte(password), []byte(password), cost)
			results <- result{password, err}
		}(password)
	}
	workers.Wait()
	close(results)
	accepted := 0
	for r := range results {
		if r.err == nil {
			accepted++
			if _, err := LoadKeyManager(path, testPasswordProvider(t, r.password)); err != nil {
				t.Fatal("acknowledged password was overwritten by a concurrent change")
			}
		} else if !errors.Is(r.err, ErrAuthentication) {
			t.Fatalf("concurrent password change = %v", r.err)
		}
	}
	if accepted != 1 {
		t.Fatal("concurrent changes did not reauthenticate the latest password")
	}
}

func TestPasswordProviderOwnsAndReleasesItsPasswordCopy(t *testing.T) {
	input := []byte("placeholder password")
	provider, err := NewPasswordProvider(input, Argon2Parameters{Time: 1, Memory: 8 * 1024, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	clear(input)
	m, err := NewKeyManager(provider)
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := m.Marshal()
	if _, err := UnlockKeyManager(blob, testPasswordProvider(t, "placeholder password")); err != nil {
		t.Fatal("provider did not retain its own password copy")
	}
	provider.Destroy()
	provider.Destroy()
	if _, err := UnlockKeyManager(blob, provider); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("destroyed password provider remained usable: %v", err)
	}
	if _, err := provider.NewParameters(); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("destroyed password provider generated a new slot: %v", err)
	}
	if _, err := m.Derive("events", "session-a/shell-a"); err != nil {
		t.Fatal("destroying a provider locked an already-unlocked store")
	}
}
