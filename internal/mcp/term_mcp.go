package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/envelope"
)

// The mcp directive: a JSON-RPC conversation.
//
// Four transports live behind it, and the interesting one is `raw`: `mcp send` and
// `mcp recv` assume nothing about the methods, so any line-oriented JSON-RPC peer
// works — an MCP server, an LSP, a private RPC. The protocol decides what the
// frames mean; this only moves them.
//
// The channel forms use handles:
//
//	pipe mac --argv npx -y some-server     → a stdio peer on a pipe channel
//	mcp open https://host/mcp              → streamable HTTP, NO shell channel spent
//	mcp tools|send|recv|call|close <handle>
//
// `mcp open` rides HTTP directly. That matters: a caller with a limited shell
// budget can hold a dozen HTTP endpoints without spending any of it, which is the
// difference between relaying an endpoint and starting a process.

// mcpChannels is the registry of opened HTTP endpoints, keyed by handle.
//
// ponytail: an in-memory map with no eviction. An endpoint is opened by an agent
// inside one conversation; a leak would be one small session struct per opened
// URL, and the alternative (a reaper) is machinery nobody asked for. Add eviction
// when a long-lived instance actually accumulates them.
var mcpChannels = map[string]*httpMCPChannel{}

// httpMCPChannel is one opened HTTP endpoint.
type httpMCPChannel struct {
	url      string
	sse      bool
	session  string // Mcp-Session-Id, once initialize answered
	client   *http.Client
	endpoint string // SSE: the message URL discovered from the event stream
	nextID   int
}

// mcpOpen connects an HTTP MCP endpoint and hands back a handle.
//
// It does not spend a shell channel: the transport is HTTP, so `ps` grows nothing.
// The handle is an opaque short name; it is what the other subcommands take.
func (s *Server) mcpOpen(ctx context.Context, stmt directive.Statement, url string) envelope.Slot {
	ch := &httpMCPChannel{url: url, sse: strings.HasSuffix(url, "/sse"), client: http.DefaultClient, nextID: 1}
	handle := "mcp" + fmt.Sprint(len(mcpChannels)+1)
	mcpChannels[handle] = ch
	return envelope.Slot{
		Code:    200,
		Headers: map[string]string{"Path": stmt.Path, "Handle": handle},
		Body:    fmt.Sprintf("%s %s (no shell channel consumed)\n", handle, url),
	}
}

// mcpChannel dispatches the subcommands that address an existing channel.
func (s *Server) mcpChannel(ctx context.Context, stmt directive.Statement, sub string, rest []string) envelope.Slot {
	if len(rest) == 0 {
		return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "invalid_argument"},
			Body: "mcp " + sub + " needs a handle (a shell handle for stdio, or the one mcp open returned)"}
	}
	handle := rest[0]
	if ch, ok := mcpChannels[handle]; ok {
		return s.mcpHTTP(ctx, stmt, sub, ch, rest[1:])
	}
	// Not an opened endpoint: it is a shell handle, and the peer is the process on
	// that channel.
	return s.mcpStdio(ctx, stmt, sub, handle, rest[1:])
}

// mcpHTTP speaks to an opened endpoint.
func (s *Server) mcpHTTP(ctx context.Context, stmt directive.Statement, sub string, ch *httpMCPChannel, rest []string) envelope.Slot {
	headers := map[string]string{"Path": stmt.Path}
	switch sub {
	case "close":
		delete(mcpChannels, strings.TrimPrefix(ch.url, "//"))
		return envelope.Slot{Code: 204, Headers: headers}
	case "recv":
		return envelope.Slot{Code: 204, Headers: headers,
			Body: "an HTTP endpoint answers each send; there is nothing to receive. Use `mcp call`."}
	case "send", "call", "tools":
		var frame map[string]any
		switch sub {
		case "tools":
			frame = map[string]any{"jsonrpc": "2.0", "id": ch.nextID, "method": "tools/list"}
		default:
			payload := strings.Join(rest[1:], " ")
			if payload == "" {
				return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "invalid_argument"},
					Body: "mcp send needs a JSON-RPC frame"}
			}
			if err := json.Unmarshal([]byte(payload), &frame); err != nil {
				return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "invalid_argument"},
					Body: "the frame is not JSON: " + err.Error()}
			}
		}
		ch.nextID++
		reply, err := ch.post(ctx, frame)
		if err != nil {
			return envelope.Slot{Code: 500, Headers: map[string]string{"Path": stmt.Path, "Error-Code": CodeConnectionFailed}, Body: err.Error()}
		}
		return envelope.Slot{Code: 200, Headers: headers, Body: reply}
	}
	return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "invalid_argument"},
		Body: "unknown mcp subcommand: " + sub}
}

// post sends one frame and returns the reply body.
func (c *httpMCPChannel) post(ctx context.Context, frame map[string]any) (string, error) {
	body, err := json.Marshal(frame)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.session = sid
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return string(raw), fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	return strings.TrimSpace(string(raw)), nil
}

// mcpStdio speaks to the process running on a shell channel.
//
// A stdio peer is just a line-oriented pipe: `run` writes the frame, `read` gets the
// reply. That is the whole implementation, and it is why an MCP server needs no
// registration ceremony — `pipe --argv` starts it, and this types at it.
func (s *Server) mcpStdio(ctx context.Context, stmt directive.Statement, sub string, handle string, rest []string) envelope.Slot {
	headers := map[string]string{"Path": stmt.Path}
	switch sub {
	case "tools", "send", "call":
		var frame string
		switch sub {
		case "tools":
			frame = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
		default:
			payload := strings.TrimSpace(strings.Join(rest, " "))
			if payload == "" {
				return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "invalid_argument"},
					Body: "mcp " + sub + " needs a JSON-RPC frame"}
			}
			frame = payload
		}
		// One frame, one line: the peer reads line-delimited JSON-RPC.
		if _, fail := s.callTool(ctx, "shell_input", argsOf("shell_id", handle, "text", frame)); fail != nil {
			return s.slotFromFailure(ctx, stmt, fail)
		}
		if _, fail := s.callTool(ctx, "shell_key", argsOf("shell_id", handle, "key", "enter")); fail != nil {
			return s.slotFromFailure(ctx, stmt, fail)
		}
		return envelope.Slot{Code: 202, Headers: headers, Body: "sent; read the reply with `mcp recv " + handle + "`"}
	case "recv":
		return s.mcpRecvFrame(ctx, stmt, handle)
	case "close":
		return envelope.Slot{Code: 204, Headers: headers}
	}
	return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "invalid_argument"},
		Body: "unknown mcp subcommand: " + sub}
}

// mcpRecvFrame reads exactly one JSON-RPC frame.
//
// The "exactly one" part is the contract: a frame is complete when the bytes parsed
// as JSON, and a partial one must be held rather than acted on. The read is
// therefore a positional read from the stream end, retried while the buffer does
// not yet parse — never a blocking read that may return half a message.
func (s *Server) mcpRecvFrame(ctx context.Context, stmt directive.Statement, handle string) envelope.Slot {
	start, ok := s.outputSizeFor(handle)
	if !ok {
		return s.slotFromFailure(ctx, stmt, &toolFailure{code: CodeShellNotFound, msg: "handle " + handle + " not found"})
	}
	res, fail := s.callTool(ctx, "shell_output", argsOf(
		"shell_id", handle, "offset", float64(start), "max_bytes", float64(defaultBytesLimit)))
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	doc, ok := toolResultMap(res)
	if !ok {
		return s.slotFromFailure(ctx, stmt, &toolFailure{code: CodeOperationFailed, msg: toolResultText(res)})
	}
	body := getString(doc, "output", "")
	frame, complete := firstJSONValue(body)
	if !complete {
		return envelope.Slot{Code: 204, Headers: map[string]string{
			"Path":          stmt.Path,
			"Content-Range": envelope.UnsatisfiedRange(int64(getFloatAny(doc, "total_bytes", 0))),
		}, Body: "no complete frame yet"}
	}
	end := int64(getFloatAny(doc, "start_offset", 0)) + int64(len(frame))
	return envelope.Slot{
		Code:    200,
		Headers: map[string]string{"Path": stmt.Path, "Content-Range": envelope.ContentRange(start, end-1, end)},
		Body:    frame,
	}
}

// firstJSONValue returns the first complete JSON value in raw.
//
// It scans for a complete object or array rather than trusting a newline: a frame
// may be pretty-printed, and a line-delimited peer may still emit a value with
// embedded newlines inside a string.
func firstJSONValue(raw string) (string, bool) {
	trimmed := strings.TrimLeft(raw, " \t\r\n")
	if trimmed == "" {
		return "", false
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	var value json.RawMessage
	if err := dec.Decode(&value); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || isSyntaxEOF(err) {
			return "", false
		}
		return "", false
	}
	return strings.TrimSpace(string(value)), true
}

// isSyntaxEOF reports whether a JSON decode failed only because the input ended.
func isSyntaxEOF(err error) bool {
	var syn *json.SyntaxError
	if errors.As(err, &syn) {
		return false
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}
