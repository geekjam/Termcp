package mcp

import (
	"encoding/json"
	"testing"
)

// TokenBudgetGuard prevents regression: the tools/list and instructions payloads
// must stay within defined limits. These values are injected into the model
// context every turn, so every byte matters.
func TestTokenBudgetGuard(t *testing.T) {
	s := New(nil, nil, nil, nil, "test")
	s.RegisterSSHConfigWriteTools()

	// Measure the listing as the model receives it, decorated address included:
	// that per-request decoration is part of the payload this guard protects.
	tools := listTools(t, s, originContext("http://127.0.0.1:18765"))
	if len(tools) != 32 {
		t.Fatalf("expected 32 tools, got %d", len(tools))
	}

	total := 0
	descBytes := 0
	propDescBytes := 0
	for _, st := range tools {
		b, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		total += len(b)
		descBytes += len(st.Description)
		props := st.InputSchema.Properties
		for _, pv := range props {
			pm, ok := pv.(map[string]any)
			if !ok {
				continue
			}
			if d, ok := pm["description"].(string); ok {
				propDescBytes += len(d)
			}
		}
	}
	// Instructions are a fixed constant again: the address is not duplicated
	// there, because `instructions` is optional and often dropped.
	instructionsLen := len(mcpServerInstructions)

	t.Logf("tools/list total: %d B", total)
	t.Logf("tool descriptions: %d B", descBytes)
	t.Logf("property descriptions: %d B", propDescBytes)
	t.Logf("instructions: %d B", instructionsLen)

	// Budgets (bytes, rough 4:1 B:tokl ratio for English text). The per-tool
	// budgets scale with the tool count: they are averages over the whole
	// surface, and a surface that grows past its headroom is expected to raise
	// these rather than shed descriptions.
	//
	// The property budget was raised from 5000 to 5300 when the extension tool
	// gained the endpoint form's two fields (url, headers): a template that names
	// an HTTP MCP server is the shape most real servers have, and a field the
	// model cannot read the meaning of is one it will set wrongly. The headroom is
	// deliberate — 5167 B is what the surface measures, so a further addition has
	// to be argued for rather than fit into the slack.
	if total > 25000 {
		t.Errorf("tools/list total %d B exceeds 25000 B budget", total)
	}
	if descBytes > 5000 {
		t.Errorf("tool descriptions %d B exceeds 5000 B budget", descBytes)
	}
	if propDescBytes > 5300 {
		t.Errorf("property descriptions %d B exceeds 5300 B budget", propDescBytes)
	}
	if instructionsLen > 2600 {
		t.Errorf("instructions %d B exceeds 2600 B budget", instructionsLen)
	}
}

// TestDeferLoadingPolicy locks the split between always-loaded core tools and
// deferred (on-demand) tools, and the default that governs whether the marker
// is emitted at all.
//
// The classification is a property of termcp; whether it reaches the wire is a
// deployment choice. Both halves matter: a model that must search before it can
// type a command pays a round trip on every interaction, so the core driving
// loop must never be deferred, and clients that drop defer_loading must still
// see the whole surface unless the operator opted in.
func TestDeferLoadingPolicy(t *testing.T) {
	core := []string{
		"session_start", "session_list", "session_info",
		"session_terminate", "session_delete",
		"shell_open", "shell_list", "shell_close", "shell_input", "shell_key", "shell_output",
		"notify_user",
	}
	isCore := make(map[string]bool, len(core))
	for _, name := range core {
		isCore[name] = true
	}

	collect := func(t *testing.T, opts ...Option) map[string]bool {
		t.Helper()
		s := New(nil, nil, nil, nil, "test", opts...)
		s.RegisterSSHConfigWriteTools()
		byName := make(map[string]bool)
		for _, st := range s.mcpServer.ListTools() {
			byName[st.Tool.Name] = st.Tool.DeferLoading
		}
		return byName
	}

	// Default: nothing is deferred, so a client that does not understand the
	// marker still receives every tool.
	def := collect(t)
	for name, deferred := range def {
		if deferred {
			t.Errorf("default config marked %q defer_loading; the marker must be opt-in", name)
		}
	}

	// partial: the hot path stays eager, the wide/low-frequency surfaces defer.
	on := collect(t, DeferLevel(DeferPartial))
	for _, name := range core {
		deferred, ok := on[name]
		if !ok {
			t.Errorf("core tool %q is not registered", name)
			continue
		}
		if deferred {
			t.Errorf("core tool %q must not be deferred (it is on the hot path)", name)
		}
	}
	for name := range deferredTools {
		got, ok := on[name]
		if !ok {
			t.Errorf("deferredTools lists %q, which is not a registered tool (stale entry)", name)
			continue
		}
		if !got {
			t.Errorf("tool %q is listed in deferredTools but was not marked defer_loading", name)
		}
	}

	// Every registered tool must be classified, so a new tool cannot silently
	// default into the always-loaded set and inflate every client's context.
	for name := range on {
		if deferredTools[name] || isCore[name] {
			continue
		}
		t.Errorf("tool %q is neither in the core list nor deferredTools; classify it", name)
	}

	// all: only the minimum driving loop stays eager; everything else —
	// including the partial-level core — defers. A client that supports
	// deferred loading pays one search round trip per first use; that is the
	// level's whole point.
	all := collect(t, DeferLevel(DeferAll))
	for name := range all {
		if !allLevelCore[name] && !deferredTools[name] && !isCore[name] {
			t.Errorf("tool %q is neither all-level core nor classified; it would silently go eager under level=all", name)
		}
	}
	for name := range allLevelCore {
		deferred, ok := all[name]
		if !ok {
			t.Errorf("all-level core tool %q is not registered", name)
			continue
		}
		if deferred {
			t.Errorf("all-level core tool %q must stay eager: the minimum driving loop cannot require a search", name)
		}
	}
}
