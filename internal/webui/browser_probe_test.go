package webui

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Chrome's virtual-time dump can advance timers without drawing another frame.
// Use its DevTools protocol with real time, and wait for the page's result. The
// isolated profile and browser lifetime belong to this probe on every OS.
func renderedProbeDOM(t *testing.T, chrome, path string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	profile := t.TempDir()
	cmd := exec.CommandContext(ctx, chrome, "--headless=new", "--disable-gpu", "--no-sandbox",
		"--no-first-run", "--no-default-browser-check", "--remote-debugging-port=0",
		"--user-data-dir="+profile, "about:blank")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()
	var conn *websocket.Conn
	defer func() {
		if conn != nil {
			_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
			_ = conn.WriteJSON(map[string]any{"id": 999, "method": "Browser.close"})
			_ = conn.Close()
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			cancel()
			<-done
		}
	}()
	// The process is joined before returning, ahead of TempDir's profile cleanup.
	var endpoint string
	for endpoint == "" {
		data, err := os.ReadFile(filepath.Join(profile, "DevToolsActivePort"))
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if err == nil && len(lines) == 2 {
			endpoint = "ws://127.0.0.1:" + strings.TrimSpace(lines[0]) + strings.TrimSpace(lines[1])
			break
		}
		select {
		case <-done:
			t.Fatalf("headless browser exited before its debugger started: %v", waitErr)
		case <-ctx.Done():
			t.Fatal("headless browser debugger did not start")
		case <-time.After(20 * time.Millisecond):
		}
	}
	var err error
	conn, _, err = websocket.DefaultDialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline, _ := ctx.Deadline()
	_ = conn.SetReadDeadline(deadline)
	_ = conn.SetWriteDeadline(deadline)
	var id int
	var sessionID string
	loaded := make(map[string]bool)
	read := func() (int, json.RawMessage) {
		var reply struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Params struct {
				Name     string `json:"name"`
				LoaderID string `json:"loaderId"`
			} `json:"params"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := conn.ReadJSON(&reply); err != nil {
			t.Fatalf("headless browser protocol: %v", err)
		}
		if reply.Error != nil {
			t.Fatalf("headless browser protocol: %s", reply.Error.Message)
		}
		if reply.Method == "Page.lifecycleEvent" && reply.Params.Name == "load" {
			loaded[reply.Params.LoaderID] = true
		}
		return reply.ID, reply.Result
	}
	call := func(method string, params any) json.RawMessage {
		id++
		request := map[string]any{"id": id, "method": method, "params": params}
		if sessionID != "" {
			request["sessionId"] = sessionID
		}
		if err := conn.WriteJSON(request); err != nil {
			t.Fatal(err)
		}
		for {
			got, result := read()
			if got == id {
				return result
			}
		}
	}
	var target struct{ TargetID string }
	if err := json.Unmarshal(call("Target.createTarget", map[string]any{"url": "about:blank"}), &target); err != nil {
		t.Fatal(err)
	}
	var attachment struct{ SessionID string }
	if err := json.Unmarshal(call("Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}), &attachment); err != nil {
		t.Fatal(err)
	}
	sessionID = attachment.SessionID
	call("Page.enable", map[string]any{})
	call("Page.setLifecycleEventsEnabled", map[string]any{"enabled": true})
	var navigation struct {
		LoaderID  string `json:"loaderId"`
		ErrorText string `json:"errorText"`
	}
	if err := json.Unmarshal(call("Page.navigate", map[string]any{"url": "file:///" + filepath.ToSlash(path)}), &navigation); err != nil || navigation.ErrorText != "" {
		t.Fatalf("headless browser navigation failed: %s (%v)", navigation.ErrorText, err)
	}
	for !loaded[navigation.LoaderID] {
		read()
	}
	result := call("Runtime.evaluate", map[string]any{
		"expression": `new Promise(resolve => {
  function finish() {
    if (!document.title.startsWith('RESULT::')) return;
    observer.disconnect();
    resolve(document.documentElement.outerHTML);
  }
  const observer = new MutationObserver(finish);
  observer.observe(document.documentElement, {subtree: true, childList: true, characterData: true});
  finish();
})`,
		"awaitPromise": true, "returnByValue": true,
	})
	var dom struct {
		Result           struct{ Value string }
		ExceptionDetails json.RawMessage
	}
	if err := json.Unmarshal(result, &dom); err != nil || len(dom.ExceptionDetails) != 0 || dom.Result.Value == "" {
		t.Fatalf("headless browser probe failed: %s (%v)", result, err)
	}
	return dom.Result.Value
}
