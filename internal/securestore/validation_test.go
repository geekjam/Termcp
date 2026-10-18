package securestore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
)

func TestMigrationRefusesOrphanHistoryAndMalformedMarks(t *testing.T) {
	for _, scenario := range []string{"orphan session", "orphan shell", "partial mark"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			legacyFixture(t, root)
			switch scenario {
			case "orphan session":
				if err := os.Remove(filepath.Join(root, "sessions", "sessionA", "manifest.json")); err != nil {
					t.Fatal(err)
				}
			case "orphan shell":
				if err := os.Remove(filepath.Join(root, "sessions", "sessionA", "shellA", "manifest.json")); err != nil {
					t.Fatal(err)
				}
			case "partial mark":
				if err := os.WriteFile(filepath.Join(root, "sessions", "sessionA", "shellA", "log.jsonl"), []byte(`{"offset":0,"status":`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(root, "sessions", "sessionA", "shellA", "log.bin")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			password := []byte("placeholder validation password")
			keys, err := encryptedfs.NewPasswordKeyManager(password, password, testCost)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Prepare(context.Background(), root, keys, nil); !errors.Is(err, ErrLegacyFormat) {
				t.Fatalf("invalid history was accepted: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "store.enc")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid history was committed")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("failed migration modified the source history")
			}
		})
	}
}

func TestMigrationWriteFailureLeavesSourcesAndCanBeRetried(t *testing.T) {
	root := t.TempDir()
	body, marks := legacyFixture(t, root)
	password := []byte("placeholder validation password")
	keys, err := encryptedfs.NewPasswordKeyManager(password, password, testCost)
	if err != nil {
		t.Fatal(err)
	}
	// A file blocking the journal directory is deterministic on all three OSes,
	// unlike chmod-based failures on privileged CI users or differing ACLs.
	blocker := filepath.Join(root, ".migration")
	if err := os.WriteFile(blocker, []byte("placeholder I/O obstruction"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(context.Background(), root, keys, nil); err == nil {
		t.Fatal("journal write failure was ignored")
	}
	if _, err := os.Stat(filepath.Join(root, "store.enc")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed write committed a new format")
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	resumed := false
	ctx, cancel := context.WithCancel(context.Background())
	_, err = Prepare(ctx, root, keys, func(p Progress) {
		if p.Stage == "archive" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("migration interruption was ignored")
	}
	if pending, err := Prepare(context.Background(), root, keys, func(p Progress) { resumed = resumed || p.Resuming }); err != nil || pending || !resumed {
		t.Fatalf("retry pending=%v resuming=%v error=%v", pending, resumed, err)
	}
	verifyMigrated(t, root, keys, body, marks)
}

func TestMigrationDoesNotRemoveUnmanagedProfileFiles(t *testing.T) {
	root := t.TempDir()
	legacyFixture(t, root)
	path := filepath.Join(root, "ssh_configs", "saved", "notes.txt")
	want := []byte("placeholder user-owned notes")
	if err := os.WriteFile(path, want, 0600); err != nil {
		t.Fatal(err)
	}
	password := []byte("placeholder validation password")
	keys, err := encryptedfs.NewPasswordKeyManager(password, password, testCost)
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := Prepare(context.Background(), root, keys, nil); pending || err != nil {
		t.Fatalf("pending=%v error=%v", pending, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(want, got) {
		t.Fatal("cleanup removed an unmanaged user file")
	}
	if _, err := os.Stat(filepath.Join(root, ".migration")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("completed migration retained staging copies")
	}
}
