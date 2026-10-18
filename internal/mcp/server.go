package mcp

import (
	"context"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/open-mcp-ai/termcp/internal/extensions"
	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/notify"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// Server wraps the MCP SSE server, streamable HTTP handler, and tool handlers.
type Server struct {
	mcpServer       *mcpserver.MCPServer
	sseServer       *mcpserver.SSEServer
	streamServer    *mcpserver.StreamableHTTPServer
	sessMgr         *session.Manager
	msgMgr          *message.Manager
	sshConfigs      *sshconfig.Store
	forwardMgr      *forward.ForwardManager
	extensions      *extensions.Reconciler
	notifyMgr       *notify.Manager
	baseURL         string                // http://host:port, set from Start()
	docsFS          fs.FS                 // embedded agent docs, set via SetDocsFS
	NoInternal      bool                  // when true, hide and refuse the built-in loopback profile
	sshConfigWrites bool                  // expose write actions on the unified ssh_config tool
	deferLevel      string                // defer_loading level: off (default) | partial | all
	sseOpts         []mcpserver.SSEOption // forwarded to the mcp-go SSE transport

	// uiNotify delivers a user-facing notification to the Web UI (the notify_user
	// tool). Set by main to webui.Handler.BroadcastUINotify so this package stays
	// decoupled from webui. Returns the number of open tabs that received it.
	uiNotify func(level, title, message, sessionID string, durationSec int) int

	// connections tracks the MCP clients currently attached (see connections.go).
	// It is fed by mcp-go's session hooks, which is the only place a client
	// attaching or detaching is observable.
	connections *connRegistry
	// connListener is called when that set changes, so the Web UI can refresh
	// without polling. Set by main to the same broadcast the session list uses.
	connListener func()
}

// Option customizes a Server at construction time. Options keep New's signature
// stable for callers (and tests) that do not care about the newer knobs.
type Option func(*Server)

// WithHTTPServer hands the mcp-go SSE transport the http.Server it should attach
// to, so the SSE and streamable-HTTP handlers share termcp's listener.
func WithHTTPServer(h *http.Server) Option {
	return func(s *Server) { s.sseOpts = append(s.sseOpts, mcpserver.WithHTTPServer(h)) }
}

// Defer levels (--mcp-defer-tools-level). "off" lists every tool eagerly.
// "partial" defers the wide, low-frequency surfaces (SFTP, forwarding,
// plumbing, config) and keeps the driving loop eager. "all" additionally
// defers everything but the minimum loop — a client that supports deferred
// loading then pays one search round trip per first use, which is the point:
// the smallest possible tools/list.
const (
	DeferOff     = "off"
	DeferPartial = "partial"
	DeferAll     = "all"
)

// allLevelCore is the tool set that stays eager under DeferAll: the minimum
// "open a session and drive it" loop.
var allLevelCore = map[string]bool{
	"session_start": true,
	"shell_input":   true,
	"shell_key":     true,
	"shell_output":  true,
}

// shouldDeferTool reports whether a tool's schema may be withheld from the
// initial tools/list. The classification lives in deferredTools and
// allLevelCore; the level lives on the Server, so this is the single decision
// point for both the tools registered in New and the ones added later by
// RegisterSSHConfigWriteTools.
func (s *Server) shouldDeferTool(name string) bool {
	switch s.deferLevel {
	case DeferPartial:
		return deferredTools[name]
	case DeferAll:
		return !allLevelCore[name]
	default:
		return false
	}
}

// DeferLevel sets the defer_loading level: "off" (default), "partial" (core
// eager, low-frequency surfaces deferred) or "all" (only the minimum driving
// loop eager). Unknown levels degrade to "off" with a warning rather than
// guessing: clients that drop the defer marker would otherwise lose tools
// silently.
func DeferLevel(level string) Option {
	return func(s *Server) {
		switch level {
		case DeferOff, DeferPartial, DeferAll:
			s.deferLevel = level
		default:
			slog.Warn("unknown mcp defer level; defer_loading disabled", "level", level)
			s.deferLevel = DeferOff
		}
	}
}

// RegisterSSHConfigWriteTools upgrades the ssh_config tool schema in place with
// the write-capable actions (create/edit/copy/delete). Call only when
// -mcp-manage-ssh-configs is set. The dispatcher (handleSSHConfigOps) already
// routes these actions; without the upgrade only action=list is advertised and
// the others are rejected at the schema layer.
func (s *Server) RegisterSSHConfigWriteTools() {
	s.sshConfigWrites = true
	fields := func() []mcpgo.ToolOption {
		var o []mcpgo.ToolOption
		o = append(o, mcpgo.WithString("host"), mcpgo.WithString("user"))
		o = append(o,
			mcpgo.WithString("password", mcpgo.Description("create: password auth (or private_key); edit: replace, omit to keep")),
			mcpgo.WithString("private_key", mcpgo.Description("create/edit: PEM private key content")),
			mcpgo.WithString("key_passphrase", mcpgo.Description("create/edit: passphrase for encrypted private_key")),
			mcpgo.WithBoolean("trust_unknown_host", mcpgo.DefaultBool(false)),
			mcpgo.WithString("known_hosts"),
			mcpgo.WithNumber("dial_timeout_seconds", mcpgo.DefaultNumber(30)),
			mcpgo.WithString("proxy", mcpgo.Description("SOCKS5 proxy URL, e.g. socks5://user:pass@host:port")),
			mcpgo.WithString("description"),
			mcpgo.WithString("default_shell"),
			mcpgo.WithString("default_mode", mcpgo.Description("Default mode: pty or pipe")),
			mcpgo.WithBoolean("default_approval", mcpgo.Description("create/edit: start sessions from this profile with review mode on (required approvals per AI write). On edit, sending false turns it off; omitting it keeps the current value")),
			mcpgo.WithString("jump_host"),
			mcpgo.WithString("jump_user"),
			mcpgo.WithNumber("jump_port", mcpgo.DefaultNumber(22)),
			mcpgo.WithString("jump_password"),
			mcpgo.WithString("jump_private_key"),
			mcpgo.WithString("jump_key_passphrase"),
			mcpgo.WithBoolean("jump_trust_unknown_host", mcpgo.DefaultBool(false)),
			mcpgo.WithString("jump_known_hosts"),
			mcpgo.WithNumber("jump_dial_timeout_seconds", mcpgo.DefaultNumber(30)),
			mcpgo.WithString("jump_proxy"),
		)
		return o
	}

	writeActions := []string{"list", "create", "edit", "copy", "delete"}
	extra := []mcpgo.ToolOption{
		// Widen the action enum in place.
		func(t *mcpgo.Tool) {
			if prop, ok := t.InputSchema.Properties["action"].(map[string]any); ok {
				prop["enum"] = writeActions
			}
		},
		mcpgo.WithString("name", mcpgo.Description("create/edit/delete: profile name ([A-Za-z0-9_-], max 64)")),
		mcpgo.WithString("source_name", mcpgo.Description("copy: existing profile to copy from")),
		mcpgo.WithString("target_name", mcpgo.Description("copy: new profile name (must not exist)")),
	}
	extra = append(extra, fields()...)

	// Apply the extra properties onto the existing registered tool schema.
	tools := s.mcpServer.ListTools()
	st, ok := tools["ssh_config"]
	if !ok {
		panic("ssh_config tool must be registered before RegisterSSHConfigWriteTools")
	}
	t := st.Tool
	for _, opt := range extra {
		opt(&t)
	}
	t.Description = "SSH connection profiles: action=list (names only, never secrets), create, edit (patch; omitted fields keep stored values incl. secrets), copy (server-side, secrets never reach the agent), delete. create requires host+user and password or private_key." + sshConfigImportFormat
	s.mcpServer.DeleteTools("ssh_config")
	s.mcpServer.AddTool(t, withLogging("ssh_config", s.handleSSHConfigOps, s))
}

// SetExtensionReconciler wires the extension layer. Without it the extension
// tool reports not_configured rather than offering a surface with no backing
// state, so a deployment that does not run the reconciler fails loudly instead
// of silently accepting templates it never connects.
func (s *Server) SetExtensionReconciler(r *extensions.Reconciler) {
	s.extensions = r
}

// SSEHandler exposes the MCP SSE endpoint for mounting on a shared mux.
func (s *Server) SSEHandler() http.Handler {
	return s.sseServer.SSEHandler()
}

// MessageHandler exposes the MCP JSON-RPC message endpoint for mounting on a shared mux.
func (s *Server) MessageHandler() http.Handler {
	return s.sseServer.MessageHandler()
}

// StreamableHTTPHandler exposes the MCP streamable-HTTP endpoint (POST/GET/DELETE on one path).
// Mount at "/stream" (or another path with a matching wrapper); clients use e.g. http://host:port/stream.
func (s *Server) StreamableHTTPHandler() http.Handler {
	return s.streamServer
}

// Start begins serving MCP over SSE on the given address.
func (s *Server) Start(addr string) error {
	s.PrepareHTTP(addr)
	return s.sseServer.Start(addr)
}

// PrepareHTTP configures resources before an independently bootstrapped HTTP
// listener publishes the handlers (for startup storage unlock/migration).
func (s *Server) PrepareHTTP(addr string) {
	host, port, _ := net.SplitHostPort(addr)
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if port == "" {
		port = "8080"
	}
	s.baseURL = "http://" + net.JoinHostPort(host, port)
	// Docs resources/prompts need baseURL, so they are registered here (once)
	// rather than in New(). registerDocs is idempotent for a single Start.
	s.registerDocs()
}

// Stop gracefully shuts down the SSE server.
func (s *Server) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if s.streamServer != nil {
		_ = s.streamServer.Shutdown(ctx)
	}
	return s.sseServer.Shutdown(ctx)
}
