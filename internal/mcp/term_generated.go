package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/clock"
	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/envelope"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// Generated replies.
//
// `ps`, `info`, `help` and the extension/profile listings are NOT byte streams:
// there is nothing to continue reading, so they carry no Content-Range and their
// body is terminal text — one line per object, columns, `key: value`. That is not
// cosmetic. A model reads `a1f3 running pipe go-build 12s` faster than the same
// facts nested in JSON, `grep`/`cut` work on it, and the handle in the first column
// can be copied straight into the next command. The format requirement that
// matters is that last one: every handle in `ps` must be usable verbatim as the
// target of another directive.

// directivePS lists what is running, one line per channel.
func (s *Server) directivePS(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	headers := map[string]string{"Path": stmt.Path}

	sessions := s.sessMgr.ListAll()
	callerNS := callerNamespace(ctx)
	if !boolFlag(stmt, "all") {
		sessions = listableSessions(sessions, callerNS)
	}

	var b strings.Builder
	for _, sess := range sessions {
		live := s.sessMgr.Get(sess.ID)
		if live == nil {
			continue
		}
		shells := live.ListChildShells()
		if len(shells) == 0 {
			b.WriteString(fmt.Sprintf("%s\t%s\t%s\t%s\n",
				sess.ID, sess.Status, dash(sess.SSHConfig), ageOf(sess.CreatedAt)))
			continue
		}
		for _, sh := range shells {
			if !boolFlag(stmt, "shells") && len(shells) > 1 && sh.Index > 1 {
				// Without --shells, one line per session: the primary channel names
				// the session, and the extra channels are one flag away. Listing all
				// of them by default would make a fleet's ps unreadable.
				continue
			}
			b.WriteString(fmt.Sprintf("%s\t%s\t%s\t%s\t%s\n",
				handleOf(sess.ID, sh.Index), sh.Status, dash(string(sh.Mode)),
				dash(sh.Name), ageOf(sh.CreatedAt)))
		}
	}
	body := b.String()
	if body == "" {
		body = "(nothing is running)\n"
	}
	// A generated reply is capped by the same window as a byte stream: a listing is
	// exactly as capable of flooding a context as a log is, and the directives with
	// no stream to continue are the ones with no other protection.
	code := 200
	if capped, cut := capGenerated(body, win); cut {
		body = capped
		code = 206
	}
	return envelope.Slot{Code: code, Headers: headers, Body: body}
}

// directiveInfo describes one session or channel as `key: value` lines.
func (s *Server) directiveInfo(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	sess, bad := s.requireSession(stmt.Target)
	if bad != nil {
		return s.slotFromFailure(ctx, stmt, failureFromResult(bad))
	}
	info := sess.Info()
	var b strings.Builder
	writeKV(&b, "id", info.ID)
	writeKV(&b, "name", dash(info.Name))
	writeKV(&b, "status", string(info.Status))
	writeKV(&b, "ssh_config", dash(info.SSHConfig))
	writeKV(&b, "endpoint", dash(info.SSHEndpoint))
	if info.ExitCode != nil {
		writeKV(&b, "exit_code", fmt.Sprint(*info.ExitCode))
	}
	writeKV(&b, "created", ageOf(info.CreatedAt))
	writeKV(&b, "approval", approvalSummaryText(info))
	for _, sh := range sess.ListChildShells() {
		writeKV(&b, "shell", fmt.Sprintf("%s %s %s %s",
			handleOf(info.ID, sh.Index), sh.Status, dash(string(sh.Mode)), dash(sh.Name)))
	}
	code := 200
	body := b.String()
	if capped, cut := capGenerated(body, win); cut {
		body = capped
		code = 206
	}
	return envelope.Slot{Code: code, Headers: map[string]string{"Path": stmt.Path}, Body: body}
}

// directiveResize changes a pty's size.
func (s *Server) directiveResize(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	if len(stmt.Args) < 2 {
		return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "invalid_argument"},
			Body: "resize needs rows and cols: `resize a1f3:1 40 120`"}
	}
	res, fail := s.callTool(ctx, "shell_resize", argsOf(
		"shell_id", stmt.Target,
		"rows", flagNumber(stmt.Args[0]),
		"cols", flagNumber(stmt.Args[1]),
	))
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	_ = res
	return envelope.Slot{Code: 204, Headers: map[string]string{"Path": stmt.Path}}
}

// directiveClose closes a channel or a whole session.
func (s *Server) directiveClose(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	args := argsOf("session_id", stmt.Target)
	if boolFlag(stmt, "force") {
		args["force"] = true
	}
	if v, ok := stmt.Flags["grace"]; ok {
		args["grace_period"] = flagNumber(v)
	}
	// A handle names a channel; a bare session id names the container. Both
	// spellings are accepted by the existing handlers, so `close` tries the
	// channel first and falls back to the session, which is what a caller means
	// either way.
	if _, bad := s.requireShell(stmt.Target); bad == nil {
		res, fail := s.callTool(ctx, "shell_close", argsOf("shell_id", stmt.Target))
		if fail != nil {
			return s.slotFromFailure(ctx, stmt, fail)
		}
		_ = res
		return envelope.Slot{Code: 204, Headers: map[string]string{"Path": stmt.Path}}
	}
	res, fail := s.callTool(ctx, "session_terminate", args)
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	_ = res
	return envelope.Slot{Code: 204, Headers: map[string]string{"Path": stmt.Path}}
}

// directiveRemove deletes a session and its logs.
func (s *Server) directiveRemove(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	res, fail := s.callTool(ctx, "session_delete", argsOf("session_id", stmt.Target))
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	_ = res
	return envelope.Slot{Code: 204, Headers: map[string]string{"Path": stmt.Path}}
}

// directiveSpan lists a channel's transcript spans.
func (s *Server) directiveSpan(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	res, fail := s.callTool(ctx, "message", argsOf("action", "list", "shell_id", stmt.Target))
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	doc, ok := toolResultMap(res)
	if !ok {
		return s.slotFromFailure(ctx, stmt, &toolFailure{code: CodeOperationFailed, msg: toolResultText(res)})
	}
	// One span per line: status, time, byte range, line range. Terminal columns,
	// because a span list is something a caller scans and greps, not a document it
	// parses field by field.
	var b strings.Builder
	spans, _ := doc["spans"].([]any)
	for _, raw := range spans {
		span, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		b.WriteString(fmt.Sprintf("%s\t%d\t%d-%d\tL%d-%d\n",
			getString(span, "s", ""),
			int64(getFloatAny(span, "t", 0)),
			int64(getFloatAny(span, "start", 0)), int64(getFloatAny(span, "end", 0)),
			int64(getFloatAny(span, "from_line", 0)), int64(getFloatAny(span, "to_line", 0))))
	}
	return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: b.String()}
}

// directiveReader registers or drops an output reader.
func (s *Server) directiveReader(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	if v, ok := stmt.Flags["drop"]; ok {
		res, fail := s.callTool(ctx, "shell_reader_unregister", argsOf(
			"shell_id", stmt.Target, "reader_id", flagNumber(v)))
		if fail != nil {
			return s.slotFromFailure(ctx, stmt, fail)
		}
		_ = res
		return envelope.Slot{Code: 204, Headers: map[string]string{"Path": stmt.Path}}
	}
	res, fail := s.callTool(ctx, "shell_reader_register", argsOf("shell_id", stmt.Target))
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	doc, ok := toolResultMap(res)
	if !ok {
		return s.slotFromFailure(ctx, stmt, &toolFailure{code: CodeOperationFailed, msg: toolResultText(res)})
	}
	// The new reader id IS news: the caller did not know it before this call.
	return envelope.Slot{
		Code:    200,
		Headers: map[string]string{"Path": stmt.Path, "Handle": stmt.Target},
		Body:    fmt.Sprintf("reader %d\n", int64(getFloatAny(doc, "reader_id", 0))),
	}
}

// directiveWatch manages wake-up rules.
func (s *Server) directiveWatch(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	args := map[string]any{}
	switch {
	case boolFlag(stmt, "list"):
		args["action"] = "list"
	case stmt.Flags["off"] != "":
		args["action"] = "unregister"
		args["rule_id"] = stmt.Flags["off"]
	default:
		args["action"] = "register"
		args["shell_id"] = stmt.Target
		if v, ok := stmt.Flags["event"]; ok {
			args["event"] = v
		}
		if v, ok := stmt.Flags["for"]; ok {
			args["silence_seconds"] = flagNumber(v)
		}
		if v, ok := stmt.Flags["channel"]; ok {
			args["channel"] = v
		}
	}
	res, fail := s.callTool(ctx, "shell_notify", args)
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	doc, ok := toolResultMap(res)
	if !ok {
		return s.slotFromFailure(ctx, stmt, &toolFailure{code: CodeOperationFailed, msg: toolResultText(res)})
	}
	return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: compactDocument(doc)}
}

// directiveNotify posts a notification to the human.
func (s *Server) directiveNotify(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	args := argsOf("message", stmt.Payload)
	if v, ok := stmt.Flags["level"]; ok {
		args["level"] = v
	}
	if v, ok := stmt.Flags["title"]; ok {
		args["title"] = v
	}
	if v, ok := stmt.Flags["on"]; ok {
		args["session_id"] = v
	}
	if boolFlag(stmt, "sticky") {
		args["duration_seconds"] = float64(0)
	} else if v, ok := stmt.Flags["for"]; ok {
		args["duration_seconds"] = flagNumber(v)
	}
	res, fail := s.callTool(ctx, "notify_user", args)
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	doc, _ := toolResultMap(res)
	return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: compactDocument(doc)}
}

// directiveDetect probes the termcp host for a shell.
func (s *Server) directiveDetect(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	res, fail := s.callTool(ctx, "shell_detect", map[string]any{})
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	doc, _ := toolResultMap(res)
	return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: compactDocument(doc)}
}

// directiveForward maps `fwd` onto the forward tool.
func (s *Server) directiveForward(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	args := map[string]any{}
	action := stmt.Flags["action"]
	if action == "" {
		action = "list"
	}
	args["action"] = action
	setFlagString(args, stmt, "session", "session_id")
	if stmt.Target != "" {
		args["session_id"] = stmt.Target
	}
	setFlagString(args, stmt, "remote-host", "remote_host")
	setFlagNumber(args, stmt, "remote-port", "remote_port")
	setFlagNumber(args, stmt, "local-port", "local_port")
	setFlagString(args, stmt, "local-host", "local_host")
	setFlagString(args, stmt, "forward-id", "forward_id")

	// The capability is decided by the action: a listing is a read, opening is a
	// reviewed change, closing is a change. Asking the table here (rather than
	// gating the whole directive) is what keeps `fwd --action list` out of the
	// review queue.
	if slot, refused := s.refuseAction(ctx, stmt, forwardCapability(action)); refused {
		return slot
	}

	res, fail := s.callTool(ctx, "forward", args)
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	if held := reviewPendingFrom(res); held != nil {
		return envelope.Slot{Code: 202, Headers: map[string]string{"Path": stmt.Path}, Body: *held}
	}
	doc, _ := toolResultMap(res)
	return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: compactDocument(doc)}
}

// forwardCapability maps a forward action onto its capability.
func forwardCapability(action string) string {
	switch action {
	case "list":
		return capability.ForwardList
	case "close":
		return capability.ForwardClose
	}
	return capability.ForwardOpen
}

// directiveFile maps `file` onto the file tools, one per action.
func (s *Server) directiveFile(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	action := stmt.Flags["action"]
	tool, capID, ok := fileToolForAction(action)
	if !ok {
		return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "invalid_argument"},
			Body: "file needs --action read|write|stat|delete|rename|mkdir|urls|perm|link|fs|getwd, got " + quoteOrDash(action)}
	}
	if slot, refused := s.refuseAction(ctx, stmt, capID); refused {
		return slot
	}

	args := argsOf("session_id", stmt.Target)
	setFlagString(args, stmt, "remote-path", "remote_path")
	setFlagString(args, stmt, "from-path", "from_path")
	setFlagString(args, stmt, "to-path", "to_path")
	setFlagString(args, stmt, "local-path", "local_path")
	setFlagString(args, stmt, "data", "data")
	setFlagString(args, stmt, "mode", "mode")
	setFlagString(args, stmt, "target", "target")
	setFlagString(args, stmt, "link-path", "link_path")
	setFlagString(args, stmt, "existing-path", "existing_path")
	setFlagString(args, stmt, "new-path", "new_path")
	setFlagNumber(args, stmt, "offset", "offset")
	setFlagNumber(args, stmt, "length", "length")
	setFlagNumber(args, stmt, "local-offset", "local_offset")
	setFlagNumber(args, stmt, "uid", "uid")
	setFlagNumber(args, stmt, "gid", "gid")
	setFlagNumber(args, stmt, "atime", "atime")
	setFlagNumber(args, stmt, "mtime", "mtime")
	setFlagNumber(args, stmt, "size", "size")
	if action != "read" && action != "write" && action != "getwd" {
		// The actions that dispatch on their own action field take it verbatim.
		switch action {
		case "perm", "link", "fs":
			args["action"] = stmt.Flags["action-verb"]
			if args["action"] == nil {
				args["action"] = stmt.Flags["op"]
			}
		}
	}

	res, fail := s.callTool(ctx, tool, args)
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	if held := reviewPendingFrom(res); held != nil {
		return envelope.Slot{Code: 202, Headers: map[string]string{"Path": stmt.Path}, Body: *held}
	}
	doc, ok := toolResultMap(res)
	if !ok {
		text := toolResultText(res)
		return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: text}
	}
	return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: compactDocument(doc)}
}

// fileToolForAction maps a `file --action` onto the tool that implements it and
// the capability it needs.
func fileToolForAction(action string) (tool, capID string, ok bool) {
	switch action {
	case "read":
		return "file_read", capability.FileRead, true
	case "write":
		return "file_write", capability.FileWrite, true
	case "stat":
		return "file_stat", capability.FileStat, true
	case "delete":
		return "file_delete", capability.FileDelete, true
	case "rename":
		return "file_rename", capability.FileRename, true
	case "mkdir":
		return "file_mkdir", capability.FileMkdir, true
	case "urls":
		return "file_urls", capability.FileURLs, true
	case "perm":
		return "file_perm", capability.FilePerm, true
	case "link":
		return "file_link", capability.FileLink, true
	case "fs":
		return "file_fs", capability.FileFs, true
	case "getwd":
		return "file_getwd", capability.FileGetwd, true
	}
	return "", "", false
}

// directiveProfile maps `profile` onto ssh_config.
func (s *Server) directiveProfile(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	action := stmt.Flags["action"]
	if action == "" {
		action = "list"
	}
	capID, ok := profileCapability(action)
	if !ok {
		return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "invalid_argument"},
			Body: "profile needs --action list|create|edit|copy|delete, got " + quoteOrDash(action)}
	}
	if slot, refused := s.refuseAction(ctx, stmt, capID); refused {
		return slot
	}
	args := map[string]any{"action": action}
	setFlagString(args, stmt, "name", "name")
	setFlagString(args, stmt, "host", "host")
	setFlagString(args, stmt, "user", "user")
	setFlagNumber(args, stmt, "port", "port")
	setFlagString(args, stmt, "password", "password")
	setFlagString(args, stmt, "private-key", "private_key")
	setFlagString(args, stmt, "key-passphrase", "key_passphrase")
	setFlagString(args, stmt, "known-hosts", "known_hosts")
	setFlagString(args, stmt, "proxy", "proxy")
	setFlagString(args, stmt, "description", "description")
	setFlagString(args, stmt, "default-shell", "default_shell")
	setFlagString(args, stmt, "default-mode", "default_mode")
	setFlagString(args, stmt, "jump-host", "jump_host")
	setFlagString(args, stmt, "jump-user", "jump_user")
	setFlagNumber(args, stmt, "jump-port", "jump_port")
	setFlagNumber(args, stmt, "dial-timeout-seconds", "dial_timeout_seconds")
	setFlagString(args, stmt, "source", "source")
	if boolFlag(stmt, "trust-unknown-host") {
		args["trust_unknown_host"] = true
	}
	if boolFlag(stmt, "default-approval") {
		args["default_approval"] = true
	}
	res, fail := s.callTool(ctx, "ssh_config", args)
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	if held := reviewPendingFrom(res); held != nil {
		return envelope.Slot{Code: 202, Headers: map[string]string{"Path": stmt.Path}, Body: *held}
	}
	if action == "list" {
		// The profile listing is a set of names, one per line: copy-pasteable into
		// the next `pty`/`pipe`.
		return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: listFromDoc(res, "configs", "name")}
	}
	doc, _ := toolResultMap(res)
	return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: compactDocument(doc)}
}

// profileCapability maps a profile action onto its capability.
func profileCapability(action string) (string, bool) {
	switch action {
	case "list":
		return capability.ProfileList, true
	case "create":
		return capability.ProfileCreate, true
	case "edit":
		return capability.ProfileEdit, true
	case "copy":
		return capability.ProfileCopy, true
	case "delete":
		return capability.ProfileDelete, true
	}
	return "", false
}

// directiveExtension maps `ext` onto the extension tool.
func (s *Server) directiveExtension(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	action := stmt.Flags["action"]
	if action == "" {
		action = "list"
	}
	capID, ok := extensionCapability(action)
	if !ok {
		return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "invalid_argument"},
			Body: "ext needs --action list|get|create|update|delete|retry, got " + quoteOrDash(action)}
	}
	if slot, refused := s.refuseAction(ctx, stmt, capID); refused {
		return slot
	}
	args := map[string]any{"action": action}
	setFlagString(args, stmt, "name", "name")
	setFlagString(args, stmt, "kind", "kind")
	setFlagString(args, stmt, "target", "target")
	setFlagString(args, stmt, "description", "description")
	setFlagString(args, stmt, "command", "command")
	setFlagString(args, stmt, "url", "url")
	setFlagString(args, stmt, "mode", "mode")
	setFlagString(args, stmt, "notes", "notes")
	setFlagNumber(args, stmt, "rows", "rows")
	setFlagNumber(args, stmt, "cols", "cols")
	setFlagList(args, stmt, "args", "args")
	setFlagList(args, stmt, "allow", "allow")
	setFlagList(args, stmt, "paths", "paths")
	if boolFlag(stmt, "enabled") {
		args["enabled"] = true
	}
	res, fail := s.callTool(ctx, "extension", args)
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	if held := reviewPendingFrom(res); held != nil {
		return envelope.Slot{Code: 202, Headers: map[string]string{"Path": stmt.Path}, Body: *held}
	}
	if action == "list" {
		// The boot manifest: one extension per line, with its state and handle, so
		// a caller that lost its context gets its toolset back in one call.
		return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: extensionListing(res)}
	}
	doc, _ := toolResultMap(res)
	return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: compactDocument(doc)}
}

// extensionCapability maps an extension action onto its capability.
func extensionCapability(action string) (string, bool) {
	switch action {
	case "list":
		return capability.ExtList, true
	case "get":
		return capability.ExtGet, true
	case "create":
		return capability.ExtCreate, true
	case "update":
		return capability.ExtUpdate, true
	case "delete":
		return capability.ExtDelete, true
	case "retry":
		return capability.ExtRetry, true
	}
	return "", false
}

// refuseAction applies a capability decision to a directive whose capability came
// from a flag.
func (s *Server) refuseAction(ctx context.Context, stmt directive.Statement, capID string) (envelope.Slot, bool) {
	if capID == "" {
		return envelope.Slot{}, false
	}
	sess := s.sessionForStatement(stmt)
	if sess == nil {
		return envelope.Slot{}, false
	}
	if sess.Restriction().Allows(capID) {
		return envelope.Slot{}, false
	}
	return envelope.Slot{
		Code: 403,
		Headers: map[string]string{
			"Path":       stmt.Path,
			"Error-Code": CodeForbidden,
		},
		Body: "this session's template does not grant " + capID,
	}, true
}

// directiveMCP handles the JSON-RPC entry point.
func (s *Server) directiveMCP(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	// `mcp open` relays an HTTP endpoint. It does not consume a shell channel: the
	// bridge speaks HTTP, so a caller can hold a dozen of them without spending
	// the shell budget.
	words := strings.Fields(stmt.Payload)
	if len(words) == 0 {
		return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "invalid_argument"},
			Body: "mcp needs a subcommand: open|tools|send|recv|call|close"}
	}
	sub := words[0]
	switch sub {
	case "open":
		if len(words) < 2 {
			return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "invalid_argument"},
				Body: "mcp open needs a URL"}
		}
		return s.mcpOpen(ctx, stmt, words[1])
	case "tools", "send", "recv", "call", "close":
		return s.mcpChannel(ctx, stmt, sub, words[1:])
	}
	return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "invalid_argument"},
		Body: "unknown mcp subcommand: " + sub}
}

// directiveHelp prints the directive table, or one directive's syntax.
func (s *Server) directiveHelp(stmt directive.Statement, win termArgs) envelope.Slot {
	if len(stmt.Args) > 0 {
		op, ok := directive.Lookup(stmt.Args[0])
		if !ok {
			return envelope.Slot{Code: 404, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "unknown_directive"},
				Body: "unknown directive: " + stmt.Args[0]}
		}
		body := op.Usage + "\n"
		if op.Detail != "" {
			// The detail is a page, so it follows the one-line template instead of
			// replacing it: the template is what a caller scans, the page is what it
			// reads once it has decided to produce the thing.
			body += "\n" + op.Detail + "\n"
		}
		capped, _ := capGenerated(body, win)
		return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: capped}
	}
	var b strings.Builder
	for _, op := range directive.Ops() {
		b.WriteString(op.Usage)
		b.WriteByte('\n')
	}
	return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: b.String()}
}

// termGuide is the empty-call answer: where am I, what can I do, and what is here.
//
// A caller that has just been woken up knows nothing, so this call has to be legal,
// cheap, and dense — never an error. It is the first move of every recovery, and
// the recovery path must not depend on remembering anything.
func (s *Server) termGuide(ctx context.Context, win termArgs) (*mcpgo.CallToolResult, error) {
	var b strings.Builder
	b.WriteString("termcp terminal. Syntax:\n")
	b.WriteString("  pty internal                                  open an interactive shell  -> Handle\n")
	b.WriteString("  pipe internal --argv sh -c 'echo hi'          run a program to exit      -> Handle\n")
	b.WriteString("  run a1f3:1 go build ./...                     one command, one call\n")
	b.WriteString("  read a1f3:1 / ps / info a1f3:1                read, list, inspect\n")
	b.WriteString("  help / help <directive>                       the full directive table\n")
	b.WriteString("Handles are <session>[:N]; copy one from ps or ext list into the next command.\n")
	b.WriteString("Every reply is TERM/1 <code>: 2xx ok, 4xx bad request, 5xx the command failed.\n\n")

	// What is running right now, in the same columns `ps` prints: the recovery path
	// is "ask, then copy a handle", not "ask, then translate".
	ps := s.directivePS(ctx, directive.Statement{Op: "ps", Path: "ps"}, win)
	b.WriteString("running now:\n")
	if strings.TrimSpace(ps.Body) == "" {
		b.WriteString("  (nothing)\n")
	} else {
		for _, line := range strings.Split(strings.TrimRight(ps.Body, "\n"), "\n") {
			b.WriteString("  " + line + "\n")
		}
	}
	return termSlotResult(envelope.Slot{Code: 200, Body: b.String()}), nil
}

// --- small helpers ---

// dash renders an empty field as a placeholder, so columns stay aligned.
func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// quoteOrDash renders a flag value for an error message.
func quoteOrDash(s string) string {
	if s == "" {
		return "(missing)"
	}
	return fmt.Sprintf("%q", s)
}

// handleOf renders a channel's short handle: the session id, plus :N for a
// channel that is not the first. The termcp:// prefix is omitted everywhere in
// replies (nine characters per slot); it is still accepted as INPUT, because the
// Web UI's copy button produces it.
func handleOf(sessionID string, index int) string {
	if index <= 1 {
		return sessionID
	}
	return fmt.Sprintf("%s:%d", sessionID, index)
}

// ageOf renders how long ago a timestamp was, in terminal units.
func ageOf(unixMilli int64) string {
	if unixMilli == 0 {
		return "-"
	}
	secs := (clock.Now() - unixMilli) / 1000
	if secs < 0 {
		secs = 0
	}
	switch {
	case secs < 60:
		return fmt.Sprintf("%ds", secs)
	case secs < 3600:
		return fmt.Sprintf("%dm", secs/60)
	default:
		return fmt.Sprintf("%dh", secs/3600)
	}
}

// writeKV writes one `key: value` line.
func writeKV(b *strings.Builder, key, value string) {
	b.WriteString(key)
	b.WriteString(": ")
	b.WriteString(value)
	b.WriteByte('\n')
}

// approvalSummaryText renders a session's review state in one field.
func approvalSummaryText(info api.Session) string {
	if !info.ApprovalMode {
		return "off"
	}
	scopes := info.ApprovalScopes
	if len(scopes) == 0 {
		return fmt.Sprintf("on (%d approver, all operations)", info.ApprovalNeed)
	}
	return fmt.Sprintf("on (%d approver, %s)", info.ApprovalNeed, strings.Join(scopes, ","))
}

// compactDocument renders a handler's JSON reply as `key: value` lines, one level
// deep. Nested structures are rendered as their JSON text, which keeps a generated
// reply readable without inventing a second rendering per document.
func compactDocument(doc map[string]any) string {
	if len(doc) == 0 {
		return ""
	}
	keys := make([]string, 0, len(doc))
	for k := range doc {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString(": ")
		switch v := doc[k].(type) {
		case string:
			b.WriteString(v)
		case nil:
			b.WriteString("-")
		case bool:
			b.WriteString(fmt.Sprint(v))
		case float64:
			if v == float64(int64(v)) {
				b.WriteString(fmt.Sprintf("%d", int64(v)))
			} else {
				b.WriteString(fmt.Sprint(v))
			}
		default:
			raw, _ := jsonMarshalIndent(map[string]any{"v": v})
			b.WriteString(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(raw, "{\n  \"v\": "), "\n}")))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// listFromDoc renders a named list inside a handler's JSON reply as one entry per
// line: the shape a caller copies into the next command.
func listFromDoc(res *mcpgo.CallToolResult, listKey, field string) string {
	doc, ok := toolResultMap(res)
	if !ok {
		return toolResultText(res)
	}
	items, _ := doc[listKey].([]any)
	var b strings.Builder
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		b.WriteString(getString(item, field, ""))
		if desc := getString(item, "description", ""); desc != "" {
			b.WriteString("\t" + desc)
		}
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		return "(none)\n"
	}
	return b.String()
}

// extensionListing renders `ext list` as the boot manifest: name, state, handle,
// and a one-line note. This is the call that gives a caller its toolset back after
// its context was cleared, so it must be one call and must be scannable.
func extensionListing(res *mcpgo.CallToolResult) string {
	doc, ok := toolResultMap(res)
	if !ok {
		return toolResultText(res)
	}
	items, _ := doc["extensions"].([]any)
	var b strings.Builder
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := getString(item, "name", "")
		state := getString(item, "status", "")
		handle := getString(item, "session_id", "")
		if shell := getString(item, "shell_id", ""); shell != "" {
			handle = shell
		}
		notes := strings.ReplaceAll(getString(item, "notes", ""), "\n", " ")
		if len(notes) > 60 {
			notes = notes[:60] + "…"
		}
		b.WriteString(fmt.Sprintf("%s\t%s\t%s\t%s\n", name, dash(state), dash(handle), notes))
	}
	if b.Len() == 0 {
		return "(no extensions installed)\n"
	}
	return b.String()
}

// capGenerated applies the byte and line windows to a generated reply, in that
// order, and reports whether it had to cut.
func capGenerated(body string, win termArgs) (string, bool) {
	cut := false
	if win.LinesLimit > 0 {
		if lines := strings.Count(body, "\n"); lines > win.LinesLimit {
			body = foldOrTruncateLines(body, win.LinesLimit, win.OnLinesOverflow)
			cut = true
		}
	}
	if win.BytesLimit > 0 && len(body) > win.BytesLimit {
		body = foldOrTruncate(body, win.BytesLimit, win.OnBytesOverflow)
		cut = true
	}
	return body, cut
}

// setFlagString copies a string flag into a handler argument.
func setFlagString(args map[string]any, stmt directive.Statement, flag, arg string) {
	if v, ok := stmt.Flags[flag]; ok && v != "" {
		args[arg] = v
	}
}

// setFlagNumber copies a numeric flag into a handler argument.
func setFlagNumber(args map[string]any, stmt directive.Statement, flag, arg string) {
	if v, ok := stmt.Flags[flag]; ok && v != "" {
		args[arg] = flagNumber(v)
	}
}

// setFlagList copies a comma-separated flag into a handler's array argument.
func setFlagList(args map[string]any, stmt directive.Statement, flag, arg string) {
	v, ok := stmt.Flags[flag]
	if !ok || v == "" {
		return
	}
	parts := strings.Split(v, ",")
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	args[arg] = out
}
