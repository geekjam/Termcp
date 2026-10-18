package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"

	"github.com/open-mcp-ai/termcp/internal/approval"
	"github.com/open-mcp-ai/termcp/internal/config"
	"github.com/open-mcp-ai/termcp/internal/daemon"
	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
	"github.com/open-mcp-ai/termcp/internal/extensions"
	"github.com/open-mcp-ai/termcp/internal/forward"
	mcpmod "github.com/open-mcp-ai/termcp/internal/mcp"
	"github.com/open-mcp-ai/termcp/internal/mcpbridge"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/internal/webui"
)

type runningApplication struct {
	once       sync.Once
	sessions   *session.Manager
	store      *storage.Store
	ssh        *sshserver.Server
	mcp        *mcpmod.Server
	extensions *extensions.Reconciler
	err        error
}

func (a *runningApplication) Close() error {
	a.once.Do(func() {
		if a.extensions != nil {
			a.extensions.Stop()
		}
		if a.sessions != nil {
			a.sessions.ShutdownAll()
		}
		if a.ssh != nil {
			a.ssh.Stop()
		}
		if a.mcp != nil {
			a.err = a.mcp.Stop()
		}
		if a.store != nil {
			a.err = errors.Join(a.err, a.store.Close())
		}
	})
	return a.err
}

func buildApplication(cfg *config.Config, keys *encryptedfs.KeyManager, server *http.Server, management *webui.Handler) (http.Handler, io.Closer, error) {
	a := &runningApplication{}
	store, err := storage.NewEncrypted(cfg.DataDir, keys)
	if err != nil {
		return nil, nil, err
	}
	a.store = store
	profiles, err := sshconfig.NewEncryptedStore(cfg.DataDir, keys)
	if err != nil {
		return nil, a, err
	}
	if !cfg.NoInternal {
		a.ssh = sshserver.New()
		mcpbridge.RegisterBuiltin(a.ssh)
		if err := a.ssh.Start(); err != nil {
			return nil, a, err
		}
	}
	messages := message.NewManager(store)
	a.sessions = session.NewManager(messages, store, a.ssh)
	if err := a.sessions.RestoreDead(); err != nil {
		return nil, a, err
	}
	forwards := forward.NewForwardManager()
	templates, err := extensions.NewEncryptedStore(cfg.DataDir, keys)
	if err != nil {
		return nil, a, err
	}
	a.extensions = extensions.NewReconciler(templates, a.sessions, messages, profiles, forwards)
	a.extensions.NoInternal = cfg.NoInternal
	if err := a.extensions.Start(context.Background()); err != nil {
		return nil, a, err
	}
	options := []mcpmod.Option{mcpmod.WithHTTPServer(server)}
	level := cfg.MCPDeferToolsLevel
	if level == "" && cfg.MCPDeferTools {
		level = mcpmod.DeferPartial
	}
	if level != "" && level != mcpmod.DeferOff {
		options = append(options, mcpmod.DeferLevel(level))
	}
	a.mcp = mcpmod.New(a.sessions, messages, profiles, forwards, versionString(), options...)
	a.mcp.SetDocsFS(webui.Assets())
	a.mcp.NoInternal = cfg.NoInternal
	if cfg.MCPManageSSHConfigs {
		a.mcp.RegisterSSHConfigWriteTools()
	}
	a.mcp.PrepareHTTP(server.Addr)
	mux := http.NewServeMux()
	mux.Handle("GET /sse", a.mcp.SSEHandler())
	mux.Handle("POST /message", a.mcp.MessageHandler())
	mux.Handle("/stream", a.mcp.StreamableHTTPHandler())
	h := &webui.Handler{Sessions: a.sessions, MsgMgr: messages, SSH: profiles, ForwardMgr: forwards, NotifyMgr: a.mcp.NotifyManager(), Extensions: a.extensions, NoInternal: cfg.NoInternal, Version: versionString(), Daemon: management.Daemon, StartedAt: management.StartedAt, DaemonLog: management.DaemonLog, IdleTimeout: management.IdleTimeout, StopDaemon: management.StopDaemon}
	h.ExecuteOperation = a.mcp.ExecuteApprovedOperation
	h.Register(mux)
	a.mcp.SetUINotifier(h.BroadcastUINotify)
	a.mcp.SetExtensionReconciler(a.extensions)
	h.MCPConnections = func() []any {
		connections := a.mcp.Connections()
		out := make([]any, len(connections))
		for i, connection := range connections {
			out[i] = connection
		}
		return out
	}
	a.mcp.SetConnectionsListener(h.NotifyClientsChanged)
	a.sessions.AddApprovalListener(func(sessionID string, request approval.Request) {
		if request.ShellID != "" {
			a.mcp.NotifyManager().OnApprovalChange(request.ShellID, "approval "+string(request.State))
		}
	})
	var handler http.Handler = mux
	if management.Daemon {
		// A migration must not be interrupted by an idle countdown. Start the
		// countdown only once a complete business graph can be published.
		watcher := daemon.NewIdleWatcher(management.IdleTimeout, management.StopDaemon)
		watcher.Exempt = daemon.IsProbe
		handler = watcher.Track(handler)
	}
	return handler, a, nil
}
