package securestore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
	"github.com/open-mcp-ai/termcp/internal/extensions"
)

func TestExtensionMigrationPreservesExistingEncryptedDataAndResumes(t *testing.T) {
	for _, alreadyEncrypted := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy-store", true: "encrypted-store"}[alreadyEncrypted], func(t *testing.T) {
			root := t.TempDir()
			body, marks := legacyFixture(t, root)
			password := []byte("placeholder migration key")
			keys, err := encryptedfs.NewPasswordKeyManager(password, password, testCost)
			if err != nil {
				t.Fatal(err)
			}
			if alreadyEncrypted {
				if pending, err := Prepare(context.Background(), root, keys, nil); err != nil || pending {
					t.Fatalf("initial migration pending=%v: %v", pending, err)
				}
			}
			raw := []byte("enabled = false\nkind = \"mcp\"\ntarget = \"internal\"\nmode = \"pipe\"\nurl = \"https://example.invalid/mcp\"\nfuture_option = \"preserve unknown fields\"\n[headers]\nAuthorization = \"placeholder migration credential\"\n")
			plainPath := filepath.Join(root, "extensions", "docs", "config.toml")
			if err := os.MkdirAll(filepath.Dir(plainPath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(plainPath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err = Prepare(ctx, root, keys, func(p Progress) {
				if p.Stage == "publish" {
					cancel()
				}
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted migration returned %v", err)
			}
			if got, err := os.ReadFile(plainPath); err != nil || !bytes.Equal(got, raw) {
				t.Fatal("interruption removed or changed the source")
			}
			resumed := false
			pending, err := Prepare(context.Background(), root, keys, func(p Progress) { resumed = resumed || p.Resuming })
			if err != nil || pending || !resumed {
				t.Fatalf("resume pending=%v resuming=%v: %v", pending, resumed, err)
			}
			verifyMigrated(t, root, keys, body, marks)
			store, err := extensions.NewEncryptedStore(root, keys)
			if err != nil {
				t.Fatal(err)
			}
			got, err := store.ReadRaw("docs")
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatal("migration changed extension credentials or unknown fields")
			}
			if _, err := os.Stat(plainPath); !os.IsNotExist(err) {
				t.Fatal("migration retained a plaintext extension")
			}
			ciphertext, err := os.ReadFile(filepath.Join(filepath.Dir(plainPath), "config.enc"))
			if err != nil || bytes.Contains(ciphertext, []byte("placeholder migration credential")) {
				t.Fatal("extension credentials were not encrypted")
			}
			if pending, err := Prepare(context.Background(), root, keys, func(Progress) { t.Error("clean startup announced another migration") }); err != nil || pending {
				t.Fatalf("clean restart pending=%v: %v", pending, err)
			}
		})
	}
}
