package session

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// TestHistoryInputProcess is run in a child process by the test below. Unlike a
// shell, it only reads lines and replies to them, so every input it receives is
// unambiguously input to an interactive process.
func TestHistoryInputProcess(t *testing.T) {
	if os.Getenv("TERMCP_HISTORY_INPUT_PROCESS") != "1" {
		return
	}
	fmt.Println("HISTORY_PROCESS_READY")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		fmt.Println("HISTORY_ACK:" + scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

func waitForHistoryText(t *testing.T, cs *ChildShell, want string) {
	waitForHistoryTextCount(t, cs, want, 1)
}

func waitForHistoryTextCount(t *testing.T, cs *ChildShell, want string, count int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last []byte
	for time.Now().Before(deadline) {
		out, _, err := cs.OutputByteRange(0, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(out), want) >= count {
			return
		}
		last = out
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d copies of %q in process output; last output: %q", count, want, last)
}

func waitForHistoryShellReady(t *testing.T, cs *ChildShell) {
	t.Helper()
	command := "printf 'HISTORY_SHELL_%s\\n' READY"
	if runtime.GOOS == "windows" {
		command = "Write-Output ('HISTORY_SHELL_' + 'READY')"
	}
	deadline := time.Now().Add(30 * time.Second)
	var last []byte
	for time.Now().Before(deadline) {
		if err := cs.SendTerminalBytes([]byte(testShellInput(command)), false); err != nil {
			t.Fatal(err)
		}
		probeDeadline := time.Now().Add(time.Second)
		for time.Now().Before(probeDeadline) && time.Now().Before(deadline) {
			out, _, err := cs.OutputByteRange(0, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			// Only executed output contains the full marker; terminal redraws
			// and echoed input cannot satisfy the readiness probe.
			if strings.Contains(string(out), "HISTORY_SHELL_READY") {
				return
			}
			last = out
			time.Sleep(20 * time.Millisecond)
		}
	}
	t.Fatalf("shell did not execute readiness probe; last output: %q", last)
}

func historyInputMarks(t *testing.T, mm *message.Manager, sessionID, shellID string) []api.LogStatus {
	t.Helper()
	var got []api.LogStatus
	for _, sp := range spans(t, mm, sessionID, shellID) {
		if sp.status != api.LogAPIInput && sp.status != api.LogAIInput {
			continue
		}
		if sp.bytes() != 0 {
			t.Fatalf("input mark covers %d output bytes: %+v", sp.bytes(), sp)
		}
		got = append(got, sp.status)
	}
	return got
}

// Input-source history must stay correct when stdin belongs to an interactive
// process. The current i/a marks identify the writer, not a shell command; the
// process case makes that distinction observable without parsing a prompt.
func TestHistoryInputModesInInteractiveProcess(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMCP_HISTORY_INPUT_PROCESS", "1")
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	mm := message.NewManager(store)
	mgr := NewManager(mm, store, srv)
	t.Cleanup(func() {
		for _, info := range mgr.ListAll() {
			_ = mgr.Delete(info.ID)
		}
	})

	s, err := mgr.Create(testConfig(executable, []string{"-test.run=TestHistoryInputProcess"}, api.ModePipe, ""))
	if err != nil {
		t.Fatal(err)
	}
	cs := s.PrimaryShell()
	if cs == nil {
		t.Fatal("session has no primary shell")
	}
	waitForHistoryText(t, cs, "HISTORY_PROCESS_READY")

	if err := cs.SendTerminalBytes([]byte("human-partial"), false); err != nil {
		t.Fatal(err)
	}
	if got := historyInputMarks(t, mm, s.ID, cs.ID); len(got) != 0 {
		t.Fatalf("partial process input created history marks: %v", got)
	}
	if err := cs.PressKey("enter", 1); err != nil {
		t.Fatal(err)
	}
	waitForHistoryText(t, cs, "HISTORY_ACK:human-partial")
	if got := historyInputMarks(t, mm, s.ID, cs.ID); len(got) != 1 || got[0] != api.LogAPIInput {
		t.Fatalf("human process input marks = %v, want [i]", got)
	}

	if err := cs.SendTerminalBytesFrom([]byte("agent-input"), false, InputFromAI); err != nil {
		t.Fatal(err)
	}
	if err := cs.PressKeyFrom("enter", 1, InputFromAI); err != nil {
		t.Fatal(err)
	}
	waitForHistoryText(t, cs, "HISTORY_ACK:agent-input")
	if got := historyInputMarks(t, mm, s.ID, cs.ID); len(got) != 2 || got[0] != api.LogAPIInput || got[1] != api.LogAIInput {
		t.Fatalf("mixed process input marks = %v, want [i a]", got)
	}

	// One write containing two complete lines currently creates one mark. This
	// characterizes the existing per-write history; it must not be mistaken for
	// a count of accepted commands or process responses.
	if err := cs.SendTerminalBytes([]byte("pasted-one\npasted-two\n"), false); err != nil {
		t.Fatal(err)
	}
	waitForHistoryText(t, cs, "HISTORY_ACK:pasted-two")
	if got := historyInputMarks(t, mm, s.ID, cs.ID); len(got) != 3 || got[2] != api.LogAPIInput {
		t.Fatalf("multi-line process paste marks = %v, want [i a i]", got)
	}
	marks := spans(t, mm, s.ID, cs.ID)
	want := []api.LogStatus{api.LogOutput, api.LogAPIInput, api.LogOutput, api.LogAIInput, api.LogOutput, api.LogAPIInput, api.LogOutput}
	if len(marks) != len(want) {
		t.Fatalf("history span count = %d, want %d: %+v", len(marks), len(want), marks)
	}
	var cursor int64
	for i, sp := range marks {
		if sp.status != want[i] || sp.start != cursor {
			t.Fatalf("history span %d = %+v, want status %q at offset %d", i, sp, want[i], cursor)
		}
		cursor = sp.end
	}
	total, err := mm.OutputSize(s.ID, cs.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != total {
		t.Fatalf("history spans end at %d, log ends at %d", cursor, total)
	}
}

// A continuation prompt is still the shell editing one command. This test
// records the present limitation: Enter already writes i before syntax is
// complete, so i must not be presented as "one complete shell command".
func TestHistoryShellContinuationIsOnlyALineMark(t *testing.T) {
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	mm := message.NewManager(store)
	mgr := NewManager(mm, store, srv)
	t.Cleanup(func() {
		for _, info := range mgr.ListAll() {
			_ = mgr.Delete(info.ID)
		}
	})
	args := testInteractiveShellArgs()
	if runtime.GOOS == "windows" {
		// Exercise PowerShell's multiline parser with a plain console reader.
		// PSReadLine redraws and optional terminal protocols are independent of
		// the input-history mark this test checks.
		startup := "Remove-Module PSReadLine -ErrorAction SilentlyContinue; " +
			"function global:PSConsoleHostReadLine { [Console]::ReadLine() }; " +
			"function global:prompt { 'history> ' }"
		args = append(args, "-NoExit", "-Command", startup)
	}
	s, err := mgr.Create(testConfig(testShell(), args, api.ModePTY, ""))
	if err != nil {
		t.Fatal(err)
	}
	cs := s.PrimaryShell()
	if cs == nil {
		t.Fatal("session has no primary shell")
	}
	// ConPTY emits terminal setup bytes before PowerShell reads input. A
	// temporarily idle log alone does not establish a live command reader.
	waitForHistoryShellReady(t, cs)
	waitForIdleBytes(t, mm, s.ID, cs.ID)
	before := len(historyInputMarks(t, mm, s.ID, cs.ID))

	first, finish := "if true; then", "printf 'HISTORY_CONTINUATION_%s\\n' DONE\nfi"
	if runtime.GOOS == "windows" {
		first, finish = "if ($true) {", "Write-Output ('HISTORY_CONTINUATION_' + 'DONE')\n}"
	}
	if err := cs.SendTerminalBytes([]byte(testShellInput(first)), false); err != nil {
		t.Fatal(err)
	}
	// Wait for the prompt and echo reader to finish before deriving spans. The
	// byte log is appended before its output status mark, so a concurrent read
	// can momentarily see bytes under the preceding input mark.
	waitForIdleBytes(t, mm, s.ID, cs.ID)
	if got := historyInputMarks(t, mm, s.ID, cs.ID); len(got) != before+1 || got[before] != api.LogAPIInput {
		t.Fatalf("continuation line marks = %v, want one i after %d readiness marks", got, before)
	}
	// The completion token is sent only after the first assertion. The shell
	// cannot have run that command at the time the first i mark was written.
	for _, line := range strings.Split(finish, "\n") {
		if err := cs.SendTerminalBytes([]byte(testShellInput(line)), false); err != nil {
			t.Fatal(err)
		}
	}
	if runtime.GOOS == "windows" {
		// PowerShell's plain console reader submits a multiline block on an
		// empty line after the closing brace.
		if err := cs.PressKey("enter", 1); err != nil {
			t.Fatal(err)
		}
	}
	// The marker is assembled by execution, independent of PSReadLine redraws.
	waitForHistoryText(t, cs, "HISTORY_CONTINUATION_DONE")
}

// Ctrl+C abandons a partially typed line. The input-side history still records
// i because the line ended, but no command result may be inferred from it.
func TestHistoryInterruptedLineIsNotAnExecutedCommand(t *testing.T) {
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	mm := message.NewManager(store)
	mgr := NewManager(mm, store, srv)
	t.Cleanup(func() {
		for _, info := range mgr.ListAll() {
			_ = mgr.Delete(info.ID)
		}
	})
	s, err := mgr.Create(testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, ""))
	if err != nil {
		t.Fatal(err)
	}
	cs := s.PrimaryShell()
	if cs == nil {
		t.Fatal("session has no primary shell")
	}
	waitForIdleBytes(t, mm, s.ID, cs.ID)

	// Build the output token at execution time. The typed command itself does
	// not contain HISTORY_CANCELLED, so terminal echo behavior cannot affect
	// the assertion on whether the command actually ran.
	command := "printf 'HISTORY_%s\\n' CANCELLED"
	if runtime.GOOS == "windows" {
		command = "Write-Output ('HISTORY_' + 'CANCELLED')"
	}
	if err := cs.SendTerminalBytes([]byte(command), false); err != nil {
		t.Fatal(err)
	}
	if got := historyInputMarks(t, mm, s.ID, cs.ID); len(got) != 0 {
		t.Fatalf("unfinished line created input marks: %v", got)
	}
	if err := cs.PressKey("ctrl+c", 1); err != nil {
		t.Fatal(err)
	}
	waitForIdleBytes(t, mm, s.ID, cs.ID)
	if got := historyInputMarks(t, mm, s.ID, cs.ID); len(got) != 1 || got[0] != api.LogAPIInput {
		t.Fatalf("interrupted line marks = %v, want [i]", got)
	}
	out, _, err := cs.OutputByteRange(0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "HISTORY_CANCELLED") {
		t.Fatal("cancelled command produced execution output")
	}
}
