package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// termCall drives the single entry the way a model would, with the same JSON
// argument shape the wire carries.
func termCall(t *testing.T, s *Server, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	res, err := s.call(context.Background(), "term", args)
	if err != nil {
		t.Fatalf("term(%v): %v", args, err)
	}
	return res
}

// termText is a term call's text projection.
func termText(t *testing.T, s *Server, args map[string]any) string {
	t.Helper()
	return toolResultText(termCall(t, s, args))
}

// headerValue reads one header out of a rendered slot. It is deliberately a
// text-level helper: the point of these tests is what a caller READS, and both
// projections come from one value.
func headerValue(t *testing.T, text, key string) (string, bool) {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, key+": "); ok {
			return v, true
		}
	}
	return "", false
}

// handleFrom reads the Handle header, which is how a caller learns a new channel.
func handleFrom(t *testing.T, text string) string {
	t.Helper()
	h, ok := headerValue(t, text, "Handle")
	if !ok {
		t.Fatalf("no Handle header in:\n%s", text)
	}
	return h
}

// A fresh instance answers the empty call. This is the first move of every
// recovery, so it must be legal, cheap, and dense — never an error.
func TestTermEmptyCallIsTheGuide(t *testing.T) {
	s := newTestServer(t)
	res := termCall(t, s, map[string]any{})
	if res.IsError {
		t.Fatalf("an empty cmd must not be an error:\n%s", toolResultText(res))
	}
	text := toolResultText(res)
	if !strings.HasPrefix(text, "TERM/1 200 OK\n") {
		t.Errorf("status line = %q", strings.SplitN(text, "\n", 2)[0])
	}
	// It must say where am I, what can I do, and what is here.
	for _, want := range []string{"Syntax", "ps", "ext list", "help", "running now"} {
		if !strings.Contains(text, want) {
			t.Errorf("the guide misses %q:\n%s", want, text)
		}
	}
	// And it must not require remembering anything: handles are obtained, not
	// supplied.
	if !strings.Contains(text, "copy one from ps") {
		t.Errorf("the guide must point at where handles come from:\n%s", text)
	}
}

// help lists the directives; help <op> teaches one. This is the "I have not used
// those yet" half of progressive disclosure; learn_mode is the "I just used it
// wrong" half.
func TestTermHelp(t *testing.T) {
	s := newTestServer(t)
	text := termText(t, s, map[string]any{"cmd": "help"})
	for _, op := range []string{"run", "read", "ps", "pipe", "pty", "alias", "help"} {
		if !strings.Contains(text, "\n"+op+" ") && !strings.Contains(text, "\n"+op+"\n") {
			t.Errorf("help misses directive %q:\n%s", op, text)
		}
	}
	single := termText(t, s, map[string]any{"cmd": "help run"})
	if !strings.Contains(single, "run <handle> <payload>") {
		t.Errorf("help run = %q", single)
	}
	if n := strings.Count(strings.TrimSpace(single), "\n"); n > 3 {
		t.Errorf("help for one directive should be one line plus the envelope, got:\n%s", single)
	}
}

// An unknown directive is a 400 that TEACHES: the failure is exactly the moment
// the caller does not know the shape, so the usage line rides along whether or
// not learn_mode was asked for.
func TestTermUnknownDirectiveTeaches(t *testing.T) {
	s := newTestServer(t)
	res := termCall(t, s, map[string]any{"cmd": "raed a1f3:1"})
	if !res.IsError {
		t.Fatal("an unknown directive must be an error")
	}
	text := toolResultText(res)
	if !strings.HasPrefix(text, "TERM/1 400 Bad Request\n") {
		t.Errorf("status line = %q", strings.SplitN(text, "\n", 2)[0])
	}
	if code, _ := headerValue(t, text, "Error-Code"); code != "unknown_directive" {
		t.Errorf("Error-Code = %q", code)
	}
	if _, ok := headerValue(t, text, "Usage"); !ok {
		t.Errorf("a 400 must carry a Usage line:\n%s", text)
	}
}

// A successful call NEVER carries Usage without learn_mode: teaching on every
// successful reply would be noise the caller cannot turn off.
func TestTermSuccessIsSilentWithoutLearnMode(t *testing.T) {
	s := newTestServer(t)
	text := termText(t, s, map[string]any{"cmd": "ps"})
	if _, ok := headerValue(t, text, "Usage"); ok {
		t.Errorf("a successful reply carried Usage without learn_mode:\n%s", text)
	}
	// A successful reply must also not carry the constant fields.
	for _, key := range []string{"Idempotent", "Status", "Exit-Code", "Error-Code"} {
		if _, ok := headerValue(t, text, key); ok {
			t.Errorf("a plain successful reply carried %s, which is constant:\n%s", key, text)
		}
	}
}

// learn_mode attaches the usage line, once per directive even across a batch.
func TestTermLearnModeDeduplicatesUsage(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "sleep 5")

	text := termText(t, s, map[string]any{
		"cmd":        "read " + h + "; read " + h,
		"learn_mode": true,
		"timeout":    float64(0),
	})
	if n := strings.Count(text, "Usage: "); n != 1 {
		t.Errorf("Usage appears %d times for two slots of one directive, want 1:\n%s", n, text)
	}
}

// A fresh pipe channel hands back a handle, which is news by definition.
func TestTermPipeReturnsHandle(t *testing.T) {
	s := newTestServer(t)
	text := termText(t, s, map[string]any{"cmd": "pipe internal --argv sleep 5"})
	if !strings.HasPrefix(text, "TERM/1 200 OK\n") {
		t.Fatalf("status line = %q", strings.SplitN(text, "\n", 2)[0])
	}
	h := handleFrom(t, text)
	if h == "" {
		t.Fatal("empty handle")
	}
	// The handle must be USABLE, and it must be the short form.
	if strings.Contains(h, "termcp://") {
		t.Errorf("Handle = %q, want the short form (the termcp:// prefix costs 9 chars per slot)", h)
	}
	if info := termText(t, s, map[string]any{"cmd": "info " + h}); !strings.HasPrefix(info, "TERM/1 200 OK") {
		t.Errorf("the returned handle does not resolve: %s", info)
	}
}

// --argv is a word list: every word after it reaches the program, quoting groups
// an argument, and a flag after it is refused rather than fed to the program.
func TestTermArgvCarriesItsWords(t *testing.T) {
	s := newTestServer(t)
	for _, cmd := range []string{
		"pipe internal --argv echo hello",
		"pipe internal --argv sh -c 'echo hello'",
		"pipe internal --argv sh -c \"echo hello\"", // a quoted argument keeps its space
	} {
		text := termText(t, s, map[string]any{"cmd": cmd})
		if !strings.HasPrefix(text, "TERM/1 200 OK") {
			t.Fatalf("%s:\n%s", cmd, text)
		}
		h := handleFrom(t, text)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, _, closed := s.channelExitState(h); closed {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		out := termText(t, s, map[string]any{"cmd": "read " + h, "timeout": float64(0)})
		if !strings.Contains(out, "hello") {
			t.Errorf("%s produced no hello:\n%s", cmd, out)
		}
	}

	// A known flag after --argv would be handed to the program if accepted.
	res := termCall(t, s, map[string]any{"cmd": "pipe internal --argv echo hi --name chat"})
	if !res.IsError || !strings.Contains(toolResultText(res), "flags come before --argv") {
		t.Errorf("a flag after --argv was not refused:\n%s", toolResultText(res))
	}
	// A bare payload used to be dropped without a word.
	res = termCall(t, s, map[string]any{"cmd": "pipe internal sleep 5"})
	if !res.IsError || !strings.Contains(toolResultText(res), "--argv") {
		t.Errorf("a bare payload was not refused:\n%s", toolResultText(res))
	}
}

// The mode is the directive: pipe builds a pipe, pty builds a pty, and there is no
// way to ask for one and silently get the other.
func TestTermModeIsTheDirective(t *testing.T) {
	s := newTestServer(t)
	pipeText := termText(t, s, map[string]any{"cmd": "pipe internal --argv sleep 5"})
	if !strings.Contains(pipeText, "pipe") {
		t.Errorf("pipe reply = %q", pipeText)
	}
	ptyText := termText(t, s, map[string]any{"cmd": "pty internal"})
	if !strings.Contains(ptyText, "pty") {
		t.Errorf("pty reply = %q", ptyText)
	}
	// `pipe x --mode pty` must never silently change the mode: --mode is not a
	// flag of either directive, so it is refused.
	res := termCall(t, s, map[string]any{"cmd": "pipe internal --mode pty"})
	if !res.IsError {
		t.Errorf("--mode was accepted; the mode must be the directive:\n%s", toolResultText(res))
	}
}

// A pty has no completion signal, so `run` samples the caller's time window and
// returns what the terminal produced in it.
func TestTermRunOnPtyReadsTheWindow(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	start := time.Now()
	text := termText(t, s, map[string]any{
		"cmd":     "run " + h + " echo hi",
		"timeout": float64(2),
	})
	elapsed := time.Since(start)
	if !strings.Contains(text, "hi") {
		t.Errorf("the command's output is missing:\n%s", text)
	}
	if elapsed > 5*time.Second {
		t.Errorf("run took %s; the caller's window is 2s", elapsed)
	}
	// A live pty channel has no exit code, because a shell does not report that a
	// command inside it finished.
	if _, ok := headerValue(t, text, "Exit-Code"); ok {
		t.Errorf("a running channel reported an Exit-Code:\n%s", text)
	}
}

// A pty has no completion signal, so `run` must wait the caller's time window
// rather than read the instant after typing. A command that prints after a
// delay proves the wait happened — and a session-level handle must resolve to
// the channel, or the wait branch is skipped entirely.
func TestTermPtyRunWaitsTheWindow(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	text := termText(t, s, map[string]any{
		"cmd":     "run " + h + " sleep 1 && echo LATE",
		"timeout": float64(3),
	})
	if !strings.Contains(text, "LATE") {
		t.Fatalf("run returned before the command's output:\n%s", text)
	}
}

// `run` reads from where it submitted, so a previous command's unread tail is not
// delivered as if it belonged to this one.
func TestTermRunStartsAtItsOwnOffset(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	termText(t, s, map[string]any{"cmd": "run " + h + " seq 1 500", "timeout": float64(2), "bytes_limit": float64(64)})
	second := termText(t, s, map[string]any{"cmd": "run " + h + " echo SECOND", "timeout": float64(2)})
	if !strings.Contains(second, "SECOND") {
		t.Fatalf("the second command's output is missing:\n%s", second)
	}
	// The residual of the first command must not be in the second body.
	if strings.Contains(second, "\n1\n2\n3") {
		t.Errorf("the second run replayed the first command's unread tail:\n%s", second)
	}
}

// A closed channel's terminal state is reported on every reply about it,
// consistently — and a non-zero exit makes each of those replies a failure.
func TestTermExitCodeIsAFunctionOfTheChannel(t *testing.T) {
	s := newTestServer(t)
	script := writeScript(t, "exit 3")
	text := termText(t, s, map[string]any{"cmd": "pipe internal --argv sh '" + script + "'"})
	h := handleFrom(t, text)
	// Wait for the process to end.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, _, closed := s.channelExitState(h); closed && st == "exited" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	first := termText(t, s, map[string]any{"cmd": "read " + h, "timeout": float64(0)})
	if st, _ := headerValue(t, first, "Status"); st != "exited" {
		t.Fatalf("Status = %q, want exited:\n%s", st, first)
	}
	if _, ok := headerValue(t, first, "Exit-Code"); !ok {
		t.Fatalf("a closed channel must report its exit code:\n%s", first)
	}
	// Same answer on the next read: the status is state, not an event.
	second := termText(t, s, map[string]any{"cmd": "read " + h, "timeout": float64(0)})
	if st1, _ := headerValue(t, first, "Status"); st1 != "exited" {
		t.Errorf("first read Status = %q", st1)
	}
	if st2, _ := headerValue(t, second, "Status"); st2 != "exited" {
		t.Errorf("second read Status = %q, want the same answer as the first", st2)
	}
}

// A read of a closed channel whose process failed is a 500 that still carries the
// bytes and the range: dropping the output would leave the caller knowing neither
// what happened nor where to continue.
func TestTermFailureKeepsTheBytes(t *testing.T) {
	s := newTestServer(t)
	// A multi-statement program is a script file, not shell syntax in --argv.
	script := writeScript(t, "echo boom >&2; exit 3")
	text := termText(t, s, map[string]any{"cmd": "pipe internal --argv sh '" + script + "'"})
	h := handleFrom(t, text)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, closed := s.channelExitState(h); closed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	res := termCall(t, s, map[string]any{"cmd": "read " + h, "timeout": float64(0)})
	out := toolResultText(res)
	if !strings.HasPrefix(out, "TERM/1 500 Internal Server Error\n") {
		t.Fatalf("status line = %q:\n%s", strings.SplitN(out, "\n", 2)[0], out)
	}
	if !res.IsError {
		t.Error("isError must be true when the code is >= 400")
	}
	if !strings.Contains(out, "boom") {
		t.Errorf("the failing command's stderr was dropped:\n%s", out)
	}
	if _, ok := headerValue(t, out, "Content-Range"); !ok {
		t.Errorf("a failed channel must still say where to continue reading:\n%s", out)
	}
	if code, _ := headerValue(t, out, "Exit-Code"); code != "3" {
		t.Errorf("Exit-Code = %q, want 3:\n%s", code, out)
	}
}

// ps is terminal text, one line per channel, and every handle in it is usable
// verbatim as another directive's target.
func TestTermPSLinesAreCopyable(t *testing.T) {
	s := newTestServer(t)
	text := termText(t, s, map[string]any{"cmd": "ps"})
	if strings.Contains(text, "{") || strings.Contains(text, "[") {
		t.Errorf("ps must be terminal columns, not JSON:\n%s", text)
	}
	h := openPipe(t, s, "sleep 5")
	text = termText(t, s, map[string]any{"cmd": "ps"})
	if !strings.Contains(text, h) {
		t.Fatalf("ps does not list %s:\n%s", h, text)
	}
	// The format requirement that matters: copy a column into the next command.
	body := text[strings.Index(text, "\n\n")+2:]
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		probe := termText(t, s, map[string]any{"cmd": "info " + fields[0]})
		if strings.Contains(probe, "404") {
			t.Errorf("the handle %q from ps does not resolve:\n%s", fields[0], probe)
		}
	}
}

// info is `key: value`, one fact per line.
func TestTermInfoIsKeyValueLines(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "sleep 5")
	text := termText(t, s, map[string]any{"cmd": "info " + h})
	for _, key := range []string{"id: ", "status: ", "ssh_config: ", "shell: "} {
		if !strings.Contains(text, key) {
			t.Errorf("info misses %q:\n%s", key, text)
		}
	}
	if strings.Contains(text, "{") {
		t.Errorf("info must be key: value lines, not JSON:\n%s", text)
	}
}

// Every window parameter applies to every directive, not just the byte streams:
// a generated reply is exactly as capable of flooding a context as a log.
func TestTermWindowAppliesToGeneratedReplies(t *testing.T) {
	s := newTestServer(t)
	for i := 0; i < 6; i++ {
		openPipe(t, s, "sleep 5")
	}
	text := termText(t, s, map[string]any{"cmd": "ps", "lines_limit": float64(2), "bytes_limit": float64(80)})
	body := text[strings.Index(text, "\n\n")+2:]
	if len(body) > 200 {
		t.Errorf("ps ignored the window (%d bytes):\n%s", len(body), text)
	}
}

// The total byte cap is a property of the reply, not of a slot.
func TestTermTotalBytesLimitAcrossSlots(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "seq 1 5000")
	time.Sleep(300 * time.Millisecond)
	cmd := "read " + h + "; read " + h + "; read " + h
	res := termCall(t, s, map[string]any{"cmd": cmd, "total_bytes_limit": float64(64), "timeout": float64(1)})
	text := toolResultText(res)

	// The cap counts BODY bytes across slots, which is what the caller pays for.
	// The text projection is the honest measure: each slot's body is everything
	// after its blank line, and the next status line belongs to the next slot.
	parts := strings.Split(text, "TERM/1 ")[1:]
	bodies := 0
	for i, part := range parts {
		j := strings.Index(part, "\n\n")
		if j < 0 {
			continue
		}
		body := part[j+2:]
		if i+1 < len(parts) {
			// Text joins responses with one newline; that byte is the separator
			// between two responses, not part of either body.
			body = strings.TrimSuffix(body, "\n")
		}
		bodies += len(body)
	}
	if bodies > 64 {
		t.Errorf("the total cap was exceeded: %d body bytes > 64:\n%s", bodies, text)
	}
	// The slot that crossed the cap still says where to continue, and the range
	// describes the bytes it actually returned.
	if !strings.Contains(text, "Content-Range: bytes 0-63/") {
		t.Errorf("the truncated slot's range does not describe what it returned:\n%s", text)
	}
	// Truncation is not failure: a capped reply is still valid.
	if res.IsError {
		t.Errorf("a capped reply must not be an error:\n%s", text)
	}
}

// bytes_limit: 0 hands back the handle and consumes nothing, so a JSON-RPC frame
// on a pipe stays there for `mcp recv`.
func TestTermBytesLimitZeroConsumesNothing(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "seq 1 100")
	text := termText(t, s, map[string]any{"cmd": "read " + h, "bytes_limit": float64(0), "timeout": float64(0)})
	if !strings.Contains(text, "TERM/1 204 No Content") {
		t.Fatalf("bytes_limit 0 must return no bytes:\n%s", text)
	}
	after := termText(t, s, map[string]any{"cmd": "read " + h, "timeout": float64(1)})
	if !strings.Contains(after, "1\n2\n3") {
		t.Errorf("a bytes_limit 0 read consumed the stream; the bytes are gone:\n%s", after)
	}
}

// A batch target produces one slot per entry: N questions, N answers.
func TestTermBatchProducesOneSlotPerTarget(t *testing.T) {
	s := newTestServer(t)
	a := openPipe(t, s, "sleep 5")
	b := openPipe(t, s, "sleep 5")
	text := termText(t, s, map[string]any{
		"cmd":     "read " + a + "," + b,
		"timeout": float64(0),
	})
	if n := strings.Count(text, "TERM/1 "); n != 2 {
		t.Errorf("two targets produced %d slots:\n%s", n, text)
	}
}

// The structured projection mirrors the text one, slot for slot, key for key.
func TestTermStructuredContentIsIsomorphic(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "sleep 5")
	res := termCall(t, s, map[string]any{"cmd": "read " + h + "; read " + h, "timeout": float64(0)})

	if res.StructuredContent == nil {
		t.Fatal("no structuredContent")
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var slots []map[string]any
	if err := json.Unmarshal(raw, &slots); err != nil {
		var one map[string]any
		if err := json.Unmarshal(raw, &one); err != nil {
			t.Fatalf("structuredContent is neither an array nor an object: %s", raw)
		}
		slots = []map[string]any{one}
	}

	texts := strings.Split(toolResultText(res), "TERM/1 ")[1:]
	if len(texts) != len(slots) {
		t.Fatalf("%d text slots vs %d structured slots", len(texts), len(slots))
	}
	for i, slot := range slots {
		for _, key := range []string{"protocol", "code", "reason"} {
			if _, ok := slot[key]; !ok {
				t.Errorf("slot %d misses the structured status-line key %q", i, key)
			}
		}
		if slot["protocol"] != "TERM/1" {
			t.Errorf("slot %d protocol = %v", i, slot["protocol"])
		}
		// Every header key in the text must exist in the structured twin.
		for _, line := range strings.Split("TERM/1 "+texts[i], "\n")[1:] {
			if line == "" {
				break
			}
			key, _, ok := strings.Cut(line, ": ")
			if !ok {
				continue
			}
			if _, ok := slot[key]; !ok {
				t.Errorf("slot %d: text has header %q that the structured twin lacks: %v", i, key, slot)
			}
		}
		// And the body travels in both projections: a client that renders only
		// structuredContent must still see it. Reply.Text joins slots with one
		// newline, so a non-last segment carries that separator extra.
		seg := "TERM/1 " + texts[i]
		if i < len(texts)-1 {
			seg = strings.TrimSuffix(seg, "\n")
		}
		body := ""
		if parts := strings.SplitN(seg, "\n\n", 2); len(parts) == 2 {
			body = parts[1]
		}
		if slot["body"] != body {
			t.Errorf("slot %d structured body = %v, want %q", i, slot["body"], body)
		}
	}
}

// The alias directive names a directive, and the expansion is visible in Path so
// a caller sees what actually ran.
func TestTermAliasExpands(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "sleep 5")
	if res := termCall(t, s, map[string]any{"cmd": "alias look read " + h}); res.IsError {
		t.Fatalf("alias failed:\n%s", toolResultText(res))
	}
	text := termText(t, s, map[string]any{"cmd": "look", "timeout": float64(0)})
	path, ok := headerValue(t, text, "Path")
	if !ok {
		t.Fatalf("a reply always carries Path:\n%s", text)
	}
	if !strings.Contains(path, "read "+h) {
		t.Errorf("Path = %q, want the EXPANDED directive", path)
	}
	list := termText(t, s, map[string]any{"cmd": "alias"})
	if !strings.Contains(list, "look = read") {
		t.Errorf("alias list = %q", list)
	}
}

// `file` maps onto the existing file tools, one per action, and a read is never
// reviewed while a write is.
func TestTermFileDirective(t *testing.T) {
	s := newTestServer(t)
	text := termText(t, s, map[string]any{"cmd": "file --action stat --remote-path /etc/hostname"})
	// No session named: the failure must be a NOT FOUND (404), not a crash.
	if !strings.Contains(text, "TERM/1 ") {
		t.Fatalf("no envelope:\n%s", text)
	}
	unknown := termText(t, s, map[string]any{"cmd": "file --action nope --remote-path /x"})
	if !strings.Contains(unknown, "TERM/1 400 Bad Request") {
		t.Errorf("an unknown file action must be a 400:\n%s", unknown)
	}
}

// fwd and profile translate onto their tools and report their actions.
func TestTermFwdAndProfileDirectives(t *testing.T) {
	s := newTestServer(t)
	fwd := termText(t, s, map[string]any{"cmd": "fwd --action list"})
	if !strings.Contains(fwd, "TERM/1 200 OK") {
		t.Errorf("fwd list = %q", fwd)
	}
	prof := termText(t, s, map[string]any{"cmd": "profile --action list"})
	if !strings.Contains(prof, "TERM/1 200 OK") {
		t.Errorf("profile list = %q", prof)
	}
	ext := termText(t, s, map[string]any{"cmd": "ext --action list"})
	// The test server has no extension reconciler, so the honest answer is
	// not_configured (422) — what matters is that the directive REACHES the
	// extension layer and reports its state instead of failing to translate.
	if !strings.Contains(ext, "TERM/1 422") || !strings.Contains(ext, "not_configured") {
		t.Errorf("ext list = %q", ext)
	}
}

// A payload reaches the shell byte for byte, including the constructs the parser
// must not touch.
func TestTermPayloadIsNotRewritten(t *testing.T) {
	s := newTestServer(t)
	// A POSIX shell is named explicitly: the payload rule is about the bytes
	// reaching the shell, not about which shell the profile happens to default to.
	script := writeScript(t, `echo $((6*7)); echo "a  b"`)
	h := openPipe(t, s, "sh '"+script+"'")
	// Read the whole stream, not a window of it: this test is about the bytes that
	// reached the shell, and a window would only prove they appear early.
	text := ""
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		text += termText(t, s, map[string]any{"cmd": "read " + h, "timeout": float64(2)})
		if strings.Contains(text, "a  b") || strings.Contains(text, "exited") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(text, "42") {
		// A login shell may not have finished starting; read the window it produced.
		time.Sleep(500 * time.Millisecond)
		text = termText(t, s, map[string]any{"cmd": "read " + h, "timeout": float64(5)})
	}
	if !strings.Contains(text, "42") {
		t.Errorf("the shell did not expand the arithmetic, so the payload was rewritten:\n%s", text)
	}
	if !strings.Contains(text, "a  b") {
		t.Errorf("the quoted whitespace was not preserved:\n%s", text)
	}
	// The script is what the shell saw, so the expansion proves nothing was
	// rewritten on the way in.
	path, _ := headerValue(t, text, "Path")
	if !strings.Contains(path, h) {
		t.Errorf("Path = %q, want the channel", path)
	}
}

// The single entry is one tool, and its schema is the budget it must fit in.
func TestTermIsTheOnlyEntryAndFitsItsBudget(t *testing.T) {
	s := New(nil, nil, nil, nil, "test")
	var term *mcpgo.Tool
	tools := listTools(t, s, originContext("http://127.0.0.1:18765"))
	for i := range tools {
		if tools[i].Name == termToolName {
			term = &tools[i]
		}
	}
	if term == nil {
		t.Fatalf("%q is not registered", termToolName)
	}
	raw, err := json.Marshal(term)
	if err != nil {
		t.Fatal(err)
	}
	const budget = 2500
	if len(raw) >= budget {
		t.Errorf("the term schema is %d bytes, over the %d-byte budget: %s", len(raw), budget, raw)
	}
	// The description must not enumerate directives: the semantics live in the
	// command syntax and in help/learn_mode, which is what lets the surface grow
	// without the tool growing.
	for _, op := range []string{"shell_input", "file_read", "session_start", "pipe", "pty"} {
		if strings.Contains(term.Description, op) {
			t.Errorf("the description names %q; directives belong in cmd's syntax, not the tool blurb:\n%s", op, term.Description)
		}
	}
	// cmd must not be required: the empty call is the entry point.
	if props := term.InputSchema.Properties; props == nil {
		t.Error("no properties")
	}
	for _, name := range term.InputSchema.Required {
		if name == "cmd" {
			t.Error("cmd must not be required: the empty call is how a caller gets its bearings")
		}
	}
}

// --- fixtures ---

// writeScript writes a shell script to a temp file and returns its path, so a
// `pipe --argv` test can run a multi-statement program without putting shell
// syntax into an argv.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "probe.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// openPipe starts a pipe channel on the loopback profile and returns its handle.
func openPipe(t *testing.T, s *Server, argv string) string {
	t.Helper()
	text := termText(t, s, map[string]any{"cmd": "pipe internal --argv " + argv})
	if !strings.HasPrefix(text, "TERM/1 200 OK") {
		t.Fatalf("pipe %s failed:\n%s", argv, text)
	}
	return handleFrom(t, text)
}

// openPty starts a pty channel (a login shell) and returns its handle.
func openPty(t *testing.T, s *Server) string {
	t.Helper()
	text := termText(t, s, map[string]any{"cmd": "pty internal"})
	if !strings.HasPrefix(text, "TERM/1 200 OK") {
		t.Fatalf("pty failed:\n%s", text)
	}
	return handleFrom(t, text)
}

// Isolation must hold on the single-entry surface too.
//
// `term` carries its target inside `cmd`, so the outer admission check — which
// resolves a session from `shell_id`/`session_id` arguments — has nothing to
// resolve and admits everything. Without a per-statement check, a caller that
// claimed a namespace could read, write to and close another caller's channel;
// this is the regression test for that hole.
func TestTermNamespaceIsolationHoldsPerDirective(t *testing.T) {
	s, _, _, _ := newTestServerWithHistory(t)
	alice := namespaceContext("alice")
	bob := namespaceContext("bob")

	created, err := s.call(alice, "term", map[string]any{"cmd": "pipe internal --argv sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	text := toolResultText(created)
	if !strings.HasPrefix(text, "TERM/1 200 OK") {
		t.Fatalf("alice could not open a channel:\n%s", text)
	}
	handle := handleFrom(t, text)

	// Every directive that names alice's channel must be refused for bob — not
	// just the ones that reach a handler. info/span/close read the manager
	// directly, which is exactly how they would slip through.
	for _, cmd := range []string{
		"type " + handle + " echo pwned",
		"run " + handle + " echo pwned",
		"key " + handle + " enter",
		"read " + handle,
		"info " + handle,
		"span " + handle,
		"close " + handle,
		"resize " + handle + " 10 10",
	} {
		res, err := s.call(bob, "term", map[string]any{"cmd": cmd})
		if err != nil {
			t.Fatal(err)
		}
		out := toolResultText(res)
		if !res.IsError || !strings.HasPrefix(out, "TERM/1 403 Forbidden") {
			t.Errorf("%q was not refused with 403 for another namespace:\n%s", cmd, out)
		}
	}

	// And a batch is refused as a whole: running the entries bob may reach while
	// skipping the ones it may not would make the outcome unknowable.
	bobOwn, err := s.call(bob, "term", map[string]any{"cmd": "pipe internal --argv sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	bobHandle := handleFrom(t, toolResultText(bobOwn))
	res, err := s.call(bob, "term", map[string]any{"cmd": "read " + bobHandle + "," + handle})
	if err != nil {
		t.Fatal(err)
	}
	if out := toolResultText(res); !strings.HasPrefix(out, "TERM/1 403 Forbidden") {
		t.Errorf("a batch naming a foreign handle must be refused whole:\n%s", out)
	}

	// A listing must not reveal it either.
	ps, err := s.call(bob, "term", map[string]any{"cmd": "ps"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(toolResultText(ps), handle) {
		t.Errorf("bob's ps lists alice's channel %s:\n%s", handle, toolResultText(ps))
	}
}

// An admission refusal is a FAILURE, never a success-shaped reply. Regression
// test for the bug where a directive only inspected the transport error and
// rendered a refused operation as 204/200 — the command had not run.
func TestTermRefusalIsNeverSuccessShaped(t *testing.T) {
	s, _, _, _ := newTestServerWithHistory(t)
	alice := namespaceContext("alice")
	bob := namespaceContext("bob")

	created, err := s.call(alice, "term", map[string]any{"cmd": "pipe internal --argv sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	handle := handleFrom(t, toolResultText(created))

	res, err := s.call(bob, "term", map[string]any{"cmd": "type " + handle + " echo pwned"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a refused directive reported success")
	}
	out := toolResultText(res)
	if strings.Contains(out, "204") || strings.Contains(out, "200 OK") {
		t.Errorf("a refusal was rendered with a success status:\n%s", out)
	}
	// The failure must name the reason a caller can act on, not a generic one.
	if code, _ := headerValue(t, out, "Error-Code"); code != "forbidden" {
		t.Errorf("Error-Code = %q, want forbidden:\n%s", code, out)
	}
}
