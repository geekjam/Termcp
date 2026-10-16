//go:build darwin || linux || windows

package encryptedfs_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
	"github.com/open-mcp-ai/termcp/internal/securestore"
)

const nativeIdentity = "tests/native-provider/config"

// The runner script supplies an isolated Keychain / Secret Service. Enabling
// this suite without that fixture must fail, rather than touch a login keyring.
func requireNativeFixture(t *testing.T) {
	t.Helper()
	if os.Getenv("TERMCP_TEST_NATIVE_KEYSTORE") != "1" {
		t.Skip("native key-store integration requires scripts/test_native_key_store.py")
	}
	root := os.Getenv("TERMCP_TEST_KEYSTORE_FIXTURE")
	if !filepath.IsAbs(root) {
		t.Fatal("native key-store fixture was not provisioned")
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Fatal("native key-store fixture is unavailable")
	}
}

func nativeCommand(t *testing.T, program string, args []string, input io.Reader) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, io.Discard, io.Discard
	cmd.WaitDelay = time.Second
	return cmd.Run()
}

func nativeSlot(t *testing.T, envelope []byte) encryptedfs.KeyParameters {
	t.Helper()
	var document struct {
		Slots []struct {
			Provider   string                    `json:"provider"`
			Parameters encryptedfs.KeyParameters `json:"parameters"`
		} `json:"slots"`
	}
	if err := json.Unmarshal(envelope, &document); err != nil || len(document.Slots) != 1 || document.Slots[0].Provider != encryptedfs.SystemProvider().Name() {
		t.Fatal("native provider did not produce exactly one system slot")
	}
	return document.Slots[0].Parameters
}

func deleteNativeCredential(t *testing.T, params encryptedfs.KeyParameters) {
	t.Helper()
	var err error
	reference := hex.EncodeToString(params.Salt)
	switch runtime.GOOS {
	case "darwin":
		err = nativeCommand(t, "/usr/bin/security", []string{"delete-generic-password", "-s", "org.termcp.encrypted-storage.v1", "-a", reference}, nil)
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 44 { // errSecItemNotFound
			err = nil
		}
	case "linux":
		err = nativeCommand(t, "secret-tool", []string{"clear", "service", "org.termcp.encrypted-storage.v1", "slot", reference}, nil)
	}
	if err != nil {
		t.Error("could not remove an isolated native test credential")
	}
}

func nativeCredentialCleanup(t *testing.T, params encryptedfs.KeyParameters) func() {
	t.Helper()
	var once sync.Once
	cleanup := func() { once.Do(func() { deleteNativeCredential(t, params) }) }
	t.Cleanup(cleanup)
	return cleanup
}

func replaceNativeCredential(t *testing.T, params encryptedfs.KeyParameters, secret string) {
	t.Helper()
	reference := hex.EncodeToString(params.Salt)
	var err error
	switch runtime.GOOS {
	case "darwin":
		// References and test secrets contain only hex/Base64 characters.
		command := "add-generic-password -U -s org.termcp.encrypted-storage.v1 -a " + reference + " -w " + secret + "\n"
		err = nativeCommand(t, "/usr/bin/security", []string{"-i"}, strings.NewReader(command))
	case "linux":
		err = nativeCommand(t, "secret-tool", []string{"store", "--label=Termcp native integration test", "service", "org.termcp.encrypted-storage.v1", "slot", reference}, strings.NewReader(secret))
	}
	if err != nil {
		t.Fatal("could not replace an isolated native test credential")
	}
}

func TestSystemProviderNativeRoundTrip(t *testing.T) {
	requireNativeFixture(t)
	if os.Getenv("TERMCP_TEST_NATIVE_CHILD") == "1" {
		var root string
		if err := json.NewDecoder(os.Stdin).Decode(&root); err != nil {
			t.Fatal("could not read the native child fixture")
		}
		keys, err := encryptedfs.LoadKeyManager(filepath.Join(root, "master.key"), encryptedfs.SystemProvider())
		if err != nil {
			t.Fatal("native child could not unlock the persisted master key")
		}
		key, err := keys.Derive("native-test", nativeIdentity)
		if err != nil {
			t.Fatal(err)
		}
		defer clear(key[:])
		data, err := encryptedfs.ReadFile(filepath.Join(root, "config.enc"), key, nativeIdentity)
		defer clear(data)
		if err != nil || string(data) != "native provider encrypted fixture" {
			t.Fatal("native child could not decrypt existing data")
		}
		return
	}
	provider := encryptedfs.SystemProvider()
	keys, err := encryptedfs.NewKeyManager(provider)
	if err != nil {
		t.Fatal("native system provider could not initialize a master key")
	}
	envelope, err := keys.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	params := nativeSlot(t, envelope)
	nativeCredentialCleanup(t, params)
	root := t.TempDir()
	if err := keys.Save(filepath.Join(root, "master.key")); err != nil {
		t.Fatal(err)
	}
	key, err := keys.Derive("native-test", nativeIdentity)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key[:])
	if err := encryptedfs.WriteFileAtomic(filepath.Join(root, "config.enc"), key, nativeIdentity, []byte("native provider encrypted fixture")); err != nil {
		t.Fatal(err)
	}
	kek, err := provider.Key(params)
	if err != nil {
		t.Fatal("native provider could not retrieve the original KEK")
	}
	defer clear(kek[:])
	if bytes.Contains(envelope, kek[:]) || bytes.Contains(envelope, []byte(base64.StdEncoding.EncodeToString(kek[:]))) {
		t.Fatal("the key envelope exposed the unprotected KEK")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSystemProviderNativeRoundTrip$")
	cmd.Env = append(os.Environ(), "TERMCP_TEST_NATIVE_CHILD=1")
	payload, _ := json.Marshal(root)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		t.Fatal("a fresh process could not unlock and read native-provider data")
	}
	after, err := os.ReadFile(filepath.Join(root, "master.key"))
	if err != nil || !bytes.Equal(envelope, after) {
		t.Fatal("native unlock rewrote the master-key envelope")
	}
}

func TestSystemProviderNativeRejectsUnavailableOrDamagedCredentials(t *testing.T) {
	requireNativeFixture(t)
	provider := encryptedfs.SystemProvider()
	keys, err := encryptedfs.NewKeyManager(provider)
	if err != nil {
		t.Fatal("native system provider could not initialize a master key")
	}
	envelope, err := keys.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	params := nativeSlot(t, envelope)
	remove := nativeCredentialCleanup(t, params)
	path := filepath.Join(t.TempDir(), "master.key")
	if err := keys.Save(path); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		// DPAPI stores protected bytes in the envelope, rather than a separate
		// credential item. Corrupt the actual DPAPI blob and require rejection.
		if len(params.Protected) == 0 {
			t.Fatal("DPAPI did not protect the KEK")
		}
		params.Protected[len(params.Protected)/2] ^= 1
		if _, err := provider.Key(params); !errors.Is(err, encryptedfs.ErrProviderUnavailable) {
			t.Fatal("DPAPI accepted a corrupted protected KEK")
		}
	} else {
		remove()
		for attempt := 0; attempt < 2; attempt++ {
			if _, err := encryptedfs.LoadKeyManager(path, provider); !errors.Is(err, encryptedfs.ErrProviderUnavailable) {
				t.Fatal("missing native credentials were accepted or regenerated")
			}
		}
		wrong, err := encryptedfs.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		replaceNativeCredential(t, params, base64.StdEncoding.EncodeToString(wrong[:]))
		nativeCredentialCleanup(t, params)
		clear(wrong[:])
		if _, err := encryptedfs.LoadKeyManager(path, provider); !errors.Is(err, encryptedfs.ErrAuthentication) {
			t.Fatal("a different native KEK unlocked the existing master key")
		}
		replaceNativeCredential(t, params, "invalid-base64")
		if _, err := encryptedfs.LoadKeyManager(path, provider); !errors.Is(err, encryptedfs.ErrProviderUnavailable) {
			t.Fatal("malformed native credentials were accepted")
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(envelope, after) {
		t.Fatal("failed native unlock changed the existing key envelope")
	}
}

func TestSystemProviderNativeControllerRestart(t *testing.T) {
	requireNativeFixture(t)
	t.Setenv(encryptedfs.EnvStorageKEK, "")
	t.Setenv("TERMCP_STORAGE_KEY_PROVIDER", "system")
	root := t.TempDir()
	derived := make(chan encryptedfs.Key, 1)
	activate := func(keys *encryptedfs.KeyManager) (http.Handler, io.Closer, error) {
		key, err := keys.Derive("native-test", nativeIdentity)
		if err != nil {
			return nil, nil, err
		}
		derived <- key
		return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), io.NopCloser(strings.NewReader("")), nil
	}
	open := func() *securestore.Controller {
		t.Helper()
		controller, err := securestore.Open(root, activate)
		if err != nil {
			t.Fatal("native system bootstrap could not open storage")
		}
		t.Cleanup(func() { _ = controller.Close() })
		deadline := time.NewTimer(30 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			status := controller.Status()
			if status.State == securestore.Ready {
				if status.PasswordConfigured || controller.Handler() == nil {
					t.Fatal("native mode did not activate without a password")
				}
				return controller
			}
			if status.ErrorCode != "" || status.State == securestore.Failed {
				t.Fatal("native system bootstrap failed")
			}
			select {
			case <-ticker.C:
			case <-deadline.C:
				t.Fatal("native system bootstrap did not become ready")
			}
		}
	}
	controller := open()
	want := <-derived
	defer clear(want[:])
	envelope, err := os.ReadFile(filepath.Join(root, "keys", "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	params := nativeSlot(t, envelope)
	remove := nativeCredentialCleanup(t, params)
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	controller = open()
	got := <-derived
	defer clear(got[:])
	if got != want {
		t.Fatal("native restart generated a different master key")
	}
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		// Exercise the application's locked startup path with an actual damaged
		// DPAPI blob, preserving every other field in the on-disk envelope.
		var document map[string]json.RawMessage
		var slots []map[string]json.RawMessage
		if err := json.Unmarshal(envelope, &document); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(document["slots"], &slots); err != nil || len(slots) != 1 || len(params.Protected) == 0 {
			t.Fatal("invalid DPAPI fixture envelope")
		}
		params.Protected[len(params.Protected)/2] ^= 1
		slots[0]["parameters"], err = json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		document["slots"], err = json.Marshal(slots)
		if err != nil {
			t.Fatal(err)
		}
		envelope, err = json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "keys", "master.key"), envelope, 0600); err != nil {
			t.Fatal(err)
		}
	} else {
		remove()
	}
	controller, err = securestore.Open(root, activate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	if status := controller.Status(); status.State != securestore.Locked || status.ErrorCode != "system_unlock_failed" || controller.Handler() != nil {
		t.Fatal("unavailable native credentials did not keep business operations locked")
	}
	if err := controller.Initialize([]byte("placeholder"), []byte("placeholder")); !errors.Is(err, securestore.ErrState) {
		t.Fatal("unavailable native credentials allowed master-key replacement")
	}
	after, err := os.ReadFile(filepath.Join(root, "keys", "master.key"))
	if err != nil || !bytes.Equal(envelope, after) {
		t.Fatal("native bootstrap changed the existing key envelope")
	}
}
