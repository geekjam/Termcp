package mcp

import mcpgo "github.com/mark3labs/mcp-go/mcp"

// annotationNone overrides mcp-go's default hint block. NewTool otherwise
// serializes four default booleans for every tool; they carry no termcp
// specific information and cost roughly 80 bytes per definition.
var annotationNone = mcpgo.WithToolAnnotation(mcpgo.ToolAnnotation{})

// sshConfigImportFormat is the import/export file shape, stated identically in
// both ssh_config descriptions (the default listing and the write-enabled one,
// which RegisterSSHConfigWriteTools installs): they are two different strings
// built in two files, and a format spec duplicated by hand would drift the
// moment one of them is edited. Both variants reach the model in different
// deployments, and neither is privileged -- the default one is what a
// read-only instance shows, the other what an operator with
// --mcp-manage-ssh-configs shows -- so the spec has to be in both.
//
// It exists because the agent is often the one asked to *produce* the text (a
// user pastes in a list of hosts and wants it as an importable file), and the
// file format is not the tool's argument shape: a bastion is a nested table,
// while jump_host is an argument of this very tool. Writing the argument spelling
// into a file parses without error and silently drops the bastion, which is the
// failure worth documenting here rather than leaving to be discovered.
const sshConfigImportFormat = " IMPORT/EXPORT FORMAT (Web UI batch import; HTTP GET/POST /api/connections/batch): TOML, one [[connections]] table per profile with name (letters/digits/_/-, max 64) and kind=\"remote\", plus host, user, and password or private_key. Optional: port, key_passphrase, trust_unknown_host, known_hosts, dial_timeout_seconds, proxy, description, default_shell, default_mode, default_approval. A bastion is NESTED: [connections.jump] ([connections.jump.jump] for a deeper hop). The flat jump_host keys are arguments of this tool, NOT file fields -- a file using them parses cleanly and silently drops the bastion. Export skips temporary profiles; \"internal\" is reserved, collisions get name-2, credentials are write-only."

// Compact descriptions keep the wire representation useful without repeating
// the same lifecycle and ID guidance in every tool. The original descriptions
// remain next to registrations as source documentation; only the description
// advertised to MCP clients is reduced.
var compactToolDescriptions = map[string]string{
	"session_start":           "Start a session; returns session_id + shell_id. ssh_config REQUIRED (\"internal\" = host loopback, or profile name). DEFAULT: omit command/args for multi-step/stateful work. command/args = one run-and-exit program: set on_exit=\"close\" so its session archives itself; never split steps across starts.",
	"shell_open":              "Open another shell channel on a session; returns shell_id. command+args = argv (no shell line: spaced command = ONE name; pass tokens as args).",
	"shell_list":              "List shell channels for a session.",
	"shell_close":             "Close one shell channel; use session_terminate for the whole session.",
	"shell_input":             "Write text to shell stdin without executing; use shell_key(enter) to run it. Returns offset/total_lines/line — where this input lands in the transcript; pair responses via message spans.",
	"shell_key":               "Send a named key to a shell.",
	"shell_output":            "Read output of a live or closed shell; empty read != done (poll, timeout<=3). Cursor (offset/tail_lines/from_line/reader_id): advance by end_offset, never bytes_returned; partial_tail_bytes>0 = mid-line tail. stream=o|e filters by producer.",
	"session_list":            "List sessions (a subsystem’s own connections are excluded).",
	"session_info":            "Session details + its shells.",
	"session_terminate":       "Close a session (DEAD): stops shells and forwards; entry stays in the registry and readable.",
	"session_delete":          "Delete a session and its on-disk byte logs.",
	"shell_resize":            "Resize shell PTY.",
	"shell_detect":            "Detect an interactive shell on the termcp host.",
	"shell_reader_register":   "Register an output reader starting at the current buffer end.",
	"shell_reader_unregister": "Release an output reader.",
	"forward":                 "SSH port forwards: local (-L), remote (-R), dynamic (-D SOCKS5), list, close.",
	"file_read":               "Read a remote file via SFTP as text, hex, or a host-side download.",
	"file_write":              "Write a remote file via SFTP inline or from a host-side file.",
	"file_stat":               "Get remote file or directory metadata.",
	"file_delete":             "Delete a remote file or empty directory.",
	"file_rename":             "Move or rename a remote path.",
	"file_mkdir":              "Create a remote directory and its parents.",
	"file_urls":               "Get direct HTTP download/upload URLs for a remote path.",
	"file_perm":               "Ownership/metadata ops: chmod, chown, or chtimes.",
	"file_link":               "Link ops: readlink, symlink, or hard link.",
	"file_fs":                 "Path/filesystem ops: truncate, realpath, or statvfs.",
	"file_getwd":              "Get the SFTP working directory for a session.",
	"message":                 "Transcript span index: status, time, byte offsets, from_line/to_line line numbers. status: o output, a AI input, i human input, q approval asked, A approved input; i/q = human operated. Bytes via shell_output(offset, max_bytes); lines via shell_output(from_line).",
	"ssh_config":              "SSH profiles: action=list names, or (if enabled) create/edit/copy/delete." + sshConfigImportFormat,
	"extension":               "Extensions — pinned sessions with trimmed capabilities; enabled extensions on a host share one connection, each driving its own shell_id. action=list → status + the session_id/shell_id to drive them through; get → one template with its notes (read them before first use); create/update author one (kind+target suffices; an omitted field keeps its stored value, an empty array clears it); delete; retry reconnects an unavailable one now. kind: mcp = an MCP server (a stdio command, or a url relayed in-process), skill = read-only file window (no shell), prompt = terminal with instructions. An available extension is an ordinary session: drive it with shell_input/shell_output or file_read against its handles; a call the template did not whitelist is really refused. Extension sessions are not listed by session_list; never terminate or delete one. Full guide: resource extensions.md.",
	"shell_notify":            "Manage wake-up rules (register/unregister/list). TO WAIT ON A HUMAN AT A PROMPT (sudo/password, interactive prompts): notify_user first, register event=output HERE, stop polling shell_output — their next keystroke wakes you; unregister when they have answered.",
	"notify_user":             "Notify the human user via the termcp Web UI: toast + system notification on every open page; session_id highlights that session's card. CALL IT BEFORE ASKING THE HUMAN FOR ANYTHING (password/sudo/MFA/passphrase, confirmation, approval, a choice, or interactive input) and when a long task ends or fails. Blocking asks: level=warn or error, duration_seconds=0 (sticky), the affected session_id. Once you have asked, WAIT: never poll, guess or fake an answer. delivered=0 means no Web UI page was open.",
}

// newTool applies termcp's compact wire representation to a tool definition.
// Tool metadata is sent to the model with every tool listing, so repeated
// parameter explanations are kept only where they add semantics beyond the
// parameter name, type, and compact tool description.
//
// Low-frequency tools get defer_loading only when the server was constructed
// with DeferTools() (the --mcp-defer-tools switch): the classification lives
// in deferredTools, and whether the marker reaches the wire is a deployment
// choice decided here, at the single registration point.
func (s *Server) newTool(name string, opts ...mcpgo.ToolOption) mcpgo.Tool {
	opts = append([]mcpgo.ToolOption{annotationNone}, opts...)
	if s.shouldDeferTool(name) {
		opts = append(opts, mcpgo.WithDeferLoading(true))
	}
	t := mcpgo.NewTool(name, opts...)
	if desc, ok := compactToolDescriptions[name]; ok {
		t.Description = desc
	}
	compactToolSchema(&t)
	return t
}

// deferredTools lists tools whose full JSON Schema may be withheld from the
// initial tools/list so a client can load them on demand (MCP deferred tool
// loading, sent as `defer_loading: true`).
//
// This is the *classification*; whether the marker is actually emitted depends
// on the --mcp-defer-tools switch (see Server.shouldDeferTool), which is off by
// default because several widely used clients ignore or mishandle the marker.
//
// Keep the CORE loop out of this set — session_start/list/info/terminate/delete,
// shell_open/close/input/key/output, notify_user — because a model that has to
// search before it can type a command wastes a round trip on every interaction.
// Deferred are the wide, low-frequency surfaces: SFTP (11 tools, parameter-heavy),
// port forwarding, PTY/reader plumbing, message inspection and SSH profile admin.
var deferredTools = map[string]bool{
	// Files (SFTP): only needed once a session is already being driven.
	"file_read": true, "file_write": true, "file_stat": true,
	"file_delete": true, "file_rename": true, "file_mkdir": true,
	"file_urls": true, "file_perm": true, "file_link": true,
	"file_fs": true, "file_getwd": true,
	// Networking and channel plumbing.
	"forward":                 true,
	"shell_resize":            true,
	"shell_detect":            true,
	"shell_notify":            true,
	"shell_reader_register":   true,
	"shell_reader_unregister": true,
	// Introspection and configuration.
	"message":    true,
	"ssh_config": true,
	"extension":  true,
}

// These fields are either identifiers, paths, coordinates, or self-evident
// SSH profile fields. Their descriptions repeat information already present in
// the tool name/instructions and are not useful enough to justify their cost.
var omitPropertyDescriptions = map[string]struct{}{
	"session_id": {}, "shell_id": {}, "reader_id": {}, "stream": {},
	"name": {}, "ssh_config": {}, "rows": {}, "cols": {},
	"remote_path": {}, "from_path": {}, "to_path": {},
	"target": {}, "link_path": {}, "existing_path": {}, "new_path": {},
	"forward_id": {}, "source_name": {}, "target_name": {},
	"host": {}, "user": {}, "port": {}, "local_host": {}, "local_port": {},
	"remote_host": {}, "remote_port": {}, "uid": {}, "gid": {},
	"atime": {}, "mtime": {}, "size": {}, "title": {},
	"description": {}, "default_shell": {},
	"trust_unknown_host": {}, "known_hosts": {}, "dial_timeout_seconds": {},
	"jump_host": {}, "jump_user": {}, "jump_port": {}, "jump_password": {},
	"jump_private_key": {}, "jump_key_passphrase": {},
	"jump_trust_unknown_host": {}, "jump_known_hosts": {},
	"jump_dial_timeout_seconds": {}, "jump_proxy": {},
}

var keepPropertyDescriptions = map[string]bool{
	"file_perm": true,
	"file_link": true, // grouped tool with action-dependent params
	"file_fs":   true, // grouped tool with action-dependent params
}

func compactToolSchema(t *mcpgo.Tool) {
	if keepPropertyDescriptions[t.Name] {
		return
	}
	for name, raw := range t.InputSchema.Properties {
		prop, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, omit := omitPropertyDescriptions[name]; omit {
			delete(prop, "description")
		}
	}
}
