package encryptedfs

import "strings"

// SystemProvider uses the login Keychain. security's interactive command is
// supplied through stdin; the process arguments contain no KEK. The OS may ask
// the user to permit access to the login Keychain.
func SystemProvider() KeyProvider {
	return credentialProvider{name: "keychain", store: keychainStore{}}
}

type keychainStore struct{}

func (keychainStore) store(reference, secret string) error {
	// All values are fixed identifiers, hex references or Base64, so none can
	// contain command separators or quoting characters in security's grammar.
	command := "add-generic-password -s " + systemService + " -a " + reference + " -w " + secret + "\n"
	output, err := credentialCommand("/usr/bin/security", []string{"-i"}, strings.NewReader(command))
	clear(output)
	if err != nil {
		return err
	}
	// Interactive security can exit successfully after a failed subcommand.
	// Read back the item before allowing the envelope to reference it.
	got, err := (keychainStore{}).load(reference)
	if err != nil || got != secret {
		return ErrProviderUnavailable
	}
	return nil
}
func (keychainStore) load(reference string) (string, error) {
	output, err := credentialCommand("/usr/bin/security", []string{"find-generic-password", "-s", systemService, "-a", reference, "-w"}, nil)
	defer clear(output)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}
