package extensions

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
)

func TestEncryptedTemplatesPreserveRawBytesAndRejectSubstitution(t *testing.T) {
	password := []byte("placeholder extension key")
	keys, err := encryptedfs.NewPasswordKeyManager(password, password, encryptedfs.Argon2Parameters{Time: 1, Memory: 8 * 1024, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	store, err := NewEncryptedStore(root, keys)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("kind = \"mcp\"\ntarget = \"internal\"\nmode = \"pipe\"\nurl = \"https://example.invalid/mcp\"\nfuture_option = \"preserved\"\n[headers]\nAuthorization = \"placeholder extension credential\"\n")
	if err := store.ImportRaw("docs", raw); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "extensions", "docs", "config.enc")
	ciphertext, err := os.ReadFile(path)
	if err != nil || bytes.Contains(ciphertext, []byte("placeholder extension credential")) {
		t.Fatalf("template was not encrypted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "config.toml")); !os.IsNotExist(err) {
		t.Fatal("encrypted save created a plaintext template")
	}
	reopened, err := NewEncryptedStore(root, keys)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.ReadRaw("docs")
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("restart lost raw fields or credentials")
	}
	if names, err := reopened.List(); err != nil || !reflect.DeepEqual(names, []string{"docs"}) {
		t.Fatalf("encrypted template list = %v: %v", names, err)
	}
	other := filepath.Join(root, "extensions", "other", "config.enc")
	if err := os.MkdirAll(filepath.Dir(other), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, ciphertext, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Load("other"); err == nil {
		t.Fatal("a different template accepted substituted ciphertext")
	}
	ciphertext[len(ciphertext)-1] ^= 1
	if err := os.WriteFile(path, ciphertext, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.ReadRaw("docs"); err == nil {
		t.Fatal("a modified ciphertext was accepted")
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "config.toml"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Delete("docs"); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Load("docs"); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted ciphertext fell back to a legacy plaintext template")
	}
}
