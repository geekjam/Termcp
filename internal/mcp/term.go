package mcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/envelope"
)

// The single-entry tool.
//
// Everything this instance can do is reachable through one tool whose only
// required input is nothing at all: `term()` answers "where am I and what is
// here", and `term(cmd: "run a1f3:1 go build ./...")` is one command in one call
// where the specialized surface needed four. The capability layer is what makes
// that safe — the same admission decision covers this entry and the specialized
// tools, so a narrower surface cannot be a way around a wider one.
//
// Terminating the tool surface is the point: the tool list stops growing because
// growth happens in the COMMAND space (a new directive, an alias, an installed
// MCP server) rather than in `tools/list`. See the design note in
// docs/mcp-tools.md.
const termToolName = "term"

// window defaults. Two seconds is the sampling window for a pty and the upper
// bound for a pipe's exit wait. Space stays the scarce axis: a wide window
// spends context that is never recovered.
const (
	// One window for both modes. A pipe returns as soon as its process exits
	// (early), a pty waits the window out; two seconds is long enough to catch
	// a terminal's late chunks and short enough that a stuck wait is cheap.
	defaultTimeoutSeconds  = 2
	defaultBytesLimit      = 8192
	defaultLinesLimit      = 200
	defaultTotalBytesLimit = 32768
)

// termArgs is the parsed window of one term call: the parameters that decide how
// the caller sees a command, never what the command does.
//
// They are named parameters on the tool rather than flags inside cmd for three
// reasons, all of which showed up in practice: they apply to every directive
// (`ps` and `help` need protection too), they must be visible in the schema with
// their defaults (so "this does not blow up my context" is a promise the caller
// can READ), and they never reach the shell — bytes sent to a shell are shaped by
// the shell, not by this protocol.
type termArgs struct {
	TimeoutSeconds  float64
	BytesLimit      int
	LinesLimit      int
	TotalBytesLimit int
	OnTimeOverflow  string
	OnBytesOverflow string
	OnLinesOverflow string
	PipeTo          string
	LearnMode       bool
}

// parseTermArgs reads the window parameters and range-checks them. A value
// outside its domain is an argument error, not a silent substitution: a caller
// that typed 0 where it meant 8192 should be told, not quietly given a different
// read.
func parseTermArgs(args map[string]any) (termArgs, *directive.Error) {
	w := termArgs{
		TimeoutSeconds:  getFloat64(args, "timeout", defaultTimeoutSeconds),
		BytesLimit:      int(getFloat64(args, "bytes_limit", defaultBytesLimit)),
		LinesLimit:      int(getFloat64(args, "lines_limit", defaultLinesLimit)),
		TotalBytesLimit: int(getFloat64(args, "total_bytes_limit", defaultTotalBytesLimit)),
		OnTimeOverflow:  overflowPolicy(args, "on_time_overflow"),
		OnBytesOverflow: overflowPolicy(args, "on_bytes_overflow"),
		OnLinesOverflow: overflowPolicy(args, "on_lines_overflow"),
		PipeTo:          strings.TrimSpace(getString(args, "pipe_to", "")),
		LearnMode:       getBool(args, "learn_mode", false),
	}
	switch {
	case w.TimeoutSeconds < 0 || w.TimeoutSeconds > 3600:
		return termArgs{}, &directive.Error{Msg: "timeout must be between 0 and 3600 seconds"}
	case w.BytesLimit < 0:
		return termArgs{}, &directive.Error{Msg: "bytes_limit must be >= 0 (0 = handle only, no bytes)"}
	case w.LinesLimit < 0:
		return termArgs{}, &directive.Error{Msg: "lines_limit must be >= 0 (0 = no line limit)"}
	case w.TotalBytesLimit <= 0:
		return termArgs{}, &directive.Error{Msg: "total_bytes_limit must be > 0: it is the absolute cap for the whole reply"}
	}
	return w, nil
}

// overflowPolicy normalises an overflow behaviour. truncate is the default
// because fold (return nothing this time) has to be asked for: a caller that runs
// `ls` on a huge directory wants the head of it, not an empty reply.
func overflowPolicy(args map[string]any, key string) string {
	v := strings.TrimSpace(strings.ToLower(getString(args, key, "")))
	switch v {
	case "", "truncate":
		return "truncate"
	case "fold":
		return "fold"
	}
	return "truncate"
}

// handleTerm is the single entry point.
func (s *Server) handleTerm(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	rawCmd := getString(request.GetArguments(), "cmd", "")
	win, derr := parseTermArgs(request.GetArguments())
	if derr != nil {
		return termSlotResult(envelope.Slot{
			Code:    400,
			Headers: map[string]string{"Error-Code": "invalid_argument"},
			Body:    derr.Msg,
		}), nil
	}

	file, err := directive.Parse(rawCmd)
	if err != nil {
		// A directive the caller invented (an alias) is unknown to the parser by
		// definition, so it is resolved here before the error is reported.
		if stmt, ok := aliases.expandError(err); ok {
			return s.runStatements(ctx, []directive.Statement{stmt}, win), nil
		}
		return termSlotResult(s.parseErrorSlot(err)), nil
	}
	// An empty cmd is the "I just woke up" call: it must be legal, cheap, and
	// information-dense, never an error.
	if file.Empty {
		return s.termGuide(ctx, win)
	}

	return s.runStatements(ctx, file.Statements, win), nil
}

// runStatements dispatches already-parsed statements and assembles the reply.
//
// An alias is expanded before dispatch and before Path is read, so the echo a
// caller gets back is the command that actually ran.
func (s *Server) runStatements(ctx context.Context, stmts []directive.Statement, win termArgs) *mcpgo.CallToolResult {
	out := termReply{win: win, seenUsage: map[string]bool{}}
	s.currentReply = &out
	defer func() { s.currentReply = nil }()
	for _, stmt := range stmts {
		out.slots = append(out.slots, s.runDirective(ctx, aliases.Expand(stmt), win)...)
	}
	return out.result()
}

// parseErrorSlot renders a directive-level failure as one slot, so a bad
// statement can sit beside a good one without taking the whole call down.
//
// A directive or flag error is 400 — the syntax is wrong, not the state — and it
// carries its Usage line whether or not learn_mode was asked for: an error is
// exactly the moment the caller does not know the shape, and that is what the
// line is for. Knowing the shape already costs nothing, because a successful call
// never carries one.
func (s *Server) parseErrorSlot(err error) envelope.Slot {
	derr, ok := err.(*directive.Error)
	if !ok {
		return envelope.Slot{
			Code:    400,
			Headers: map[string]string{"Error-Code": "invalid_argument"},
			Body:    err.Error(),
		}
	}
	headers := map[string]string{"Error-Code": errorCodeFor(derr)}
	if derr.Usage != "" {
		headers["Usage"] = derr.Usage
	}
	if _, known := directive.Lookup(derr.Op); !known && derr.Op != "" {
		// An unknown directive is exactly the caller who needs the table, so the
		// whole listing rides along instead of a second round trip.
		headers["Usage"] = directiveTable()
	}
	return envelope.Slot{Code: 400, Headers: headers, Body: derr.Msg}
}

// directiveTable renders the one-line table `help` prints, for the error reply
// that has to teach the whole shape.
func directiveTable() string {
	var b strings.Builder
	for i, op := range directive.Ops() {
		if i > 0 {
			b.WriteString(" | ")
		}
		b.WriteString(op.Name)
	}
	return b.String()
}

// errorCodeFor classifies a directive error.
func errorCodeFor(derr *directive.Error) string {
	if derr.Op == "" {
		return "invalid_argument"
	}
	if _, known := directive.Lookup(derr.Op); !known {
		return "unknown_directive"
	}
	return "invalid_argument"
}

// termReply accumulates the slots of one call and applies the total byte cap.
type termReply struct {
	win       termArgs
	slots     []envelope.Slot
	seenUsage map[string]bool
}

// result assembles the tool result: the text projection, the structured twin, and
// the isError flag taken from the codes.
//
// The total byte cap is applied here, across slots, because it is a property of
// the REPLY and not of one slot: ten slots each within their own byte limit still
// add up to a context the caller did not budget for.
func (t termReply) result() *mcpgo.CallToolResult {
	slots := t.applyTotalCap(t.slots)
	reply := envelope.Reply{Slots: slots}
	result := mcpgo.NewToolResultText(reply.Text())
	result.IsError = reply.IsError()
	structured := reply.Structured()
	if len(structured) == 1 {
		result.StructuredContent = structured[0]
	} else {
		result.StructuredContent = structured
	}
	return result
}

// applyTotalCap truncates the tail of the reply to the caller's total budget.
//
// A slot that crosses the cap is cut to exactly what fits and reports 206 with a
// Content-Range describing what it DID return; every slot after it is reduced to
// its headers. That is the same "fold, never drop" rule the per-slot windows use:
// the bytes are still in the log, and the range says where to continue.
func (t termReply) applyTotalCap(slots []envelope.Slot) []envelope.Slot {
	total := 0
	for i, slot := range slots {
		if total >= t.win.TotalBytesLimit {
			slot.Body = ""
			if slot.Code < 400 {
				slot.Code = 206
			}
			slots[i] = slot
			continue
		}
		remaining := t.win.TotalBytesLimit - total
		if len(slot.Body) > remaining {
			// Cut, don't just truncate: Content-Range has to describe the bytes the
			// body still carries, or the caller's next read starts past the gap.
			slot = slot.Cut(remaining)
			if slot.Code < 400 {
				slot.Code = 206
			}
		}
		total += len(slot.Body)
		slots[i] = slot
	}
	return slots
}

// termSlotResult wraps one slot as a tool result.
func termSlotResult(slot envelope.Slot) *mcpgo.CallToolResult {
	return termReply{win: defaultTermArgs(), slots: []envelope.Slot{slot}}.result()
}

// defaultTermArgs is the window used when a reply is built outside a call (an
// internal error path); it matches the schema's defaults so the two cannot write
// different obligations.
func defaultTermArgs() termArgs {
	return termArgs{
		TimeoutSeconds:  defaultTimeoutSeconds,
		BytesLimit:      defaultBytesLimit,
		LinesLimit:      defaultLinesLimit,
		TotalBytesLimit: defaultTotalBytesLimit,
		OnTimeOverflow:  "truncate",
		OnBytesOverflow: "truncate",
		OnLinesOverflow: "truncate",
	}
}

// withUsage adds the directive's usage line when learn_mode asked for it. It is
// a method on the accumulating reply so the "same directive once" rule has one
// home.
func (t *termReply) withUsage(stmt directive.Statement, headers map[string]string) {
	if !t.win.LearnMode || t.seenUsage[stmt.Op] {
		return
	}
	if op, ok := directive.Lookup(stmt.Op); ok && op.Usage != "" {
		headers["Usage"] = op.Usage
		t.seenUsage[stmt.Op] = true
	}
}

// atoiFlag reads a numeric flag, returning the fallback when absent or unusable.
func atoiFlag(stmt directive.Statement, key string, fallback int) int {
	raw, ok := stmt.Flags[key]
	if !ok || raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}

// boolFlag reports whether a valueless flag was given.
func boolFlag(stmt directive.Statement, key string) bool {
	_, ok := stmt.Flags[key]
	return ok
}

// int64Flag reads a 64-bit numeric flag.
func int64Flag(stmt directive.Statement, key string, fallback int64) int64 {
	raw, ok := stmt.Flags[key]
	if !ok || raw == "" {
		return fallback
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}

// argsOf is the compact way to hand a directive onto an existing handler: the
// handler keeps its own argument names, which is what makes the single entry a
// re-rendering of the existing surface rather than a reimplementation of it.
func argsOf(pairs ...any) map[string]any {
	if len(pairs)%2 != 0 {
		panic("argsOf needs key/value pairs")
	}
	out := make(map[string]any, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		key, ok := pairs[i].(string)
		if !ok {
			panic("argsOf keys are strings")
		}
		out[key] = pairs[i+1]
	}
	return out
}

// callTool invokes one of the existing handlers through the same admission path
// the MCP server uses, and renders its result as one slot.
//
// This is the whole trick behind the single entry: the directives do not
// reimplement anything. They translate a command line into the arguments the
// specialized handler already takes, and the reply is re-rendered as an HTTP
// response. So `run` is not a second implementation of shell_input + shell_key +
// shell_output; it is those calls, in order, with one reply.
// callTool invokes one operation and reports BOTH kinds of failure the same way:
// a transport error and an isError result.
//
// The isError half is not a convenience. Admission refusals (namespace isolation,
// a template's capability list) come back as an isError result, not as an error,
// and a directive that only inspected the error would report the refusal as a
// 204 success — the operation never ran, and the caller was told it did. Every
// directive routes through here for exactly that reason.
func (s *Server) callTool(ctx context.Context, tool string, args map[string]any) (*mcpgo.CallToolResult, *toolFailure) {
	h := s.handlerFor(tool)
	if h == nil {
		return nil, &toolFailure{code: "unknown_directive", msg: "no handler for " + tool}
	}
	req := mcpgo.CallToolRequest{}
	req.Params.Name = tool
	req.Params.Arguments = args
	res, err := withLogging(tool, h, s)(ctx, req)
	if err != nil {
		return nil, &toolFailure{code: CodeOperationFailed, msg: err.Error()}
	}
	if res != nil && res.IsError {
		return res, failureFromResult(res)
	}
	return res, nil
}

// toolFailure is a handler failure translated into envelope terms.
type toolFailure struct {
	code string
	msg  string
}

// statusFor maps a tool error code onto the HTTP status that describes it.
//
// The mapping is by FAILURE KIND, not by tool: "the session is gone" is a 404 no
// matter which directive asked, and "the whitelist says no" is a 403 no matter
// which entrance was used.
func statusFor(code string) int {
	switch code {
	case CodeForbidden:
		return 403
	case CodeSessionNotFound, CodeShellNotFound, CodeForwardNotFound,
		CodeSSHConfigNotFound, CodeRuleNotFound, CodeHistoryNotFound,
		CodeReaderNotRegistered:
		return 404
	case CodeConflict, CodeSessionNotRunning:
		return 409
	case CodeInvalidArgument:
		return 400
	case CodeNotConfigured:
		return 422
	}
	return 500
}

// failureSlot renders a handler failure.
func failureSlot(f *toolFailure) envelope.Slot {
	if f == nil {
		return envelope.Slot{Code: 500, Body: "the directive failed without a reason"}
	}
	return envelope.Slot{
		Code: statusFor(f.code),
		Headers: map[string]string{
			"Error-Code": f.code,
			"Idempotent": "no",
		},
		Body: f.msg,
	}
}

// jsonSlot renders a JSON-producing handler's reply as one slot's body.
//
// It is used for the directives whose existing reply is a document (a session's
// info, a forward list). The generated-text directives (ps, help, ext list) do
// not go through here: their bodies are terminal columns, because a model reads
// `a1f3 running pipe go-build 12s` faster than the same facts as nested JSON.
func jsonSlot(code int, headers map[string]string, m map[string]any) envelope.Slot {
	body, _ := jsonMarshalIndent(m)
	if headers == nil {
		headers = map[string]string{}
	}
	headers["Content-Type"] = "application/json"
	return envelope.Slot{Code: code, Headers: headers, Body: body}
}

// fmtBytes renders a byte count for a generated body.
func fmtBytes(n int) string { return fmt.Sprintf("%d B", n) }

// termDescription is the whole model-facing description of the tool.
//
// It deliberately names no directive: the semantics live in `cmd`'s syntax and in
// `help`/`learn_mode`, not in the tool's name or blurb. That is the difference
// between a tool surface that can grow and one that cannot — every directive this
// text listed would be a line to edit the day the command space gains one.
const termDescription = "Run one terminal line on this instance and read its result: a session's bytes, a file, a listing, a directive's own output. Pass no cmd to get your bearings (what is running, what is installed, the syntax). The window parameters bound how much comes back, never what the command does."
