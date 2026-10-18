package mcp

import (
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// registerTools declares every MCP tool on the server. It is the whole tool
// surface in one place: name, description, arguments and the handler each call
// dispatches to. Split out of New so the surrounding function reads as
// assembly (managers, transports) rather than as a 230-line table.
func registerTools(mcpServer *mcpserver.MCPServer, s *Server) {
	mcpServer.AddTool(s.newTool("session_start",
		mcpgo.WithDescription("Start a session (connection container) plus its primary shell. ssh_config REQUIRED: a profile name from ssh_config(action=list), \"internal\" for a local terminal on the termcp host itself (no SSH), or a termcp:// entry locator pasted by the user (e.g. \"termcp://mac\" — parsed directly, no lookup needed). Empty command/args = the profile's default_shell, else the target's own login shell (pty), else an error (pipe). WARNING: command/args = single run-and-exit program; for multi-step or stateful work omit them and drive an interactive shell instead. Returns session_id and shell_id."),
		mcpgo.WithString("command", mcpgo.Description("Executable line; empty with no args = login shell / profile default_shell")),
		mcpgo.WithArray("args", mcpgo.Description("Argv after command"), mcpgo.WithStringItems()),
		mcpgo.WithString("mode", mcpgo.Description("Mode of the primary shell only: \"pty\" (default, interactive TUI; echoes input — Windows cannot disable this, line protocols use pipe) or \"pipe\" (no TTY, no echo; programmatic IO)."), mcpgo.DefaultString("pty")),
		mcpgo.WithString("on_exit", mcpgo.Description("What happens to the session when its last shell ends by itself: \"keep\" (default) leaves it running for reuse (forwards, SFTP, more shells); \"close\" terminates it, so a run-and-exit command's session archives itself instead of piling up in the running list. Its output stays readable either way."), mcpgo.DefaultString("keep"), mcpgo.Enum("keep", "close")),
		mcpgo.WithString("name"),
		mcpgo.WithNumber("rows", mcpgo.DefaultNumber(24)),
		mcpgo.WithNumber("cols", mcpgo.DefaultNumber(80)),
		mcpgo.WithString("ssh_config", mcpgo.Required(), mcpgo.Description("REQUIRED: \"internal\" = local terminal on the termcp host (no SSH), a profile name from ssh_config(action=list), or a termcp:// entry locator (e.g. \"termcp://mac\")")),
	), withLogging("session_start", s.handleStartSession, s))

	mcpServer.AddTool(s.newTool("shell_open",
		mcpgo.WithDescription("Open another shell channel on an existing session connection (reuses SSH transport). Returns shell_id for I/O and session_id of the parent. command+args run as argv (like session_start): a command string containing spaces is ONE executable name and fails — pass the extra tokens as args instead (e.g. command=\"node\", args=[\"app.js\",\"stream\"])."),
		mcpgo.WithString("session_id", mcpgo.Required()),
		mcpgo.WithString("name"),
		mcpgo.WithString("command", mcpgo.Description("Executable; empty = resolve down the chain: profile default_shell, then (pty only) the target's own login shell. pipe with an empty command and no default_shell is refused — there is no login shell to request on a pipe channel.")),
		mcpgo.WithArray("args", mcpgo.Description("Argv after command"), mcpgo.WithStringItems()),
		mcpgo.WithString("mode", mcpgo.Description("Mode for this shell channel: \"pty\" (default, interactive TUI; echoes input) or \"pipe\" (no TTY, no echo, run-to-exit; line protocols). Per shell — the session is only the connection."), mcpgo.DefaultString("pty")),
		mcpgo.WithNumber("rows", mcpgo.DefaultNumber(24)),
		mcpgo.WithNumber("cols", mcpgo.DefaultNumber(80)),
	), withLogging("shell_open", s.handleStartSubShell, s))

	mcpServer.AddTool(s.newTool("shell_list",
		mcpgo.WithDescription("List shell channels on a session: session_id + shells (id, name, status, timestamps). Use ids with shell_input/shell_key/shell_output/shell_close."),
		mcpgo.WithString("session_id", mcpgo.Required()),
	), withLogging("shell_list", s.handleListSubshells, s))

	mcpServer.AddTool(s.newTool("shell_close",
		mcpgo.WithDescription("Close one shell channel by shell_id without tearing down the session. For internal primary shell, close is a no-op (process outlives the tab). Use session_terminate to stop the whole session."),
		mcpgo.WithString("shell_id", mcpgo.Required()),
	), withLogging("shell_close", s.handleCloseShell, s))

	mcpServer.AddTool(s.newTool("shell_input",
		mcpgo.WithDescription("Write text bytes to a shell's stdin. Follow with shell_key(key=\"enter\") to execute the typed line, then shell_output for the result. shell_id accepts a raw id, a session id, or a termcp:// locator (\"termcp://#<sid>\" = primary shell, \"termcp://#<sid>:2\" = 2nd shell channel), or a comma-separated list (batch: type into many shells at once, one command to a fleet — each entry is attempted independently so one dead shell does not stop the rest, and the reply is {results:[{shell_id, ok, ...}]} with one outcome per entry; a single id keeps the flat reply). Returns {success, offset, total_lines, line}: offset is the stream end at submission — where this input's echo begins (a pipe shell records no echo; the zero-length a/i mark in message(action=list) lands there). line is the line the echo continues, 0 when it starts a fresh line (it becomes total_lines+1 once complete)."),
		mcpgo.WithString("shell_id", mcpgo.Required()),
		mcpgo.WithString("text", mcpgo.Required(), mcpgo.Description("UTF-8 text to write (no automatic newline)")),
	), withLogging("shell_input", s.handleSendInput, s))

	mcpServer.AddTool(s.newTool("shell_key",
		mcpgo.WithDescription("Send a named key to a shell. Supported: enter, tab, esc, up/down/left/right, backspace, delete, home, end, ctrl+c/d/z/l/u/w. Use enter after shell_input to run a command. shell_id accepts a raw id, a session id, a termcp:// locator (\"termcp://#<sid>\" or \"termcp://#<sid>:N\"), or a comma-separated list (batch: end many lines at once; the reply is {results:[...]}, one outcome per entry, and a single id keeps the flat reply)."),
		mcpgo.WithString("shell_id", mcpgo.Required()),
		mcpgo.WithString("key", mcpgo.Required(), mcpgo.Description("Named key (e.g. enter, ctrl+c, up)")),
		mcpgo.WithNumber("repeat", mcpgo.Description("Times to send the key (1–20)"), mcpgo.DefaultNumber(1)),
	), withLogging("shell_key", s.handlePressKey, s))

	mcpServer.AddTool(s.newTool("shell_output",
		mcpgo.WithDescription("Unified output reader for live AND closed (DEAD) shells with one byte-stream cursor model. shell_id may be a shell_id, a session_id, or a termcp:// locator (\"termcp://#<sid>\" = primary shell, \"termcp://#<sid>:N\" = Nth shell channel). Default: live = new output since the last read on reader_id (blocking up to timeout); closed = recent tail. tail_lines=N returns the last N lines; offset>=0 reads raw bytes from that position (stateless paging with has_more); from_line=N reads whole numbered lines from the persisted log (1-based, line numbers are absolute and never shift; the trailing partial line has no number).\n\nByte fields describe the RAW stream: start_offset + raw_bytes == end_offset always, and cursors advance by end_offset (or raw_bytes) — never by bytes_returned, which is the CLEANED text length and differs whenever strip_ansi removes anything. partial_tail_bytes > 0 means the window's last bytes have no '\\n' yet: a line-protocol consumer must hold them back until the line completes. stream=\"o\"/\"e\" filters the window by low-level producer (stdout/stderr; pipe channels only - a pty merges stderr at the source, so \"e\" returns nothing there): offsets always describe the raw window, only the returned content (bytes_returned) shrinks. Returns {output, has_more, lines_returned, bytes_returned, returned_bytes, raw_bytes, start_offset, end_offset, total_bytes, partial_tail_bytes, source, session_id, shell_id, session_status, session_uptime_seconds?}; from_line adds from_line/to_line/total_lines/line_bytes."),
		mcpgo.WithString("shell_id", mcpgo.Required(), mcpgo.Description("shell_id, session_id, or termcp:// locator (termcp://#<sid> / termcp://#<sid>:N)")),
		mcpgo.WithBoolean("strip_ansi", mcpgo.Description("If true, strip ANSI SGR/cursor escapes and compress terminal noise"), mcpgo.DefaultBool(true)),
		mcpgo.WithNumber("timeout", mcpgo.Description("Blocking wait for new output on LIVE shells, in seconds (0–60); 0 = non-blocking; ignored for closed-session reads"), mcpgo.DefaultNumber(3)),
		mcpgo.WithNumber("max_lines", mcpgo.Description("Return at most N newline-terminated lines (from the read window); 0 = no line limit"), mcpgo.DefaultNumber(0)),
		mcpgo.WithNumber("max_bytes", mcpgo.Description("Max raw bytes per call (from offset or tail); 0 = no limit. Use with start_offset/end_offset/has_more to paginate"), mcpgo.DefaultNumber(8192)),
		mcpgo.WithNumber("offset", mcpgo.Description("Raw byte position to start reading; -1 = reader cursor (live, default) / tail (closed)"), mcpgo.DefaultNumber(-1)),
		mcpgo.WithNumber("tail_lines", mcpgo.Description("Return only the last N lines of the stream (overrides offset); 0 = off"), mcpgo.DefaultNumber(0)),
		mcpgo.WithNumber("from_line", mcpgo.Description("Read whole lines from line N (1-based, absolute, from the persisted log; pairs with max_lines/max_bytes; exclusive of offset/tail_lines/reader_id)"), mcpgo.DefaultNumber(0)),
		mcpgo.WithString("stream", mcpgo.Description("Return \"o\"=stdout, \"e\"=stderr (pty: none), default \"all\"; offsets stay raw.")),
		mcpgo.WithNumber("reader_id", mcpgo.DefaultNumber(0)),
	), withLogging("shell_output", s.handleReadOutput, s))

	mcpServer.AddTool(s.newTool("session_list",
		mcpgo.WithDescription("Return metadata for every running parent session (exited ones are auto-removed). Child shells excluded — use shell_list. Sessions a subsystem keeps for itself (an extension's shared connection) are excluded too; reach those through that subsystem's own handle. A caller that claimed a namespace (X-Termcp-Namespace) sees only its own."),
	), withLogging("session_list", s.handleListSessions, s))
	mcpServer.AddTool(s.newTool("session_info",
		mcpgo.WithDescription("Return a JSON document with detailed fields for one session: identifiers, command line, PTY size, remote connection metadata, exit state, etc. session_id accepts a raw id or a termcp:// locator (\"termcp://#<sid>\"). Mode is a per-shell property and is not on the session record; read it from shell_list."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id or termcp:// locator (termcp://#<sid>)")),
	), withLogging("session_info", s.handleGetSessionInfo, s))

	mcpServer.AddTool(s.newTool("session_terminate",
		mcpgo.WithDescription("Close a session: terminates all shells, closes the SSH transport, cascades forwards, but keeps the entry in the registry (status: exited / DEAD) and in the Web UI as a read-only tile, so output remains readable via shell_output. session_id accepts a raw id, a termcp:// locator (\"termcp://#<sid>\"), or a comma-separated list of these (batch): a batch closes every entry independently — one failing entry (e.g. a not-found id) does not stop the rest — and returns per-session outcomes JSON. force=true = immediate kill; force=false waits grace_period after SIGTERM. To permanently erase the session and its on-disk byte logs, use session_delete. To close one shell only, use shell_close."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id, termcp:// locator (termcp://#<sid>), or a comma-separated list of these")),
		mcpgo.WithBoolean("force", mcpgo.Description("If true, end immediately without honoring grace_period"), mcpgo.DefaultBool(false)),
		mcpgo.WithNumber("grace_period", mcpgo.Description("Seconds to allow after SIGTERM before hard close when force is false (0–60)"), mcpgo.DefaultNumber(5)),
	), withLogging("session_terminate", s.handleTerminateSession, s))

	mcpServer.AddTool(s.newTool("session_delete",
		mcpgo.WithDescription("Permanently delete a session: finalizes its process (running or DEAD), releases every child resource (shells, forwards, notification rules, buffers), drops the registry entry (its tile disappears from the Web UI) and removes its on-disk directory (manifests + log.bin + log.jsonl). Irreversible. session_id accepts a raw id, a termcp:// locator, or a comma-separated list (batch): every entry is deleted independently — one failing entry does not stop the rest — and the result carries per-session outcomes. To merely stop a session and keep reading its output, use session_terminate."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id, termcp:// locator (termcp://#<sid>), or a comma-separated list of these")),
	), withLogging("session_delete", s.handleDeleteSession, s))

	mcpServer.AddTool(s.newTool("shell_resize",
		mcpgo.WithDescription("Update PTY rows/cols for a shell channel (propagates to SSH remote PTY when applicable). Requires a pty shell; a pipe shell has no TTY and returns an error."),
		mcpgo.WithString("shell_id", mcpgo.Required()),
		mcpgo.WithNumber("rows", mcpgo.DefaultNumber(24)),
		mcpgo.WithNumber("cols", mcpgo.DefaultNumber(80)),
	), withLogging("shell_resize", s.handleResizePty, s))

	mcpServer.AddTool(s.newTool("shell_detect",
		mcpgo.WithDescription("Probe the termcp host (not a remote ssh_config) for an interactive shell: returns path, family (unix/powershell/cmd), and a hint."),
	), withLogging("shell_detect", s.handleDetectShell, s))

	mcpServer.AddTool(s.newTool("ssh_config",
		mcpgo.WithDescription("SSH connection profiles: action=list returns usable profile names for session_start (never secrets or hostnames)."+sshConfigImportFormat),
		mcpgo.WithString("action", mcpgo.Required(), mcpgo.Enum("list")),
	), withLogging("ssh_config", s.handleSSHConfigOps, s))

	mcpServer.AddTool(s.newTool("extension",
		mcpgo.WithDescription(extensionToolDescription),
		mcpgo.WithString("action", mcpgo.Required(), mcpgo.Enum(extensionActions...)),
		mcpgo.WithString("name", mcpgo.Description("get/create/update/delete/retry: extension name ([A-Za-z0-9_-], max 64)")),
		mcpgo.WithString("kind", mcpgo.Description("create: mcp | skill | prompt | custom; supplies the defaults for the fields you omit")),
		mcpgo.WithString("target", mcpgo.Description("create: an ssh_config profile name, or \"internal\" for the termcp host's own loopback")),
		mcpgo.WithString("description"),
		mcpgo.WithString("command", mcpgo.Description("create/update: the executable a pipe or run-and-exit session runs; a pipe channel has no fallback shell, so mode=pipe requires one")),
		mcpgo.WithArray("args", mcpgo.Description("create/update: argv after command; omit keeps the stored value, [] clears it"), mcpgo.WithStringItems()),
		mcpgo.WithString("url", mcpgo.Description("create/update: an HTTP MCP endpoint to relay instead of a command (SSE if the path ends in /sse); target must be \"internal\"")),
		mcpgo.WithObject("headers", mcpgo.Description("create/update: request headers for url (name -> value); {} clears")),
		mcpgo.WithString("mode", mcpgo.Description("create/update: pty or pipe; kind=skill leaves it empty, meaning no shell at all"), mcpgo.Enum("pty", "pipe")),
		mcpgo.WithNumber("rows", mcpgo.Description("create/update: pty height (0 = default 24)")),
		mcpgo.WithNumber("cols", mcpgo.Description("create/update: pty width (0 = default 80)")),
		mcpgo.WithArray("allow", mcpgo.Description("create/update: the tool whitelist this session accepts; empty = unrestricted, and a call outside it is really refused"), mcpgo.WithStringItems()),
		mcpgo.WithArray("paths", mcpgo.Description("create/update: remote paths file tools may touch — prefix, glob, or re:regex; kind=skill requires at least one"), mcpgo.WithStringItems()),
		mcpgo.WithString("notes", mcpgo.Description("create/update: the operating instructions action=get returns; \"\" clears them")),
		mcpgo.WithBoolean("enabled", mcpgo.Description("create/update: false parks it — session torn down, nothing reconnects until enabled")),
		mcpgo.WithArray("forwards", mcpgo.Description("create/update: tunnels set up on this session and torn down with it; replaces the list when present")),
		extensionForwardItems(),
	), withLogging("extension", s.handleExtensionOps, s))

	mcpServer.AddTool(s.newTool("message",
		mcpgo.WithDescription("A session's transcript index: action=list returns the spans of the shell's byte log (status, time, start, end) in order, each also carrying from_line/to_line — the lines of the raw stream it covers (line numbers are absolute, 1-based; a span ending mid-line reports to_line < from_line, meaning it completes no line). total_lines gives the stream's complete-line count. The bytes themselves are read with shell_output(offset=start, max_bytes=end-start), or by line with shell_output(from_line=...).\n\nstatus is the one field worth reading: it says who produced the span - in the business sense (who acted), not which pipe bytes arrived on: o covers both stdout and stderr, stream ownership is read through shell_output(stream=). o = output from the shell, a = input an AI agent wrote through MCP, i = input written at the terminal (the human, through the Web UI), q = an approval was requested at this point, A = an approved input was released and written. An i or q span is the evidence that a human has operated this terminal, which is otherwise invisible in the byte stream (the agent's and the human's keystrokes produce the same echo). Input spans are zero-length marks: start == end, because the bytes live in the terminal's echo rather than being written twice. Both `a` and `i` mark the point where a line was submitted, not where typing started."),
		mcpgo.WithString("action", mcpgo.Required(), mcpgo.Enum("list")),
		mcpgo.WithString("session_id", mcpgo.Required()),
	), withLogging("message", s.handleMessageOps, s))

	mcpServer.AddTool(s.newTool("shell_reader_register",
		mcpgo.WithDescription("Allocate a new output reader_id for a shell, observing only bytes written after registration (no backlog). Pair every shell_output(..., reader_id) with the returned id."),
		mcpgo.WithString("shell_id", mcpgo.Required()),
	), withLogging("shell_reader_register", s.handleRegisterReader, s))

	mcpServer.AddTool(s.newTool("shell_reader_unregister",
		mcpgo.WithDescription("Release a reader_id previously returned by shell_reader_register."),
		mcpgo.WithString("shell_id", mcpgo.Required()),
		mcpgo.WithNumber("reader_id", mcpgo.Required(), mcpgo.Description("Non-zero reader id from shell_reader_register")),
	), withLogging("shell_reader_unregister", s.handleUnregisterReader, s))

	mcpServer.AddTool(s.newTool("shell_notify",
		mcpgo.WithDescription("Manage event notifications (reverse wake-up signal) for a shell channel: action=register sets a rule on channel resource or sampling; action=unregister removes by rule_id; action=list returns active rules.\n\nUse it to WAIT on a human instead of polling: when the command you started now needs a person at the terminal (a sudo/password prompt, an interactive installer, any prompt they must answer), call notify_user so they know it is their turn, then register event=output on that shell and stop calling shell_output in a loop — their next keystroke echo wakes you. Unregister when they have answered; closing the shell or deleting the session clears the rule as well."),
		mcpgo.WithString("action", mcpgo.Required(), mcpgo.Enum("register", "unregister", "list")),
		mcpgo.WithString("shell_id", mcpgo.Description("Target shell_id (required for register; optional filter for list)")),
		mcpgo.WithString("channel", mcpgo.Description("Delivery channel (required for register): resource (MCP notifications/resources/updated) or sampling (MCP sampling/createMessage)"), mcpgo.Enum("resource", "sampling")),
		mcpgo.WithString("event", mcpgo.Description("Trigger event (register only): output (default, dual-edge: immediate + 2s trailing delay), exit (one-shot on exit/abort), silence (one-shot after N sec without output)"), mcpgo.DefaultString("output"), mcpgo.Enum("exit", "silence", "output")),
		mcpgo.WithNumber("silence_seconds", mcpgo.Description("Silence window in seconds for event=silence (default 3)"), mcpgo.DefaultNumber(3)),
		mcpgo.WithString("rule_id", mcpgo.Description("Rule identifier (required for unregister)")),
	), withLogging("shell_notify", s.handleShellNotifyOps, s))

	mcpServer.AddTool(s.newTool("notify_user",
		mcpgo.WithDescription("Post a visible notification to the human at the termcp Web UI (not the AI Agent): a colored toast on every open page plus a browser system notification; when session_id is set, that session's card is also highlighted.\n\nUse your own judgment: notify whenever the human would want to be interrupted or may not be watching this conversation — anything that needs their eyes, decision or action counts, not a fixed list of cases. **Always call notify_user before asking the human for anything**: passwords, sudo/passphrases, MFA, confirmation, approval, a choice, or interactive input; also notify when a long task ends or fails. Blockers read best as level=warn/error with duration_seconds=0 and the affected session_id, so the toast stands out and the right card highlights. Also say the same thing in your reply: delivered=0 means no Web UI tab was open and the toast was never shown."),
		mcpgo.WithString("message", mcpgo.Required(), mcpgo.Description("Notification text shown to the user")),
		mcpgo.WithString("title"),
		mcpgo.WithString("level", mcpgo.Description("info (default), success, warn, or error"), mcpgo.DefaultString("info"), mcpgo.Enum("info", "success", "warn", "error")),
		mcpgo.WithNumber("duration_seconds", mcpgo.Description("Seconds the toast stays before auto-dismissing (0–600); 0 = sticky until dismissed. Prefer 0 when the human must act before it can be ignored"), mcpgo.DefaultNumber(10)),
		mcpgo.WithString("session_id", mcpgo.Description("Optional: highlight this session's card (and its terminal window, if open)")),
	), withLogging("notify_user", s.handleNotifyUser, s))

	// --- Port forwarding: one entry, action selects mode (OpenSSH names) ---
	mcpServer.AddTool(s.newTool("forward",
		mcpgo.WithDescription("SSH port forwarding: action(local) = ssh -L termcp hears on local_port→remote_host:remote_port; action(remote) = ssh -R (server listens on local_host:local_port → remote_host:remote_port reachable from termcp); action(dynamic) = ssh -D SOCKS5 (local_port, 0 = random); action(list) all forwards; action(close) by forward_id."),
		mcpgo.WithString("action", mcpgo.Required(), mcpgo.Enum("local", "remote", "dynamic", "list", "close")),
		mcpgo.WithString("session_id", mcpgo.Description("For local/remote/dynamic; from session_start")),
		mcpgo.WithString("remote_host", mcpgo.Description("local: target host relative to the SSH server"), mcpgo.DefaultString("localhost")),
		mcpgo.WithNumber("remote_port", mcpgo.Description("local: target port on remote host")),
		mcpgo.WithNumber("local_port", mcpgo.Description("local/dynamic: local listen port (0 = random)")),
		mcpgo.WithString("local_host", mcpgo.Description("remote: bind address for the SSH server listener"), mcpgo.DefaultString("0.0.0.0")),
		mcpgo.WithString("forward_id", mcpgo.Description("close: id from action(list)")),
	), withLogging("forward", s.handleForwardOps, s))
	// --- File operation tools (session-scoped SFTP) ---
	mcpServer.AddTool(s.newTool("file_read",
		mcpgo.WithDescription("Read a remote file via SSH/SFTP. mode text = printable with \\xHH escapes; hex = hex dump; file = download to the termcp host. text/hex reads return at most 8 MiB per call: page with offset + has_more/total_size from the result. mode=file streams the whole file (omit offset/length). Example: read first 1KB hex of /var/log/syslog — {session_id, remote_path, mode:\"hex\", offset:0, length:1024}."),
		mcpgo.WithString("session_id", mcpgo.Required()),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file path")),
		mcpgo.WithNumber("offset", mcpgo.Description("Start byte offset (0-based)"), mcpgo.DefaultNumber(0)),
		mcpgo.WithNumber("length", mcpgo.Description("Bytes to read (0 = rest of file; text/hex capped at 8 MiB per call)"), mcpgo.DefaultNumber(0)),
		mcpgo.WithString("mode", mcpgo.Description("Output mode: text, hex, or file"), mcpgo.DefaultString("text"), mcpgo.Enum("text", "hex", "file")),
		mcpgo.WithString("local_path", mcpgo.Description("Download destination path on the termcp host. Only used with mode=file.")),
	), withLogging("file_read", s.handleFileRead, s))

	mcpServer.AddTool(s.newTool("file_write",
		mcpgo.WithDescription("Write a remote file via SSH/SFTP. Small writes: inline data (text with \\xHH, or hex). Large/binary: local_path + local_offset + length streams from a file on the termcp host. offset>0 writes without truncating (use file_stat size to append)."),
		mcpgo.WithString("session_id", mcpgo.Required()),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file path")),
		mcpgo.WithNumber("offset", mcpgo.Description("Write start offset: 0 rewrites the file from the beginning (truncates); >0 writes at that byte position without truncating (to append, set offset to the current file size from file_stat)"), mcpgo.DefaultNumber(0)),
		mcpgo.WithString("data", mcpgo.Description("Inline data to write (text or hex per mode)")),
		mcpgo.WithString("mode", mcpgo.Description("Data encoding: text (default, supports \\xHH) or hex"), mcpgo.DefaultString("text"), mcpgo.Enum("text", "hex")),
		mcpgo.WithString("local_path", mcpgo.Description("Source file path on the termcp host")),
		mcpgo.WithNumber("local_offset", mcpgo.Description("Read start offset in local file"), mcpgo.DefaultNumber(0)),
		mcpgo.WithNumber("length", mcpgo.Description("Bytes to read from local file (0=all)"), mcpgo.DefaultNumber(0)),
	), withLogging("file_write", s.handleFileWrite, s))

	mcpServer.AddTool(s.newTool("file_stat",
		mcpgo.WithDescription("Get file or directory info from remote via SSH/SFTP. Returns name, size, is_dir, mod_time, and children list for directories."),
		mcpgo.WithString("session_id", mcpgo.Required()),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file or directory path")),
	), withLogging("file_stat", s.handleFileStat, s))

	mcpServer.AddTool(s.newTool("file_delete",
		mcpgo.WithDescription("Delete a remote file or empty directory via SSH/SFTP."),
		mcpgo.WithString("session_id", mcpgo.Required()),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file or directory path to delete")),
	), withLogging("file_delete", s.handleFileDelete, s))

	mcpServer.AddTool(s.newTool("file_rename",
		mcpgo.WithDescription("Move or rename a remote file/directory via SSH/SFTP (same filesystem)."),
		mcpgo.WithString("session_id", mcpgo.Required()),
		mcpgo.WithString("from_path", mcpgo.Required(), mcpgo.Description("Current remote path")),
		mcpgo.WithString("to_path", mcpgo.Required(), mcpgo.Description("New remote path")),
	), withLogging("file_rename", s.handleFileRename, s))

	mcpServer.AddTool(s.newTool("file_mkdir",
		mcpgo.WithDescription("Create a directory (and parents) on the remote via SSH/SFTP."),
		mcpgo.WithString("session_id", mcpgo.Required()),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote directory path to create")),
	), withLogging("file_mkdir", s.handleFileMakeDir, s))

	mcpServer.AddTool(s.newTool("file_urls",
		mcpgo.WithDescription("Get HTTP download/upload URLs for a remote file path under a session. Use these URLs for direct curl/wget/browser access. response_headers are folded into the download URL, so whoever opens it (curl, a browser) gets them."),
		mcpgo.WithString("session_id", mcpgo.Required()),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file path")),
		mcpgo.WithObject("response_headers", mcpgo.AdditionalProperties(map[string]any{"type": "string"}),
			mcpgo.Description("Headers the download answers with, e.g. {\"Content-Disposition\": \"attachment; filename=\\\"x.png\\\"\"}; folded into the URL. Content-Length/Content-Range stay the server's.")),
	), withLogging("file_urls", s.handleGetFileURLs, s))

	// --- Low-frequency file operations: one tool per parameter family, ---
	// dispatched by the `action` enum. This keeps rare operations available
	// (SFTP works uniformly on Windows/Unix) without bloating tools/list.
	mcpServer.AddTool(s.newTool("file_perm",
		mcpgo.WithDescription("Ownership/metadata ops on a remote path: chmod (mode, decimal perms), chown (uid+gid), chtimes (atime+mtime Unix milliseconds)."),
		mcpgo.WithString("session_id", mcpgo.Required()),
		mcpgo.WithString("action", mcpgo.Required(), mcpgo.Enum("chmod", "chown", "chtimes")),
		mcpgo.WithString("remote_path", mcpgo.Required()),
		mcpgo.WithNumber("mode", mcpgo.Description("chmod: decimal Unix perms, e.g. 493 = 0755")),
		mcpgo.WithNumber("uid", mcpgo.Description("chown: numeric user ID")),
		mcpgo.WithNumber("gid", mcpgo.Description("chown: numeric group ID")),
		mcpgo.WithNumber("atime", mcpgo.Description("chtimes: access time, Unix milliseconds")),
		mcpgo.WithNumber("mtime", mcpgo.Description("chtimes: modification time, Unix milliseconds")),
	), withLogging("file_perm", s.handleFilePerm, s))

	mcpServer.AddTool(s.newTool("file_link",
		mcpgo.WithDescription("Link ops: readlink (remote_path → {target}), symlink (target + link_path), link/hardlink (existing_path + new_path)."),
		mcpgo.WithString("session_id", mcpgo.Required()),
		mcpgo.WithString("action", mcpgo.Required(), mcpgo.Enum("readlink", "symlink", "link")),
		mcpgo.WithString("remote_path", mcpgo.Description("readlink: symlink path")),
		mcpgo.WithString("target", mcpgo.Description("symlink: existing path to point to")),
		mcpgo.WithString("link_path", mcpgo.Description("symlink: new symlink path")),
		mcpgo.WithString("existing_path", mcpgo.Description("link: existing file")),
		mcpgo.WithString("new_path", mcpgo.Description("link: new hard link path")),
	), withLogging("file_link", s.handleFileLinkOp, s))

	mcpServer.AddTool(s.newTool("file_fs",
		mcpgo.WithDescription("Path/filesystem ops on a remote path: truncate (size bytes), realpath ({canonical_path}), statvfs (space/inodes)."),
		mcpgo.WithString("session_id", mcpgo.Required()),
		mcpgo.WithString("action", mcpgo.Required(), mcpgo.Enum("truncate", "realpath", "statvfs")),
		mcpgo.WithString("remote_path", mcpgo.Required()),
		mcpgo.WithNumber("size", mcpgo.Description("truncate: new size in bytes")),
	), withLogging("file_fs", s.handleFileFsOp, s))

	mcpServer.AddTool(s.newTool("file_getwd",
		mcpgo.WithDescription("Get the SFTP working directory for this session connection. This is not the interactive shell's cwd (pwd); use a shell command for that."),
		mcpgo.WithString("session_id", mcpgo.Required()),
	), withLogging("file_getwd", s.handleFileGetwd, s))
}
