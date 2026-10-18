package encryptedfs

import "strings"

// SystemProvider uses libsecret's secret-tool, which talks to the desktop
// Secret Service. Headless systems need ENV or password mode instead.
func SystemProvider() KeyProvider {
	return credentialProvider{name: "secret-service", store: secretServiceStore{}}
}

type secretServiceStore struct{}

func (secretServiceStore) store(reference, secret string) error {
	output, err := credentialCommand("secret-tool", []string{"store", "--label=Termcp encrypted storage", "service", systemService, "slot", reference}, strings.NewReader(secret))
	clear(output)
	return err
}
func (secretServiceStore) load(reference string) (string, error) {
	output, err := credentialCommand("secret-tool", []string{"lookup", "service", systemService, "slot", reference}, nil)
	defer clear(output)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}
