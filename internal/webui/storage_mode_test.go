package webui

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
	"github.com/open-mcp-ai/termcp/internal/securestore"
)

func TestStorageUnlockModeAPIAuthenticationAndRestart(t *testing.T) {
	key, err := encryptedfs.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(encryptedfs.EnvStorageKEK, base64.StdEncoding.EncodeToString(key[:]))
	root := t.TempDir()
	options := securestore.Options{PasswordCost: encryptedfs.Argon2Parameters{Time: 1, Memory: 8 * 1024, Threads: 1}}
	activate := func(*encryptedfs.KeyManager) (http.Handler, io.Closer, error) {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "business") }), nil, nil
	}
	verifier, err := auth.NewVerifier("placeholder mode HTTP credential", "")
	if err != nil {
		t.Fatal(err)
	}
	var controller *securestore.Controller
	var handler http.Handler
	open := func() {
		t.Helper()
		var err error
		controller, err = securestore.OpenWithOptions(root, activate, options)
		if err != nil {
			t.Fatal(err)
		}
		current := controller
		t.Cleanup(func() { _ = current.Close() })
		handler = auth.Middleware(verifier, StorageGateway(controller, &Handler{}))
	}
	request := func(path, body, origin, remote string, authenticated bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "http://termcp.invalid"+path, strings.NewReader(body))
		req.RemoteAddr = remote
		req.Header.Set("Content-Type", "application/json")
		if authenticated {
			req.Header.Set("Authorization", "Bearer placeholder mode HTTP credential")
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}
	open()
	waitStorageAPI(t, controller)
	const path = "/api/storage/unlock-mode"
	const manual = `{"unlock_mode":"password","new_password":"placeholder chosen password","confirm_password":"placeholder chosen password"}`
	if rr := request(path, manual, "", "127.0.0.1:10000", false); rr.Code != 401 {
		t.Fatal("unauthenticated mode selection was accepted")
	}
	if rr := request(path, manual, "https://attacker.invalid", "127.0.0.1:10000", true); rr.Code != 403 {
		t.Fatal("cross-origin mode selection was accepted")
	}
	if rr := request(path, manual, "", "198.51.100.1:10000", true); rr.Code != 426 {
		t.Fatal("mode password was accepted over unsafe remote transport")
	}
	if rr := request(path, `{"unlock_mode":"other"}`, "", "127.0.0.1:10000", true); rr.Code != 400 || !strings.Contains(rr.Body.String(), "invalid_unlock_mode") {
		t.Fatal("invalid mode was accepted")
	}
	if rr := request(path, `{"unlock_mode":"password","new_password":"placeholder chosen password","confirm_password":"mismatch"}`, "", "127.0.0.1:10000", true); rr.Code != 400 {
		t.Fatal("first manual password bypassed confirmation")
	}
	if rr := request(path, manual, "", "127.0.0.1:10000", true); rr.Code != 200 || !strings.Contains(rr.Body.String(), `"unlock_mode":"password"`) || strings.Contains(rr.Body.String(), "placeholder chosen password") {
		t.Fatal("manual mode selection failed or echoed its password")
	}
	if controller.Status().State != securestore.Ready || controller.Handler() == nil {
		t.Fatal("mode selection locked the running service")
	}
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	open()
	if controller.Status().State != securestore.Locked {
		t.Fatal("saved manual mode did not block business startup despite ENV")
	}
	if rr := request(path, `{"unlock_mode":"automatic"}`, "", "127.0.0.1:10000", true); rr.Code != 409 {
		t.Fatal("locked storage allowed changing unlock policy")
	}
	if rr := request("/api/storage/unlock", `{"password":"placeholder chosen password"}`, "", "127.0.0.1:10000", true); rr.Code != 202 {
		t.Fatal("saved manual mode could not unlock by password")
	}
	waitStorageAPI(t, controller)
	if controller.Status().AutoUnlock {
		t.Fatal("manual unlock silently enabled ENV hosting")
	}
	if rr := request(path, `{"unlock_mode":"automatic","current_password":"incorrect placeholder"}`, "", "127.0.0.1:10000", true); rr.Code != 403 || !strings.Contains(rr.Body.String(), "invalid_current_password") {
		t.Fatal("switching to automatic mode bypassed current-password proof")
	}
	if rr := request(path, `{"unlock_mode":"automatic","current_password":"placeholder chosen password"}`, "", "127.0.0.1:10000", true); rr.Code != 200 || !strings.Contains(rr.Body.String(), `"auto_unlock":true`) {
		t.Fatal("automatic mode could not be restored")
	}
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	open()
	waitStorageAPI(t, controller)
	if !controller.Status().AutoUnlock || !controller.Status().UnlockModeSaved {
		t.Fatal("saved automatic mode required another password")
	}
}
