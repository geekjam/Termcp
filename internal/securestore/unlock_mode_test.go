package securestore

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
)

func TestSavedUnlockModeControlsRestartsAndSurvivesPasswordChanges(t *testing.T) {
	options := autoUnlockOptions(t)
	options.InitializeSystem = true
	root := t.TempDir()
	c := openAutoController(t, root, options)
	waitPrepared(t, c)
	want, _ := c.keys.Derive("events", "placeholder-session")
	password := []byte("placeholder selected manual password")
	if err := c.SetUnlockMode(encryptedfs.UnlockPassword, nil, password, password); err != nil {
		t.Fatal(err)
	}
	if status := c.Status(); status.UnlockMode != encryptedfs.UnlockPassword || !status.UnlockModeSaved || status.AutoUnlock || !status.PasswordConfigured || c.Handler() == nil {
		t.Fatal("manual selection did not preserve the running service")
	}
	for range 2 {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		c = openAutoController(t, root, options)
		if status := c.Status(); status.State != Locked || status.AutoUnlock || status.UnlockMode != encryptedfs.UnlockPassword || c.Handler() != nil {
			t.Fatal("saved manual mode was ignored on restart")
		}
		before, _ := os.ReadFile(c.keyPath())
		if err := c.Unlock(password); err != nil {
			t.Fatal(err)
		}
		waitPrepared(t, c)
		after, _ := os.ReadFile(c.keyPath())
		if c.Status().AutoUnlock || c.keys.HasProvider("test-system") || !bytes.Equal(before, after) {
			t.Fatal("manual unlock silently re-enrolled automatic hosting")
		}
	}
	if err := c.SetUnlockMode(encryptedfs.UnlockAutomatic, []byte("incorrect placeholder"), nil, nil); !errors.Is(err, encryptedfs.ErrAuthentication) {
		t.Fatal("switching modes bypassed current-password proof")
	}
	next := []byte("placeholder rotated manual password")
	if err := c.ChangePassword(password, next, next); err != nil {
		t.Fatal(err)
	}
	if c.Status().UnlockMode != encryptedfs.UnlockPassword {
		t.Fatal("password rotation cleared the saved mode")
	}
	if err := c.SetUnlockMode(encryptedfs.UnlockAutomatic, next, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// An explicit Web UI preference takes precedence over the old deployment
	// default, while a backend remains available for automatic hosting.
	options.RequirePassword = true
	c = openAutoController(t, root, options)
	if status := waitPrepared(t, c); !status.AutoUnlock || status.UnlockMode != encryptedfs.UnlockAutomatic {
		t.Fatal("saved automatic mode did not survive restart")
	}
	got, _ := c.keys.Derive("events", "placeholder-session")
	if got != want {
		t.Fatal("mode selection or password rotation changed existing data")
	}
}

func TestSavedManualModeOverridesEnvironmentAndFailedSwitchPreservesPolicy(t *testing.T) {
	key, err := encryptedfs.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(key[:])
	t.Setenv(encryptedfs.EnvStorageKEK, encoded)
	root := t.TempDir()
	options := Options{PasswordCost: testCost}
	c := openAutoController(t, root, options)
	waitPrepared(t, c)
	password := []byte("placeholder environment manual password")
	if err := c.SetUnlockMode(encryptedfs.UnlockPassword, nil, password, password); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = openAutoController(t, root, options)
	if c.Status().State != Locked {
		t.Fatal("ENV overrode a saved manual selection")
	}
	if err := c.Unlock(password); err != nil {
		t.Fatal(err)
	}
	waitPrepared(t, c)
	before, _ := os.ReadFile(c.keyPath())
	t.Setenv(encryptedfs.EnvStorageKEK, "")
	if err := c.SetUnlockMode(encryptedfs.UnlockAutomatic, password, nil, nil); !errors.Is(err, encryptedfs.ErrProviderUnavailable) {
		t.Fatal("unavailable hosting reported automatic unlock success")
	}
	after, _ := os.ReadFile(c.keyPath())
	if !bytes.Equal(before, after) || c.Status().UnlockMode != encryptedfs.UnlockPassword || c.Status().AutoUnlock {
		t.Fatal("failed automatic selection discarded manual recovery")
	}
	t.Setenv(encryptedfs.EnvStorageKEK, encoded)
	if err := c.SetUnlockMode(encryptedfs.UnlockAutomatic, password, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = openAutoController(t, root, options)
	if status := waitPrepared(t, c); !status.AutoUnlock || status.UnlockMode != encryptedfs.UnlockAutomatic {
		t.Fatal("restored ENV hosting did not restart automatically")
	}
}
