package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/config"
	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
	"github.com/open-mcp-ai/termcp/internal/securestore"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/internal/webui"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

func TestApplicationPublishesMigratedHistoryOnlyAfterUnlock(t *testing.T) {
	t.Setenv(encryptedfs.EnvStorageKEK, "")
	root := t.TempDir()
	legacy := storage.New(root)
	t.Cleanup(func() { _ = legacy.Close() })
	if err := legacy.SaveSession(api.Session{ID: "sessionA", Status: api.SessionRunning}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.SaveShell("sessionA", api.Session{ID: "shellA", Index: 3, Status: api.SessionRunning}); err != nil {
		t.Fatal(err)
	}
	want := "placeholder transcript\r\n\x1b[31mfinal line"
	if _, err := legacy.AppendLog("sessionA", "shellA", []byte(want), storage.StdioOut); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DataDir: root, NoInternal: true}
	management := &webui.Handler{}
	server := &http.Server{Addr: "127.0.0.1:18765"}
	options := securestore.Options{PasswordCost: encryptedfs.Argon2Parameters{Time: 1, Memory: 8 * 1024, Threads: 1}}
	open := func() *securestore.Controller {
		c, err := securestore.OpenWithOptions(root, func(keys *encryptedfs.KeyManager) (http.Handler, io.Closer, error) {
			return buildApplication(cfg, keys, server, management)
		}, options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	c := open()
	request := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		webui.StorageGateway(c, management).ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		return rr
	}
	if rr := request("/api/sessions"); rr.Code != 503 {
		t.Fatal("business graph existed before setup")
	}
	password := []byte("placeholder application password")
	if err := c.Initialize(password, password); err != nil {
		t.Fatal(err)
	}
	wait := func() {
		t.Helper()
		deadline := time.NewTimer(20 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for c.Status().State != securestore.Ready {
			if c.Status().State == securestore.Failed {
				t.Fatalf("activation failed: %+v", c.Status())
			}
			select {
			case <-ticker.C:
			case <-deadline.C:
				t.Fatal("activation timed out")
			}
		}
	}
	verify := func() {
		t.Helper()
		rr := request("/api/sessions/sessionA/shells")
		var shells struct {
			Shells []api.Session `json:"shells"`
		}
		if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &shells) != nil || len(shells.Shells) != 1 || shells.Shells[0].Index != 3 {
			t.Fatal("activation lost channel numbering")
		}
		rr = request("/api/shells/shellA/output-range?start=0&max=4096")
		var output struct {
			Data  string `json:"d"`
			Total int64  `json:"total"`
		}
		if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &output) != nil || output.Data != want || output.Total != int64(len(want)) {
			t.Fatal("application history differs from legacy plaintext")
		}
	}
	wait()
	verify()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = open()
	if rr := request("/api/shells/shellA/output-range?start=0"); rr.Code != 503 {
		t.Fatal("restart skipped unlock")
	}
	if err := c.Unlock(password); err != nil {
		t.Fatal(err)
	}
	wait()
	verify()
}

func TestDiagnosticHandlerDoesNotCopySecretsIntoPlaintextLogs(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(diagnosticHandler{slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})})
	logger.With("command", "placeholder private command").WithGroup("operation").LogAttrs(context.Background(), slog.LevelDebug, "operation finished",
		slog.String("preview", "placeholder private output"), slog.String("path", "placeholder private path"),
		slog.Any("err", errors.New("placeholder private error")), slog.String("tool", "shell_output"), slog.Int("bytes", 20))
	text := output.String()
	if strings.Contains(text, "placeholder private") || !strings.Contains(text, `"tool":"shell_output"`) || !strings.Contains(text, `"bytes":20`) || !strings.Contains(text, `"error_present":true`) {
		t.Fatal("diagnostics did not preserve safe metrics and remove private fields")
	}
	if attr, ok := diagnosticAttribute(slog.Any("err", nil)); !ok || attr.Value.Bool() {
		t.Fatal("nil error was reported as a failure")
	}
}
