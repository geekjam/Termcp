package securestore

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
)

func autoUnlockOptions(t *testing.T) Options {
	t.Helper()
	t.Setenv(encryptedfs.EnvStorageKEK, "")
	key, err := encryptedfs.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMCP_TEST_SYSTEM_KEK", base64.StdEncoding.EncodeToString(key[:]))
	return Options{PasswordCost: testCost, SystemProvider: testSystemProvider{encryptedfs.EnvProvider{Variable: "TERMCP_TEST_SYSTEM_KEK"}}}
}

func openAutoController(t *testing.T, root string, options Options) *Controller {
	t.Helper()
	c, err := OpenWithOptions(root, fakeActivation, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestPasswordSetupEnrollsAutomaticUnlockAndRetainsPasswordProof(t *testing.T) {
	options := autoUnlockOptions(t)
	root := t.TempDir()
	password, next := []byte("placeholder original passphrase"), []byte("placeholder new passphrase")
	c := openAutoController(t, root, options)
	if c.Status().State != Uninitialized {
		t.Fatal("default setup skipped custom password initialization")
	}
	if err := c.Initialize(password, password); err != nil {
		t.Fatal(err)
	}
	if status := waitPrepared(t, c); !status.AutoUnlock || !status.PasswordConfigured || status.AutoUnlockError != "" {
		t.Fatalf("password setup did not enroll automatic unlock: %+v", status)
	}
	want, _ := c.keys.Derive("events", "placeholder-session")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = openAutoController(t, root, options)
	if status := waitPrepared(t, c); !status.AutoUnlock || c.Handler() == nil {
		t.Fatal("restart required another password")
	}
	got, _ := c.keys.Derive("events", "placeholder-session")
	if got != want {
		t.Fatal("automatic restart changed the data key")
	}
	if err := c.ChangePassword([]byte("incorrect placeholder"), next, next); !errors.Is(err, encryptedfs.ErrAuthentication) {
		t.Fatal("automatic unlock bypassed current-password proof")
	}
	if err := c.ChangePassword(password, next, []byte("mismatch")); !errors.Is(err, encryptedfs.ErrConfirmation) {
		t.Fatal("password change bypassed confirmation")
	}
	if err := c.ChangePassword(password, next, next); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = openAutoController(t, root, options)
	waitPrepared(t, c)
	got, _ = c.keys.Derive("events", "placeholder-session")
	if got != want || !c.Status().AutoUnlock {
		t.Fatal("password change broke automatic unlock or existing history")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// Missing credentials keep startup locked and do not mutate the envelope.
	t.Setenv("TERMCP_TEST_SYSTEM_KEK", "")
	c = openAutoController(t, root, options)
	before, _ := os.ReadFile(c.keyPath())
	if status := c.Status(); status.State != Locked || status.AutoUnlock || status.ErrorCode != "system_unlock_failed" || c.Handler() != nil {
		t.Fatal("missing native credentials did not require password recovery")
	}
	if err := c.Unlock(password); !errors.Is(err, encryptedfs.ErrAuthentication) {
		t.Fatal("retired password still unlocks storage")
	}
	after, _ := os.ReadFile(c.keyPath())
	if !bytes.Equal(before, after) {
		t.Fatal("failed recovery changed the envelope")
	}
	if err := c.Unlock(next); err != nil {
		t.Fatal(err)
	}
	if status := waitPrepared(t, c); status.AutoUnlock || status.AutoUnlockError != "automatic_unlock_unavailable" {
		t.Fatal("unavailable hosting did not preserve manual recovery with a warning")
	}
	got, _ = c.keys.Derive("events", "placeholder-session")
	if got != want {
		t.Fatal("manual recovery changed the master key")
	}
}

func TestExistingPasswordStoreEnrollsOnceAfterAuthenticatedUnlock(t *testing.T) {
	options := autoUnlockOptions(t)
	options.InitializeSystem = true
	root := t.TempDir()
	password := []byte("placeholder upgrade passphrase")
	c := openAutoController(t, root, Options{PasswordCost: testCost})
	if err := c.Initialize(password, password); err != nil {
		t.Fatal(err)
	}
	waitPrepared(t, c)
	want, _ := c.keys.Derive("config", "hosts")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = openAutoController(t, root, options)
	if c.Status().State != Locked || c.Status().ErrorCode != "" || c.Status().AutoUnlock {
		t.Fatal("upgrade enrolled without authenticating the existing password")
	}
	if err := c.Unlock(password); err != nil {
		t.Fatal(err)
	}
	if status := waitPrepared(t, c); !status.AutoUnlock {
		t.Fatal("upgrade did not enroll native hosting")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = openAutoController(t, root, options)
	waitPrepared(t, c)
	got, _ := c.keys.Derive("config", "hosts")
	if got != want || !c.Status().AutoUnlock {
		t.Fatal("upgraded store did not restart with the same key")
	}
}

func TestUnavailableSystemHostingFallsBackToPasswordAndCanEnrollLater(t *testing.T) {
	options := autoUnlockOptions(t)
	options.InitializeSystem = true
	credential := os.Getenv("TERMCP_TEST_SYSTEM_KEK")
	t.Setenv("TERMCP_TEST_SYSTEM_KEK", "")
	root := t.TempDir()
	c := openAutoController(t, root, options)
	if c.Status().State != Uninitialized || c.Status().ErrorCode != "system_unlock_failed" {
		t.Fatal("unavailable native hosting did not offer explicit password setup")
	}
	password := []byte("placeholder offline setup passphrase")
	if err := c.Initialize(password, password); err != nil {
		t.Fatal(err)
	}
	if status := waitPrepared(t, c); status.AutoUnlock || status.AutoUnlockError == "" || !status.PasswordConfigured {
		t.Fatal("password fallback hid its manual-unlock requirement")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMCP_TEST_SYSTEM_KEK", credential)
	c = openAutoController(t, root, options)
	if c.Status().State != Locked || c.Status().ErrorCode != "" {
		t.Fatal("a newly available system service enrolled without password proof")
	}
	if err := c.Unlock(password); err != nil {
		t.Fatal(err)
	}
	if status := waitPrepared(t, c); !status.AutoUnlock || status.AutoUnlockError != "" {
		t.Fatal("successful password proof did not enroll the restored system service")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = openAutoController(t, root, options)
	waitPrepared(t, c)
}

func TestPasswordStoreCanEnrollEnvironmentForUnattendedRestart(t *testing.T) {
	t.Setenv(encryptedfs.EnvStorageKEK, "")
	root := t.TempDir()
	password := []byte("placeholder container passphrase")
	options := Options{PasswordCost: testCost}
	c := openAutoController(t, root, options)
	if err := c.Initialize(password, password); err != nil {
		t.Fatal(err)
	}
	waitPrepared(t, c)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	key, err := encryptedfs.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(encryptedfs.EnvStorageKEK, base64.StdEncoding.EncodeToString(key[:]))
	c = openAutoController(t, root, options)
	if c.Status().State != Locked || c.Status().ErrorCode != "environment_unlock_failed" {
		t.Fatal("adding ENV bypassed existing password authentication")
	}
	if err := c.Unlock(password); err != nil {
		t.Fatal(err)
	}
	waitPrepared(t, c)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = openAutoController(t, root, options)
	if status := waitPrepared(t, c); !status.AutoUnlock || !status.PasswordConfigured {
		t.Fatal("container restart required a password or removed password recovery")
	}
}

func TestManagedStoreSupportsOptionalPasswordWithoutReplacingDataKey(t *testing.T) {
	options := autoUnlockOptions(t)
	options.InitializeSystem = true
	root := t.TempDir()
	c := openAutoController(t, root, options)
	if status := waitPrepared(t, c); !status.AutoUnlock || status.PasswordConfigured {
		t.Fatal("system hosting required a password on first startup")
	}
	want, _ := c.keys.Derive("events", "placeholder-session")
	password, next := []byte("placeholder optional passphrase"), []byte("placeholder changed passphrase")
	if err := c.Initialize(password, []byte("mismatch")); !errors.Is(err, encryptedfs.ErrConfirmation) {
		t.Fatal("first optional password bypassed confirmation")
	}
	if c.Status().PasswordConfigured {
		t.Fatal("failed setup published a password")
	}
	if err := c.Initialize(password, password); err != nil {
		t.Fatal(err)
	}
	if !c.Status().PasswordConfigured || !c.Status().AutoUnlock {
		t.Fatal("optional password disabled automatic unlock")
	}
	if err := c.Initialize(next, next); !errors.Is(err, ErrState) {
		t.Fatal("first-setup endpoint replaced an existing password")
	}
	if err := c.ChangePassword([]byte("incorrect placeholder"), next, next); !errors.Is(err, encryptedfs.ErrAuthentication) {
		t.Fatal("managed mode bypassed current-password proof")
	}
	if err := c.ChangePassword(password, next, next); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = openAutoController(t, root, options)
	waitPrepared(t, c)
	got, _ := c.keys.Derive("events", "placeholder-session")
	if got != want || !c.Status().AutoUnlock {
		t.Fatal("optional password setup or rotation changed existing data or startup")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMCP_TEST_SYSTEM_KEK", "")
	c = openAutoController(t, root, options)
	if err := c.Unlock(next); err != nil {
		t.Fatal("optional password cannot recover after losing system credentials")
	}
	waitPrepared(t, c)
}
