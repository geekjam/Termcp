//go:build !darwin && !linux && !windows

package encryptedfs

func SystemProvider() KeyProvider { return unavailableSystemProvider{} }

type unavailableSystemProvider struct{}

func (unavailableSystemProvider) Name() string { return "system-unavailable" }
func (unavailableSystemProvider) NewParameters() (KeyParameters, error) {
	return KeyParameters{}, ErrProviderUnavailable
}
func (unavailableSystemProvider) Key(KeyParameters) (Key, error) {
	return Key{}, ErrProviderUnavailable
}
