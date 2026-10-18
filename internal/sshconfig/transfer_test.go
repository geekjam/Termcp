package sshconfig

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
)

func TestProtectedBatchIsPortableAndNeverExportsTemporaryProfiles(t *testing.T) {
	source, _ := newEncryptedProfiles(t, t.TempDir())
	if _, err := source.ImportBatch([]byte(batchFixture), false); err != nil {
		t.Fatal(err)
	}
	raw, _ := source.ReadRaw("alpha")
	if err := source.SaveWithOptions("temporary", raw, true); err != nil {
		t.Fatal(err)
	}
	want, err := source.ExportBatch([]string{"alpha", "temporary"})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := source.ExportBatchWithPassword([]string{"alpha", "temporary"}, nil)
	if err != nil || !bytes.Equal(plain, want) {
		t.Fatal("empty export password changed plaintext compatibility")
	}
	password := []byte("placeholder independent export password 🔑")
	protected, err := source.ExportBatchWithPassword([]string{"alpha", "temporary"}, password)
	if err != nil || !IsEncryptedBatch(protected) {
		t.Fatalf("protected export: %v", err)
	}
	if bytes.Contains(protected, []byte("alpha.example")) || bytes.Contains(protected, []byte("connections")) {
		t.Fatal("protected file contains plaintext configuration")
	}
	decoded, err := DecryptBatch(protected, password)
	if err != nil || !bytes.Equal(decoded, want) || bytes.Contains(decoded, []byte(`name = "temporary"`)) {
		t.Fatal("encrypted export changed selection or exported a temporary profile")
	}
	target, _ := newEncryptedProfiles(t, t.TempDir()) // independent local data key
	result, err := target.ImportBatchWithPassword(protected, password, false)
	if err != nil || result.Imported != 1 {
		t.Fatalf("portable import: %v", err)
	}
	got, err := target.Load("alpha")
	expected, _ := source.Load("alpha")
	if err != nil || !reflect.DeepEqual(got, expected) {
		t.Fatal("cross-store import changed the profile")
	}
	result, err = target.ImportBatchWithPassword(protected, password, true)
	if err != nil || result.Renamed[0].To != "alpha-2" || !target.IsTemporary("alpha-2") {
		t.Fatal("encrypted import lost collision/temporary semantics")
	}
}

func TestProtectedBatchAuthenticationRejectsBeforeImport(t *testing.T) {
	password := []byte("placeholder export password")
	cost := encryptedfs.Argon2Parameters{Time: 1, Memory: 8 * 1024, Threads: 1}
	protected, err := encryptBatch([]byte(batchFixture), password, cost)
	if err != nil {
		t.Fatal(err)
	}
	other, err := encryptBatch([]byte(batchFixture), password, cost)
	if err != nil || bytes.Equal(protected, other) {
		t.Fatal("protected exports reused their salt/nonce")
	}
	for _, tc := range []struct {
		name           string
		data, password []byte
		want           error
	}{
		{"missing password", protected, nil, ErrTransferPasswordRequired},
		{"wrong password", protected, []byte("wrong placeholder"), ErrTransferAuthentication},
		{"plaintext with password", []byte(batchFixture), password, ErrTransferUnexpectedPassword},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore(t.TempDir())
			if _, err := s.ImportBatchWithPassword(tc.data, tc.password, false); !errors.Is(err, tc.want) {
				t.Fatalf("got %v", err)
			}
			if names, _ := s.List(); !reflect.DeepEqual(names, []string{"internal"}) {
				t.Fatal("rejected file partially imported profiles")
			}
		})
	}
	for _, position := range []int{24, 40, len(protected) - 1} {
		changed := bytes.Clone(protected)
		changed[position] ^= 1
		if _, err := DecryptBatch(changed, password); !errors.Is(err, ErrTransferAuthentication) {
			t.Fatal("tampered salt/nonce/ciphertext was accepted")
		}
	}
	for _, n := range []int{8, 71, len(protected) - 1} {
		if _, err := DecryptBatch(protected[:n], password); !errors.Is(err, ErrTransferFormat) {
			t.Fatal("truncated export was accepted")
		}
	}
	for _, position := range []int{8, 10, 12, 16, 20, 21, 64} {
		changed := bytes.Clone(protected)
		if position == 12 || position == 16 {
			binary.LittleEndian.PutUint32(changed[position:], ^uint32(0))
		} else {
			changed[position] ^= 0x80
		}
		if _, err := DecryptBatch(changed, password); !errors.Is(err, ErrTransferFormat) {
			t.Fatal("unbounded KDF or malformed header was accepted")
		}
	}
}
