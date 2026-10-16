package sshconfig

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
)

func TestCommittedEncryptedProfilesCannotDisappearAsAnEmptyStore(t *testing.T) {
	root := t.TempDir()
	store, keys := newEncryptedProfiles(t, root)
	if err := store.EnsureEncryptedDocument(); err != nil {
		t.Fatal(err)
	}
	// The storage controller authenticates this control record before activating
	// profiles. This test only needs its presence to exercise missing-file policy.
	if err := os.WriteFile(filepath.Join(root, "store.enc"), []byte("placeholder committed control"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEncryptedStore(root, keys); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "config", "hosts.enc")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEncryptedStore(root, keys); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing committed configuration became an empty profile list")
	}
}

func newEncryptedProfiles(t *testing.T, root string) (*Store, *encryptedfs.KeyManager) {
	t.Helper()
	key, err := encryptedfs.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMCP_TEST_PROFILE_KEK", base64.StdEncoding.EncodeToString(key[:]))
	keys, err := encryptedfs.NewKeyManager(encryptedfs.EnvProvider{Variable: "TERMCP_TEST_PROFILE_KEK"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewEncryptedStore(root, keys)
	if err != nil {
		t.Fatal(err)
	}
	return store, keys
}

func TestEncryptedProfilesRoundTripTemporaryModesAndExport(t *testing.T) {
	root := t.TempDir()
	store, keys := newEncryptedProfiles(t, root)
	permanent := []byte("kind = \"remote\"\nhost = \"example.invalid\"\nuser = \"placeholder\"\npassword = \"private persistent marker\"\n")
	temporary := bytes.ReplaceAll(permanent, []byte("private persistent marker"), []byte("private temporary marker"))
	if err := store.SaveWithOptions("temporary", temporary, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "config")); !os.IsNotExist(err) {
		t.Fatal("a temporary host caused persistent configuration writes")
	}
	if err := store.Save("saved", permanent); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("internal", []byte("kind = \"internal\"\ndescription = \"custom fixture override\"\n")); err != nil {
		t.Fatal(err)
	}
	for _, names := range [][]string{nil, {"temporary"}, {"saved", "temporary"}} {
		data, err := store.ExportBatch(names)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("private temporary marker")) {
			t.Fatal("temporary credentials were exported")
		}
	}
	path := filepath.Join(root, "config", "hosts.enc")
	raw, err := os.ReadFile(path)
	if err != nil || bytes.Contains(raw, []byte("private persistent marker")) || bytes.Contains(raw, []byte("private temporary marker")) {
		t.Fatal("plaintext credentials reached hosts.enc")
	}
	reopened, err := NewEncryptedStore(root, keys)
	if err != nil {
		t.Fatal(err)
	}
	if names, err := reopened.List(); err != nil || !reflect.DeepEqual(names, []string{"internal", "saved"}) {
		t.Fatalf("temporary host survived restart: %v", err)
	}
	if data, err := reopened.ReadRaw("saved"); err != nil || !bytes.Equal(data, permanent) {
		t.Fatal("raw profile bytes changed")
	}
	if err := store.Rename("saved", "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWithOptions("renamed", temporary, true); err != nil {
		t.Fatal(err)
	}
	if data, err := store.ExportBatch([]string{"renamed"}); err != nil || bytes.Contains(data, []byte("marker")) {
		t.Fatal("a persistent-to-temporary transition remained exportable")
	}
	reopened, err = NewEncryptedStore(root, keys)
	if err != nil {
		t.Fatal(err)
	}
	if names, err := reopened.List(); err != nil || !reflect.DeepEqual(names, []string{"internal"}) {
		t.Fatal("temporary transition retained a persistent copy")
	}
	if _, err := os.Stat(filepath.Join(root, "ssh_configs")); !os.IsNotExist(err) {
		t.Fatal("encrypted store wrote a legacy configuration directory")
	}
}

func TestEncryptedBatchImportAndFailedSavePreserveNamespace(t *testing.T) {
	root := t.TempDir()
	store, _ := newEncryptedProfiles(t, root)
	raw := []byte("kind = \"remote\"\nhost = \"example.invalid\"\nuser = \"placeholder\"\npassword = \"private profile marker\"\n")
	if err := store.Save("saved", raw); err != nil {
		t.Fatal(err)
	}
	export, err := store.ExportBatch(nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.ImportBatch(export, false)
	if err != nil || result.Imported != 1 || len(result.Renamed) != 1 || result.Renamed[0].To != "saved-2" {
		t.Fatalf("encrypted import collision handling changed: %v", err)
	}
	config := filepath.Join(root, "config")
	old := filepath.Join(root, "previous-config")
	if err := os.Rename(config, old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("blocking fixture file"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWithOptions("saved", raw, true); err == nil {
		t.Fatal("failed save acknowledged a storage mode change")
	}
	if store.IsTemporary("saved") {
		t.Fatal("failed persistence changed the in-memory storage mode")
	}
	if data, err := store.ReadRaw("saved"); err != nil || !bytes.Equal(data, raw) {
		t.Fatal("failed save lost the usable profile")
	}
	if err := store.Rename("saved", "other"); err == nil {
		t.Fatal("failed rename acknowledged success")
	}
	if names, err := store.List(); err != nil || !reflect.DeepEqual(names, []string{"internal", "saved", "saved-2"}) {
		t.Fatal("failed rename modified the namespace")
	}
}
