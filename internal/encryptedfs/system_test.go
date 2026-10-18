package encryptedfs

import (
	"errors"
	"testing"
)

type memoryCredentials struct {
	values  map[string]string
	offline bool
}

func (s *memoryCredentials) store(reference, value string) error {
	if s.offline {
		return ErrProviderUnavailable
	}
	s.values[reference] = value
	return nil
}
func (s *memoryCredentials) load(reference string) (string, error) {
	if s.offline || s.values[reference] == "" {
		return "", ErrProviderUnavailable
	}
	return s.values[reference], nil
}

// Unit tests use an injected service. Opt-in native coverage lives in
// system_native_test.go and requires the isolated runner script.
func TestCredentialProviderRequiresOriginalCredentialService(t *testing.T) {
	store := &memoryCredentials{values: map[string]string{}}
	provider := credentialProvider{name: "test-system", store: store}
	keys, err := NewKeyManager(provider)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := keys.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := keys.Derive("test", "identity")
	unlocked, err := UnlockKeyManager(encoded, provider)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := unlocked.Derive("test", "identity")
	if got != want {
		t.Fatal("system unlock changed the data key")
	}
	store.offline = true
	if _, err := UnlockKeyManager(encoded, provider); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatal("unavailable credentials did not fail closed")
	}
	store.offline = false
	store.values = map[string]string{}
	if _, err := UnlockKeyManager(encoded, provider); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatal("missing credentials were regenerated")
	}
	if _, err := provider.Key(KeyParameters{Salt: make([]byte, 16), Protected: []byte{1}}); !errors.Is(err, ErrFormat) {
		t.Fatal("foreign provider parameters accepted")
	}
}
