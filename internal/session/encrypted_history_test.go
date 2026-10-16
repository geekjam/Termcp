package session

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// Exercise the real SSH pipe/output writer, rather than only appending directly
// to storage. The deterministic child avoids platform shell/prompt differences.
func TestEncryptedHistoryIsReadableLiveAndSurvivesJoinedShutdown(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMCP_HISTORY_INPUT_PROCESS", "1")
	password := []byte("placeholder live-history password")
	keys, err := encryptedfs.NewPasswordKeyManager(password, password, encryptedfs.Argon2Parameters{Time: 1, Memory: 8 * 1024, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	store, err := storage.NewEncrypted(root, keys)
	if err != nil {
		t.Fatal(err)
	}
	server := startTestServer(t)
	messages := message.NewManager(store)
	manager := NewManager(messages, store, server)
	joined := false
	t.Cleanup(func() {
		if !joined {
			manager.ShutdownAll()
		}
		_ = store.Close()
	})
	session, err := manager.Create(testConfig(executable, []string{"-test.run=^TestHistoryInputProcess$"}, api.ModePipe, "placeholder encrypted session"))
	if err != nil {
		t.Fatal(err)
	}
	shell := session.PrimaryShell()
	if shell == nil {
		t.Fatal("session has no primary shell")
	}
	waitForHistoryText(t, shell, "HISTORY_PROCESS_READY")
	if err := shell.SendTerminalBytesFrom([]byte("ENCRYPTED_LIVE_MARKER"), false, InputFromAI); err != nil {
		t.Fatal(err)
	}
	if err := shell.PressKeyFrom("enter", 1, InputFromAI); err != nil {
		t.Fatal(err)
	}
	waitForHistoryText(t, shell, "HISTORY_ACK:ENCRYPTED_LIVE_MARKER")
	if session.Info().Status != api.SessionRunning {
		t.Fatal("history was only visible after process exit")
	}
	live, err := store.ReadLog(session.ID, shell.ID, 0, 1<<20)
	if err != nil || !bytes.Contains(live, []byte("HISTORY_ACK:ENCRYPTED_LIVE_MARKER")) {
		t.Fatal("live output did not reach encrypted storage")
	}
	manager.ShutdownAll()
	joined = true
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		if err == nil && bytes.Contains(body, []byte("ENCRYPTED_LIVE_MARKER")) {
			t.Error("live output escaped encryption")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.NewEncrypted(root, keys)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored := NewManager(message.NewManager(reopened), reopened, server)
	defer restored.ShutdownAll()
	if err := restored.RestoreDead(); err != nil {
		t.Fatal(err)
	}
	if restored.Get(session.ID) == nil || restored.Get(session.ID).Info().Status == api.SessionRunning {
		t.Fatal("history did not restore as a DEAD session")
	}
	got, err := reopened.ReadLog(session.ID, shell.ID, 0, 1<<20)
	if err != nil || !bytes.Contains(got, live) {
		t.Fatal("joined shutdown lost previously visible output")
	}
	marks, err := reopened.ReadMarks(session.ID, shell.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundInput := false
	for _, mark := range marks {
		foundInput = foundInput || mark.Status == api.LogAIInput
	}
	if !foundInput {
		t.Fatal("encrypted event log lost input-source marks")
	}
}
