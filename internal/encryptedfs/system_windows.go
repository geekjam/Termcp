package encryptedfs

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// SystemProvider binds a KEK to the current Windows account with DPAPI.
// Protected contains only DPAPI ciphertext, never the KEK itself.
func SystemProvider() KeyProvider { return dpapiProvider{} }

type dpapiProvider struct{}

func (dpapiProvider) Name() string { return "dpapi" }
func (dpapiProvider) NewParameters() (KeyParameters, error) {
	key, err := GenerateKey()
	if err != nil {
		return KeyParameters{}, err
	}
	defer clear(key[:])
	input := windows.DataBlob{Size: uint32(len(key)), Data: &key[0]}
	var output windows.DataBlob
	if err := windows.CryptProtectData(&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return KeyParameters{}, ErrProviderUnavailable
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
	return KeyParameters{Protected: append([]byte(nil), unsafe.Slice(output.Data, output.Size)...)}, nil
}
func (dpapiProvider) Key(params KeyParameters) (Key, error) {
	var key Key
	if len(params.Salt) != 0 || params.KDF != (Argon2Parameters{}) || len(params.Protected) == 0 || len(params.Protected) > 16<<10 {
		return key, ErrFormat
	}
	input := windows.DataBlob{Size: uint32(len(params.Protected)), Data: &params.Protected[0]}
	var output windows.DataBlob
	if err := windows.CryptUnprotectData(&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return key, ErrProviderUnavailable
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
	decoded := unsafe.Slice(output.Data, output.Size)
	defer clear(decoded)
	if len(decoded) != len(key) {
		return key, ErrProviderUnavailable
	}
	copy(key[:], decoded)
	return key, nil
}
