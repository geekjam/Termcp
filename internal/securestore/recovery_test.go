package securestore

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

type testSystemProvider struct{ encryptedfs.EnvProvider }

func (testSystemProvider) Name() string { return "test-system" }

func TestControllerSystemAutoUnlockNeverReplacesMissingCredentials(t *testing.T) {
	t.Setenv(encryptedfs.EnvStorageKEK, "")
	// The injected system provider has no side effects on the runner's keyring.
	key, err := encryptedfs.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMCP_TEST_SYSTEM_KEK", base64.StdEncoding.EncodeToString(key[:]))
	root := t.TempDir()
	options := Options{SystemProvider: testSystemProvider{encryptedfs.EnvProvider{Variable: "TERMCP_TEST_SYSTEM_KEK"}}, InitializeSystem: true}
	c, err := OpenWithOptions(root, fakeActivation, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if status := waitPrepared(t, c); status.PasswordConfigured {
		t.Fatal("system mode invented a password")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = OpenWithOptions(root, fakeActivation, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	waitPrepared(t, c)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, "keys", "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMCP_TEST_SYSTEM_KEK", "")
	c, err = OpenWithOptions(root, fakeActivation, options)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Status().State != Locked || c.Status().ErrorCode != "system_unlock_failed" || c.Handler() != nil {
		t.Fatal("missing system credentials did not keep storage locked")
	}
	if err := c.Initialize([]byte("placeholder"), []byte("placeholder")); !errors.Is(err, ErrState) {
		t.Fatal("missing credentials allowed replacement of the master key")
	}
	after, _ := os.ReadFile(filepath.Join(root, "keys", "master.key"))
	if !bytes.Equal(before, after) {
		t.Fatal("unavailable provider changed the envelope")
	}
}

func TestControllerRecoversIncompleteTailButRefusesAuthenticatedCorruption(t *testing.T) {
	t.Setenv(encryptedfs.EnvStorageKEK, "")
	for _, incomplete := range []bool{true, false} {
		name := "authenticated corruption"
		if incomplete {
			name = "incomplete tail"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			password := []byte("placeholder recovery password")
			keys, err := encryptedfs.NewPasswordKeyManager(password, password, testCost)
			if err != nil {
				t.Fatal(err)
			}
			if err := encryptedfs.MkdirAllDurable(filepath.Join(root, "keys")); err != nil {
				t.Fatal(err)
			}
			if err := keys.Save(filepath.Join(root, "keys", "master.key")); err != nil {
				t.Fatal(err)
			}
			if _, err := Prepare(context.Background(), root, keys, nil); err != nil {
				t.Fatal(err)
			}
			store, err := storage.NewEncrypted(root, keys)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			if err := store.SaveSession(api.Session{ID: "sessionA"}); err != nil {
				t.Fatal(err)
			}
			if err := store.SaveShell("sessionA", api.Session{ID: "shellA"}); err != nil {
				t.Fatal(err)
			}
			for _, data := range []string{"complete prefix", "unfinished tail"} {
				if _, err := store.AppendLog("sessionA", "shellA", []byte(data), storage.StdioOut); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "sessions", "sessionA", "shellA", "events.enc")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if incomplete {
				data = data[:len(data)-1]
			} else {
				data[len(data)-1] ^= 1
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			c, err := openController(root, fakeActivation, testCost)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if err := c.Unlock(password); err != nil {
				t.Fatal(err)
			}
			if incomplete {
				waitPrepared(t, c)
				store, err := storage.NewEncrypted(root, keys)
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				got, err := store.ReadLog("sessionA", "shellA", 0, 4096)
				if err != nil || string(got) != "complete prefix" {
					t.Fatal("tail recovery lost the complete prefix")
				}
				backups, _ := filepath.Glob(path + ".recovery-*")
				if len(backups) != 1 {
					t.Fatal("tail recovery did not preserve the original ciphertext")
				}
				preserved, _ := os.ReadFile(backups[0])
				if !bytes.Equal(preserved, data) {
					t.Fatal("recovery backup changed")
				}
			} else {
				deadline := time.NewTimer(20 * time.Second)
				defer deadline.Stop()
				ticker := time.NewTicker(5 * time.Millisecond)
				defer ticker.Stop()
				for c.Status().State != Failed {
					if c.Handler() != nil {
						t.Fatal("corrupted history became available")
					}
					select {
					case <-ticker.C:
					case <-deadline.C:
						t.Fatal("corruption detection timed out")
					}
				}
				unchanged, _ := os.ReadFile(path)
				if !bytes.Equal(unchanged, data) {
					t.Fatal("authenticated corruption was truncated")
				}
			}
		})
	}
}
