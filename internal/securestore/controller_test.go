package securestore

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

var testCost = encryptedfs.Argon2Parameters{Time: 1, Memory: 8 * 1024, Threads: 1}

func waitPrepared(t *testing.T, c *Controller) Status {
	t.Helper()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		status := c.Status()
		if status.State == Ready || status.State == CleanupPending {
			return status
		}
		if status.State == Failed {
			t.Fatalf("storage preparation failed: %s", status.ErrorCode)
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("storage preparation timed out: %+v", status)
		}
	}
}

func fakeActivation(*encryptedfs.KeyManager) (http.Handler, io.Closer, error) {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ready") }), io.NopCloser(strings.NewReader("")), nil
}

func legacyFixture(t *testing.T, root string) ([]byte, []api.LogMark) {
	t.Helper()
	store := storage.New(root)
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SaveSession(api.Session{ID: "sessionA", Name: "placeholder session", Status: api.SessionRunning}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveShell("sessionA", api.Session{ID: "shellA", Name: "placeholder shell", Status: api.SessionRunning}); err != nil {
		t.Fatal(err)
	}
	chunks := [][]byte{[]byte("private output marker\r\n"), {0xff, 0, '\n'}, []byte("\x1b[31mstderr\x1b[0m\n"), []byte("tail")}
	var body []byte
	var marks []api.LogMark
	for i, chunk := range chunks {
		offset, err := store.AppendLog("sessionA", "shellA", chunk, storage.StdioStream(i%2))
		if err != nil {
			t.Fatal(err)
		}
		mark := api.LogMark{Offset: offset, Time: int64(1000 + i), Status: api.LogOutput}
		if err := store.AppendMark("sessionA", "shellA", mark); err != nil {
			t.Fatal(err)
		}
		marks = append(marks, mark)
		body = append(body, chunk...)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	hosts := sshconfig.NewStore(root)
	raw := []byte("kind = \"remote\"\nhost = \"example.invalid\"\nuser = \"placeholder\"\npassword = \"placeholder host credential\"\n")
	if err := hosts.Save("saved", raw); err != nil {
		t.Fatal(err)
	}
	if err := hosts.SaveWithOptions("temporary", raw, true); err != nil {
		t.Fatal(err)
	}
	if err := hosts.Save("internal", []byte("kind = \"internal\"\ndescription = \"placeholder override\"\n")); err != nil {
		t.Fatal(err)
	}
	return body, marks
}

func verifyMigrated(t *testing.T, root string, keys *encryptedfs.KeyManager, body []byte, marks []api.LogMark) {
	t.Helper()
	store, err := storage.NewEncrypted(root, keys)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, err := store.ReadLog("sessionA", "shellA", 0, len(body)+100)
	if err != nil || !bytes.Equal(body, got) {
		t.Fatal("migration changed output bytes or their logical offsets")
	}
	gotMarks, err := store.ReadMarks("sessionA", "shellA")
	if err != nil || !reflect.DeepEqual(gotMarks, marks) {
		t.Fatal("migration changed status marks")
	}
	lines, err := store.ReadLineRecords("sessionA", "shellA", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var expectedLines []int64
	for i, b := range body {
		if b == '\n' {
			expectedLines = append(expectedLines, int64(i))
		}
	}
	if !reflect.DeepEqual(lines, expectedLines) {
		t.Fatal("line positions changed during migration")
	}
	sessions, err := store.LoadSessions()
	if err != nil || len(sessions) != 1 || sessions[0].ID != "sessionA" || len(sessions[0].Shells) != 1 || sessions[0].Shells[0].ID != "shellA" {
		t.Fatalf("session/shell identities changed: %v", err)
	}
	hosts, err := sshconfig.NewEncryptedStore(root, keys)
	if err != nil {
		t.Fatal(err)
	}
	names, err := hosts.List()
	if err != nil || !reflect.DeepEqual(names, []string{"internal", "saved"}) {
		t.Fatalf("migration changed profiles or persisted a temporary host: %v", err)
	}
	internal, err := hosts.Load("internal")
	if err != nil || internal.Description != "placeholder override" {
		t.Fatal("migration lost the internal profile override")
	}
}

func TestControllerMigratesAndReauthenticatesPasswordChanges(t *testing.T) {
	t.Setenv(encryptedfs.EnvStorageKEK, "")
	root := t.TempDir()
	body, marks := legacyFixture(t, root)
	c, err := openController(root, fakeActivation, testCost)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if c.Status().State != Uninitialized || !c.Status().LegacyFound || c.Handler() != nil {
		t.Fatal("legacy storage was exposed before initialization")
	}
	password := []byte("placeholder master password")
	if err := c.Initialize(password, []byte("mismatched placeholder")); !errors.Is(err, encryptedfs.ErrConfirmation) {
		t.Fatalf("initial confirmation was not checked: %v", err)
	}
	if _, err := os.Stat(c.keyPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected initialization persisted a key envelope")
	}
	if err := c.Initialize(password, password); err != nil {
		t.Fatal(err)
	}
	if status := waitPrepared(t, c); status.State != Ready {
		t.Fatalf("unexpected cleanup state: %+v", status)
	}
	verifyMigrated(t, root, c.keys, body, marks)
	if _, err := os.Stat(filepath.Join(root, "sessions", "sessionA", "shellA", "log.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old plaintext output was retained after successful cleanup")
	}
	if _, err := os.Stat(filepath.Join(root, "ssh_configs", "saved")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("migration retained an empty plaintext profile-name directory")
	}
	if err := c.Initialize(password, password); !errors.Is(err, ErrState) {
		t.Fatal("initialized storage accepted another initialization")
	}
	replacement := []byte("placeholder replacement password")
	if err := c.ChangePassword([]byte("incorrect placeholder"), replacement, replacement); !errors.Is(err, encryptedfs.ErrAuthentication) {
		t.Fatalf("already unlocked store skipped current password verification: %v", err)
	}
	if err := c.ChangePassword(password, replacement, replacement); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openController(root, fakeActivation, testCost)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Status().State != Locked || reopened.Handler() != nil {
		t.Fatal("password store did not require unlock after restart")
	}
	if err := reopened.Unlock(password); !errors.Is(err, encryptedfs.ErrAuthentication) {
		t.Fatalf("old password accepted after persisted change: %v", err)
	}
	if err := reopened.Unlock(replacement); err != nil {
		t.Fatal(err)
	}
	waitPrepared(t, reopened)
	verifyMigrated(t, root, reopened.keys, body, marks)
}

func TestInterruptedMigrationResumesWithoutPublishingPartialHistory(t *testing.T) {
	root := t.TempDir()
	body, marks := legacyFixture(t, root)
	keys, err := encryptedfs.NewPasswordKeyManager([]byte("placeholder password"), []byte("placeholder password"), testCost)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err = Prepare(ctx, root, keys, func(p Progress) {
		if p.Stage == "archive" && p.Completed == 1 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted migration = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "store.enc")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("incomplete migration published the store commit record")
	}
	if _, err := os.Stat(filepath.Join(root, "sessions", "sessionA", "shellA", "log.bin")); err != nil {
		t.Fatal("interrupted migration removed legacy history")
	}
	pending, err := Prepare(context.Background(), root, keys, nil)
	if err != nil || pending {
		t.Fatalf("resumed migration pending=%v err=%v", pending, err)
	}
	verifyMigrated(t, root, keys, body, marks)
	if _, err := Prepare(context.Background(), root, keys, func(Progress) { t.Error("clean restart repeated migration") }); err != nil {
		t.Fatal(err)
	}
}

func TestSourceChangesPreventCommitAndCleanupFailuresRemainVisible(t *testing.T) {
	for _, changeStage := range []string{"publish", "cleanup"} {
		t.Run(changeStage, func(t *testing.T) {
			root := t.TempDir()
			body, marks := legacyFixture(t, root)
			keys, err := encryptedfs.NewPasswordKeyManager([]byte("placeholder password"), []byte("placeholder password"), testCost)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "ssh_configs", "saved", "config.toml")
			changed := []byte("changed during migration")
			pending, err := Prepare(context.Background(), root, keys, func(progress Progress) {
				if progress.Stage == changeStage {
					if err := os.WriteFile(path, changed, 0600); err != nil {
						t.Error(err)
					}
				}
			})
			if changeStage == "publish" {
				if !errors.Is(err, ErrLegacyChanged) {
					t.Fatalf("changing source did not block commit: %v", err)
				}
				if _, err := os.Stat(filepath.Join(root, "store.enc")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("changed source was committed")
				}
			} else {
				if err != nil || !pending {
					t.Fatalf("cleanup failure did not remain visible: pending=%v err=%v", pending, err)
				}
				verifyMigrated(t, root, keys, body, marks)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, changed) {
				t.Fatal("migration erased a source modified by another writer")
			}
		})
	}
}

func TestDirectoryOwnershipMissingKeyAndEnvironmentUnlock(t *testing.T) {
	t.Setenv(encryptedfs.EnvStorageKEK, "")
	root := t.TempDir()
	c, err := openController(root, fakeActivation, testCost)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if other, err := openController(root, fakeActivation, testCost); !errors.Is(err, encryptedfs.ErrBusy) {
		if other != nil {
			_ = other.Close()
		}
		t.Fatalf("second process acquired storage ownership: %v", err)
	}
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, "store.enc"), []byte("placeholder ciphertext"), 0600); err != nil {
		t.Fatal(err)
	}
	if other, err := openController(broken, fakeActivation, testCost); !errors.Is(err, ErrMissingKey) {
		if other != nil {
			_ = other.Close()
		}
		t.Fatalf("missing key was silently regenerated: %v", err)
	}
	key, err := encryptedfs.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(encryptedfs.EnvStorageKEK, base64.StdEncoding.EncodeToString(key[:]))
	auto, err := openController(t.TempDir(), fakeActivation, testCost)
	if err != nil {
		t.Fatal(err)
	}
	defer auto.Close()
	if status := waitPrepared(t, auto); status.PasswordConfigured {
		t.Fatal("environment-only initialization fabricated a password slot")
	}
}
