package webui

import (
	"net/http"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/locator"
	"github.com/open-mcp-ai/termcp/internal/session"
)

// The REST half of a session's Restriction.
//
// The whitelist was enforced on the MCP tool surface only, which is half a
// boundary: the extension layer's whole promise is that "a call the template did
// not whitelist is genuinely refused", and every capability its presets name —
// terminal I/O, reading files, opening a channel — exists on the HTTP surface
// too, on the proof that this is "the operator's own interface". That reasoning
// answers whether review mode should gate the path; it does not answer whether a
// template's author granted it. An agent handed an extension handle can reach
// 127.0.0.1:$PORT/api/shells/{id}/input with the same deployment token, so the
// template's whitelist has to be enforced here as well, with the same names and
// the same refusal text.
//
// These are deliberately thin: the decisions live in internal/session
// (AllowsTool / AllowsPath / ForbiddenTool / ForbiddenPath), so the two surfaces
// cannot drift into disagreeing about what a template permits.

// restRefuseTool answers the request when sess's whitelist does not carry tool,
// and reports whether it did. A nil session (or an unrestricted one) passes.
func restRefuseTool(w http.ResponseWriter, sess *session.Session, tool string) bool {
	if sess == nil || sess.Restriction().AllowsTool(tool) {
		return false
	}
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error_code": "forbidden",
		"error":      session.ForbiddenTool(tool),
	})
	return true
}

// restRefuseSessionIDs answers a comma-separated session id list when ANY entry
// is restricted, and reports whether it did. The list is resolved the way the
// MCP guard resolves one — a raw id, or a termcp:// locator — so a caller cannot
// walk around the refusal by spelling the id differently. Ids that resolve to
// nothing are left to the handler's own not-found handling.
func (h *Handler) restRefuseSessionIDs(w http.ResponseWriter, raw, tool string) bool {
	for _, id := range strings.Split(raw, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		sess := h.Sessions.Get(id)
		if sess == nil {
			if p, err := locator.Parse(id); err == nil && p.Kind != locator.KindEntry {
				sess = h.Sessions.Get(p.SessionID)
			}
		}
		if restRefuseTool(w, sess, tool) {
			return true
		}
	}
	return false
}

// sessionOwning resolves the session that owns a resource, for the routes that
// name the resource rather than the session: DELETE /api/forwards/{id},
// DELETE /api/notifications/{id}, DELETE /api/shells/{id} and the close-shell
// compatibility route. Without it those would be the only paths to a capability
// that skip the whitelist, since none carries a session id to check.
//
// Both ids are consulted because a resource records the session that created it
// and, for a rule, the shell it targets. The session is the container the
// whitelist belongs to; the shell is the fallback for an entry that predates the
// session field or was cleaned up. GetSessionByShellID is the lookup, not
// GetByShellID: the former also finds a retained DEAD/restored snapshot, so a
// closed session's resource is still resolved to its owner instead of reading as
// unowned and passing the gate. A nil Sessions manager yields nil, which
// restRefuseTool reads as unrestricted — the same thing an unfindable id means.
func (h *Handler) sessionOwning(sessionID, shellID string) *session.Session {
	if h.Sessions == nil {
		return nil
	}
	if sess := h.Sessions.Get(sessionID); sess != nil {
		return sess
	}
	if sess := h.Sessions.GetSessionByShellID(shellID); sess != nil {
		return sess
	}
	return nil
}

// restRefusePaths answers the request when any path named in paths is outside
// sess's whitelist, and reports whether it did. Each entry is {argumentName,
// path}; an empty path is skipped, so a caller may pass a tool's whole argument
// set and let the empty ones fall out. As on the MCP side, reading is as gated as
// writing: the whitelist is about reachability, not direction.
func restRefusePaths(w http.ResponseWriter, sess *session.Session, paths ...[2]string) bool {
	if sess == nil {
		return false
	}
	r := sess.Restriction()
	for _, p := range paths {
		if p[1] == "" {
			continue
		}
		if !r.AllowsPath(p[1]) {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error_code": "forbidden",
				"error":      session.ForbiddenPath(p[0], p[1]),
			})
			return true
		}
	}
	return false
}
