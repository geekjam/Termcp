package storage

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

func encryptedTestKeys(t *testing.T) *encryptedfs.KeyManager {
	t.Helper()
	key, err := encryptedfs.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMCP_TEST_BACKEND_KEK", base64.StdEncoding.EncodeToString(key[:]))
	keys, err := encryptedfs.NewKeyManager(encryptedfs.EnvProvider{Variable: "TERMCP_TEST_BACKEND_KEK"})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func TestEncryptedStoreMatchesLegacyByteLineStreamAndMarkViews(t *testing.T) {
	keys := encryptedTestKeys(t)
	root := t.TempDir()
	secure, err := NewEncrypted(root, keys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secure.Close() })
	legacy := newStore(t)
	for _, store := range []*Store{legacy, secure} {
		if err := store.SaveSession(api.Session{ID: "sessionA", Name: "private session name", Status: api.SessionRunning}); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveShell("sessionA", api.Session{ID: "shellA", Name: "private shell name", Status: api.SessionRunning}); err != nil {
			t.Fatal(err)
		}
		for i, chunk := range [][]byte{[]byte("private output marker\r\n"), {0xff, 0, '\n'}, []byte("\x1b[31mstderr\x1b[0m\n"), []byte("unterminated")} {
			offset, err := store.AppendLog("sessionA", "shellA", chunk, StdioStream(i%2))
			if err != nil {
				t.Fatal(err)
			}
			mark := api.LogMark{Offset: offset, Time: int64(i + 1000), Status: api.LogOutput}
			if i == 1 {
				mark.Status = api.LogAIInput
			}
			if err := store.AppendMark("sessionA", "shellA", mark); err != nil {
				t.Fatal(err)
			}
			if i == 1 {
				mark.Status = api.LogApprovalGranted
				if err := store.AppendMark("sessionA", "shellA", mark); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := store.SyncLineIndex("sessionA", "shellA"); err != nil {
			t.Fatal(err)
		}
	}
	size, err := legacy.LogSize("sessionA", "shellA")
	if err != nil {
		t.Fatal(err)
	}
	for offset := int64(0); offset <= size+1; offset++ {
		for _, length := range []int{1, 7, 99} {
			want, err := legacy.ReadLog("sessionA", "shellA", offset, length)
			if err != nil {
				t.Fatal(err)
			}
			got, err := secure.ReadLog("sessionA", "shellA", offset, length)
			if err != nil || !bytes.Equal(want, got) {
				t.Fatalf("byte view changed at offset=%d length=%d: %v", offset, length, err)
			}
		}
		want, wantCloser, err := legacy.ReadMarksWindow("sessionA", "shellA", offset, offset+8)
		if err != nil {
			t.Fatal(err)
		}
		got, gotCloser, err := secure.ReadMarksWindow("sessionA", "shellA", offset, offset+8)
		if err != nil || !reflect.DeepEqual(want, got) || wantCloser != gotCloser {
			t.Fatalf("mark window changed at %d: %v", offset, err)
		}
	}
	for _, read := range []func(*Store) ([]int64, error){
		func(s *Store) ([]int64, error) { return s.ReadStdioRecords("sessionA", "shellA") },
		func(s *Store) ([]int64, error) { return s.ReadLineRecords("sessionA", "shellA", 0, 100) },
	} {
		want, err := read(legacy)
		if err != nil {
			t.Fatal(err)
		}
		got, err := read(secure)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("derived history view changed: %v", err)
		}
	}
	wantSessions, _ := legacy.LoadSessions()
	gotSessions, err := secure.LoadSessions()
	if err != nil || !reflect.DeepEqual(gotSessions, wantSessions) {
		t.Fatalf("metadata changed: %v", err)
	}
	if err := secure.RecordResize("sessionA", "shellA", 40, 120); err != nil {
		t.Fatal(err)
	}
	if got, err := secure.LogSize("sessionA", "shellA"); err != nil || got != size {
		t.Fatal("resize advanced the output byte offset")
	}
	if err := secure.Close(); err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if entry.Name() != "meta.enc" && entry.Name() != "events.enc" && entry.Name() != "events.idx.enc" {
			t.Errorf("unexpected persistent file %q", entry.Name())
		}
		data, err := os.ReadFile(path)
		if err == nil && (bytes.Contains(data, []byte("private output marker")) || bytes.Contains(data, []byte("private session name"))) {
			t.Error("history or metadata appeared in plaintext")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewEncrypted(root, keys)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.ReadLog("sessionA", "shellA", 0, int(size))
	if err != nil || len(got) != int(size) {
		t.Fatal("restart lost history")
	}
	if offset, err := reopened.AppendLog("sessionA", "shellA", []byte("resume"), StdioErr); err != nil || offset != size {
		t.Fatalf("read-only handle did not upgrade safely: offset=%d err=%v", offset, err)
	}
}

func TestEncryptedDeletionRefusesLateOutputAndMetadata(t *testing.T) {
	root := t.TempDir()
	store, err := NewEncrypted(root, encryptedTestKeys(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SaveSession(api.Session{ID: "sessionA"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveShell("sessionA", api.Session{ID: "shellA"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendLog("sessionA", "shellA", []byte("history"), StdioOut); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(root, "sessions", "sessionA", "shellA", ".legacy-fixture")
	if err := os.MkdirAll(backup, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backup, "log.bin.enc"), []byte("placeholder ciphertext"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteShell("sessionA", "shellA"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendLog("sessionA", "shellA", []byte("late output"), StdioOut); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("deleted shell was revived: %v", err)
	}
	if err := store.SaveShell("sessionA", api.Session{ID: "shellA"}); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("late metadata revived a deleted shell: %v", err)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatal("shell deletion retained its migration copies")
	}
	if err := store.DeleteSession("sessionA"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveShell("sessionA", api.Session{ID: "shellA"}); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("late metadata revived a deleted session: %v", err)
	}
	if _, err := store.AppendLog("sessionA", "otherShell", []byte("late output"), StdioOut); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("late output revived a deleted session: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "sessions", "sessionA")); !os.IsNotExist(err) {
		t.Fatal("deletion recreated an orphan directory")
	}
}
