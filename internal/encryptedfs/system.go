package encryptedfs

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"io"
	"os/exec"
	"time"
)

const systemService = "org.termcp.encrypted-storage.v1"

// credentialStore stores a KEK outside the data directory. Entries use random
// slot references, never the local login name, data path or a remote host name.
type credentialStore interface {
	store(reference, secret string) error
	load(reference string) (string, error)
}

type credentialProvider struct {
	name  string
	store credentialStore
}

func (p credentialProvider) Name() string { return p.name }
func (p credentialProvider) NewParameters() (KeyParameters, error) {
	params := KeyParameters{Salt: make([]byte, 16)}
	if _, err := rand.Read(params.Salt); err != nil {
		return KeyParameters{}, err
	}
	key, err := GenerateKey()
	if err != nil {
		return KeyParameters{}, err
	}
	defer clear(key[:])
	if err := p.store.store(hex.EncodeToString(params.Salt), base64.StdEncoding.EncodeToString(key[:])); err != nil {
		return KeyParameters{}, ErrProviderUnavailable
	}
	return params, nil
}
func (p credentialProvider) Key(params KeyParameters) (Key, error) {
	var key Key
	if len(params.Salt) != 16 || params.KDF != (Argon2Parameters{}) || len(params.Protected) != 0 {
		return key, ErrFormat
	}
	secret, err := p.store.load(hex.EncodeToString(params.Salt))
	if err != nil {
		return key, ErrProviderUnavailable
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(secret)
	defer clear(decoded)
	if err != nil || len(decoded) != len(key) {
		return key, ErrProviderUnavailable
	}
	copy(key[:], decoded)
	return key, nil
}

// No system command receives a secret in argv, environment or a temporary file.
// Discard stderr because a system credential tool may include sensitive values.
func credentialCommand(path string, args []string, input io.Reader) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdin, cmd.Stderr = input, io.Discard
	cmd.WaitDelay = time.Second
	return cmd.Output()
}
