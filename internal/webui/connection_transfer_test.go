package webui

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/configcrypt"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

func TestMultipartImportAcceptsLegacyPasswordExport(t *testing.T) {
	source, target := exportImportEnv(t)
	request := httptest.NewRequest("GET", "/api/connections/batch", nil)
	request.Header.Set(EncryptHeader, "placeholder compatible export")
	export := httptest.NewRecorder()
	source.ServeHTTP(export, request)
	if export.Code != 200 || !configcrypt.IsEncrypted(export.Body.Bytes()) {
		t.Fatal("legacy header export no longer produces its portable format")
	}
	for _, password := range []string{"", "incorrect placeholder"} {
		result := postTransferFile(t, target, export.Body.Bytes(), password, false)
		if result.Code != 400 && result.Code != 403 {
			t.Fatalf("invalid legacy password status=%d", result.Code)
		}
	}
	result := postTransferFile(t, target, export.Body.Bytes(), "placeholder compatible export", false)
	if result.Code != 201 || !strings.Contains(result.Body.String(), `"imported":1`) {
		t.Fatalf("legacy file multipart import status=%d: %s", result.Code, result.Body.String())
	}
}

func postTransferJSON(t *testing.T, mux http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, r)
	return rr
}

func postTransferFile(t *testing.T, mux http.Handler, file []byte, password string, temporary bool) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", "configuration.enc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(file); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteField("password", password); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	path := "/api/connections/batch/import"
	if temporary {
		path += "?temporary=true"
	}
	r := httptest.NewRequest("POST", path, &body)
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("Content-Type", w.FormDataContentType())
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, r)
	return rr
}

func TestConnectionTransferOptionalPasswordAndAuthenticatedImport(t *testing.T) {
	_, source, mux := newConnectionTestHandler(t)
	raw, _ := source.ReadRaw("one")
	if err := source.SaveWithOptions("temporary", raw, true); err != nil {
		t.Fatal(err)
	}
	plain := postTransferJSON(t, mux, "/api/connections/batch/export", `{"names":["one","temporary"]}`)
	if plain.Code != 200 || !strings.Contains(plain.Body.String(), `name = "one"`) || strings.Contains(plain.Body.String(), `name = "temporary"`) || !strings.Contains(plain.Header().Get("Content-Disposition"), `.toml"`) {
		t.Fatal("empty password did not export plaintext persistent profiles")
	}
	protected := postTransferJSON(t, mux, "/api/connections/batch/export", `{"names":["one","temporary"],"password":"placeholder file password","confirm_password":"placeholder file password"}`)
	if protected.Code != 200 || !sshconfig.IsEncryptedBatch(protected.Body.Bytes()) || !strings.Contains(protected.Header().Get("Content-Disposition"), `.toml.enc"`) || protected.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("password did not protect the export")
	}
	target := sshconfig.NewStore(t.TempDir())
	h := &Handler{SSH: target}
	receiver := http.NewServeMux()
	h.Register(receiver)
	for _, password := range []string{"", "incorrect placeholder"} {
		rr := postTransferFile(t, receiver, protected.Body.Bytes(), password, false)
		if rr.Code != 400 && rr.Code != 403 {
			t.Fatalf("missing/wrong password status=%d", rr.Code)
		}
		if names, _ := target.List(); len(names) != 1 {
			t.Fatal("password failure partially imported hosts")
		}
		if strings.Contains(rr.Body.String(), "placeholder") {
			t.Fatal("API echoed the submitted password")
		}
	}
	rr := postTransferFile(t, receiver, protected.Body.Bytes(), "placeholder file password", true)
	if rr.Code != 201 || !target.IsTemporary("one") {
		t.Fatalf("protected import status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr = postTransferFile(t, receiver, plain.Body.Bytes(), "", false); rr.Code != 201 {
		t.Fatalf("plaintext multipart import status=%d", rr.Code)
	}
	if _, err := target.Load("one-2"); err != nil {
		t.Fatal("new import lost collision behavior")
	}
	legacy := httptest.NewRecorder()
	receiver.ServeHTTP(legacy, httptest.NewRequest("POST", "/api/connections/batch", bytes.NewReader(plain.Body.Bytes())))
	if legacy.Code != 201 {
		t.Fatal("legacy plaintext importer stopped working")
	}
}

func TestConnectionTransferRefusesUnsafeOrAmbiguousRequests(t *testing.T) {
	h, _, mux := newConnectionTestHandler(t)
	for _, body := range []string{
		`{"password":"placeholder","confirm_password":"different"}`,
		`{"password":"","confirm_password":"nonempty placeholder"}`,
		`{"names":["internal"]}`,
		`{"names":["one"],"unknown":true}`,
		`{} {}`,
	} {
		if rr := postTransferJSON(t, mux, "/api/connections/batch/export", body); rr.Code != 400 {
			t.Fatalf("invalid export status=%d", rr.Code)
		}
	}
	r := httptest.NewRequest("POST", "/api/connections/batch/export", strings.NewReader(`{"password":"placeholder","confirm_password":"placeholder"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://attacker.invalid")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, r)
	if rr.Code != 403 {
		t.Fatal("cross-origin export accepted")
	}
	r.Header.Del("Origin")
	r.RemoteAddr = "198.51.100.1:12345"
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, r)
	if rr.Code != 426 {
		t.Fatal("remote password submission over plaintext HTTP accepted")
	}
	if rr := postTransferJSON(t, mux, "/api/connections/batch/export?password=placeholder", `{}`); rr.Code != 400 {
		t.Fatal("password in URL accepted")
	}
	h.configTransferMu.Lock()
	rr = postTransferJSON(t, mux, "/api/connections/batch/export", `{}`)
	h.configTransferMu.Unlock()
	if rr.Code != 429 || rr.Header().Get("Retry-After") == "" {
		t.Fatal("concurrent expensive transfer was not bounded")
	}
	var response map[string]string
	if json.Unmarshal(rr.Body.Bytes(), &response) != nil {
		t.Fatal("error is not machine readable")
	}
	// Duplicate password/file fields must not depend on multipart field ordering.
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for i := 0; i < 2; i++ {
		_ = form.WriteField("password", "placeholder")
	}
	part, _ := form.CreateFormFile("file", "configuration.toml")
	_, _ = io.WriteString(part, "connections = []\n")
	_ = form.Close()
	r = httptest.NewRequest("POST", "/api/connections/batch/import", &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, r)
	if rr.Code != 400 {
		t.Fatal("duplicate multipart fields accepted")
	}
}

func TestConnectionImportStorageFailureIsNotReportedAsABadPassword(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ssh_configs"), []byte("placeholder I/O obstruction"), 0600); err != nil {
		t.Fatal(err)
	}
	h := &Handler{SSH: sshconfig.NewStore(root)}
	mux := http.NewServeMux()
	h.Register(mux)
	raw := []byte("[[connections]]\nname = \"one\"\nkind = \"remote\"\nhost = \"example.invalid\"\nuser = \"placeholder\"\npassword = \"placeholder\"\n")
	rr := postTransferFile(t, mux, raw, "", false)
	if rr.Code != 500 || !strings.Contains(rr.Body.String(), "connection_import_failed") || strings.Contains(rr.Body.String(), root) {
		t.Fatalf("I/O failure status=%d body=%s", rr.Code, rr.Body.String())
	}
}
