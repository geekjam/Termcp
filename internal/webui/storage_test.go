package webui

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
	"github.com/open-mcp-ai/termcp/internal/securestore"
)

func TestStoragePasswordTransportRequiresTLSOrTrustedProxy(t *testing.T) {
	t.Setenv("TERMCP_STORAGE_TRUSTED_PROXIES", "192.0.2.0/24,2001:db8::/64,invalid")
	for _, tc := range []struct {
		address, forwarded string
		tls, want          bool
	}{
		{"127.0.0.1:10000", "", false, true},
		{"[::1]:10000", "", false, true},
		{"198.51.100.1:10000", "", false, false},
		{"198.51.100.1:10000", "https", false, false},
		{"192.0.2.1:10000", "http", false, false},
		{"192.0.2.1:10000", "https", false, true},
		{"[2001:db8::1]:10000", "https", false, true},
		{"198.51.100.1:10000", "", true, true},
	} {
		r := httptest.NewRequest("POST", "http://termcp.invalid/api/storage/password", nil)
		r.RemoteAddr = tc.address
		r.Header.Set("X-Forwarded-Proto", tc.forwarded)
		if tc.tls {
			r.TLS = &tls.ConnectionState{}
		}
		if storageTransportAllowed(r) != tc.want {
			t.Errorf("address=%s forwarded=%s TLS=%v", tc.address, tc.forwarded, tc.tls)
		}
	}
}

func waitStorageAPI(t *testing.T, controller *securestore.Controller) {
	t.Helper()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		state := controller.Status()
		if state.State == securestore.Ready {
			return
		}
		if state.State == securestore.Failed {
			t.Fatalf("storage failed: %s", state.ErrorCode)
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("storage timeout: %+v", state)
		}
	}
}

func TestStorageAPIAuthBootstrapAndPasswordRotation(t *testing.T) {
	t.Setenv(encryptedfs.EnvStorageKEK, "")
	root := t.TempDir()
	options := securestore.Options{PasswordCost: encryptedfs.Argon2Parameters{Time: 1, Memory: 8 * 1024, Threads: 1}}
	activate := func(*encryptedfs.KeyManager) (http.Handler, io.Closer, error) {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "business") }), io.NopCloser(strings.NewReader("")), nil
	}
	controller, err := securestore.OpenWithOptions(root, activate, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	verifier, err := auth.NewVerifier("placeholder HTTP credential", "")
	if err != nil {
		t.Fatal(err)
	}
	handler := auth.Middleware(verifier, StorageGateway(controller, &Handler{}))
	request := func(method, path, body string, authenticated bool, origin string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "http://termcp.invalid"+path, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:34567"
		if authenticated {
			req.Header.Set("Authorization", "Bearer placeholder HTTP credential")
		}
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}
	if rr := request("GET", "/api/storage/status", "", false, ""); rr.Code != 401 {
		t.Fatalf("unauthenticated state=%d", rr.Code)
	}
	if rr := request("GET", "/api/sessions", "", true, ""); rr.Code != 503 || !strings.Contains(rr.Body.String(), "UNINITIALIZED") {
		t.Fatal("business was exposed before unlock")
	}
	if rr := request("GET", "/api.md", "", false, ""); rr.Code != 200 {
		t.Fatal("bootstrap docs require a storage password")
	}
	if rr := request("GET", "/", "", true, ""); uiEmbedded && (rr.Code != 303 || rr.Header().Get("Location") != "storage.html") {
		t.Fatal("browser was not directed to setup")
	}
	initial := `{"new_password":"placeholder master password","confirm_password":"placeholder master password"}`
	if rr := request("POST", "/api/storage/initialize", initial, true, "https://attacker.invalid"); rr.Code != 403 {
		t.Fatal("cross-origin setup was accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "keys", "master.key")); !os.IsNotExist(err) {
		t.Fatal("cross-origin request initialized storage")
	}
	if rr := request("POST", "/api/storage/initialize", `{"new_password":"placeholder master password","confirm_password":"different placeholder"}`, true, ""); rr.Code != 400 || !strings.Contains(rr.Body.String(), "password_confirmation_mismatch") {
		t.Fatal("backend omitted initial password confirmation")
	}
	if rr := request("POST", "/api/storage/initialize", initial, true, "http://termcp.invalid"); rr.Code != 202 {
		t.Fatalf("initialize=%d %s", rr.Code, rr.Body.String())
	}
	waitStorageAPI(t, controller)
	if rr := request("GET", "/api/sessions", "", true, ""); rr.Body.String() != "business" {
		t.Fatal("ready handler was not published")
	}
	if rr := request("POST", "/api/storage/initialize", initial, true, ""); rr.Code != 409 {
		t.Fatal("setup overwrote initialized storage")
	}
	if rr := request("POST", "/api/storage/password", `{"current_password":"wrong placeholder","new_password":"replacement placeholder","confirm_password":"replacement placeholder"}`, true, ""); rr.Code != 403 || !strings.Contains(rr.Body.String(), "invalid_current_password") {
		t.Fatal("unlocked store skipped current password proof")
	}
	if rr := request("POST", "/api/storage/password", `{"current_password":"placeholder master password","new_password":"replacement placeholder","confirm_password":"different placeholder"}`, true, ""); rr.Code != 400 {
		t.Fatal("backend trusted frontend confirmation")
	}
	if rr := request("POST", "/api/storage/password", `{"current_password":"placeholder master password","new_password":"replacement placeholder","confirm_password":"replacement placeholder"}`, true, ""); rr.Code != 200 {
		t.Fatalf("password change=%d %s", rr.Code, rr.Body.String())
	}
	if rr := request("POST", "/api/storage/password", `{"current_password":"replacement placeholder","new_password":"another placeholder","confirm_password":"another placeholder"}`, true, ""); rr.Code != 429 || rr.Header().Get("Retry-After") == "" {
		t.Fatal("password attempts were not rate limited")
	}
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	controller, err = securestore.OpenWithOptions(root, activate, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	handler = auth.Middleware(verifier, StorageGateway(controller, &Handler{}))
	if rr := request("GET", "/api/sessions", "", true, ""); rr.Code != 503 {
		t.Fatal("restart bypassed the storage password")
	}
	if rr := request("POST", "/api/storage/unlock", `{"password":"placeholder master password"}`, true, ""); rr.Code != 403 {
		t.Fatal("old password remained valid")
	}
	if rr := request("POST", "/api/storage/unlock", `{"password":"replacement placeholder"}`, true, ""); rr.Code != 202 {
		t.Fatalf("unlock=%d %s", rr.Code, rr.Body.String())
	}
	waitStorageAPI(t, controller)
}

func TestStorageAPIBoundsAndNoPasswordEcho(t *testing.T) {
	t.Setenv(encryptedfs.EnvStorageKEK, "")
	controller, err := securestore.OpenWithOptions(t.TempDir(), nil, securestore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	handler := StorageGateway(controller, &Handler{})
	for _, tc := range []struct {
		body, content string
		want          int
	}{
		{`{"new_password":"private placeholder","confirm_password":"different"}`, "text/plain", 415},
		{`{"new_password":"private placeholder","confirm_password":"different","unknown":true}`, "application/json", 400},
		{`{} {}`, "application/json", 400},
		{strings.Repeat(" ", 17<<10) + `{}`, "application/json", 400},
		{`{"new_password":"private placeholder","confirm_password":"different"}`, "application/json", 400},
	} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "http://termcp.invalid/api/storage/initialize", bytes.NewBufferString(tc.body))
		req.RemoteAddr = "127.0.0.1:34567"
		req.Header.Set("Content-Type", tc.content)
		handler.ServeHTTP(rr, req)
		if rr.Code != tc.want {
			t.Fatalf("request status=%d, want=%d", rr.Code, tc.want)
		}
		if strings.Contains(rr.Body.String(), "private placeholder") {
			t.Fatal("password appeared in API response")
		}
		var result map[string]any
		if json.Unmarshal(rr.Body.Bytes(), &result) != nil || result["error"] == nil {
			t.Fatal("invalid operation did not return a structured error")
		}
	}
}
