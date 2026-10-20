package webui

import (
	"strings"
	"testing"
)

// TestTheDockWearsTheSharedBatchRow pins the dock to the SHARED component.
//
// The dock's batch keys were once written by hand: a private mode flag, a private
// ticked map and a private painter, all in shell-windows.js. They worked, and that
// is what made the mistake hard to see — the dock looked right while being the
// fourth copy of a component that exists precisely so a column's batch row cannot
// drift from its neighbours'. The drift this time was the shape of the thing: the
// dock grew a key set (dock-key, dock-toggle-count, dock-invert) that no other
// column had, so a change to the real row would reach three columns and miss it.
//
// So: the dock builds its row through createBatchBar, its keys wear the shared
// chip, and it owns no state of its own.
func TestTheDockWearsTheSharedBatchRow(t *testing.T) {
	shell := readAssetLF(t, "static/js/shell-windows.js")
	index := readAssetLF(t, "index.html")

	// It is a caller of the component, not a reimplementation.
	if !strings.Contains(shell, "createBatchBar({") {
		t.Fatal("the dock must build its batch row through createBatchBar")
	}
	for _, dead := range []string{"_dockSelectOn", "_dockTicked", "dockPaintBar", "dockToggleTick", "dockInvertTicks", "dockEnterSelect", "dockExitSelect"} {
		if strings.Contains(shell, dead) {
			t.Errorf("the dock still owns %s; the shared component owns the mode and the ticked set", dead)
		}
	}

	// Its keys are flat shared chips, in the markup, with the ids the config names.
	for _, want := range []string{
		`class="ext-key ext-key-icon" id="dock-sel-invert"`,
		/* Icon-only: it carried a "Stop" label, which made this key wider than the
		   two beside it for a word that repeats what the X already is. The tooltip
		   and aria-label still carry the full sentence. */
		`class="ext-key ext-key-icon ext-key-danger" id="dock-close-ticked"`,
		/* The count shares the archive's chip class and starts IN THE FLOW at zero:
		   a hidden readout cannot be told apart from a broken one, which is how the
		   dock's counter was reported ("always 0" — the number was right, the element
		   carrying it was `hidden` because the component's paint path returned early
		   for every alwaysOn caller, and the dock's onUpdate hid it at zero anyway). */
		`id="dock-toggle-count"`,
	} {
		if !strings.Contains(index, want) {
			t.Errorf("the dock's markup must carry %s", want)
		}
	}
	// No selection-mode switch, and no private key vocabulary of its own.
	for _, bespoke := range []string{
		`id="dock-select-toggle"`, `class="dock-key`, `id="dock-invert"`, `id="dock-exit-select"`,
	} {
		if strings.Contains(index, bespoke) {
			t.Errorf("the dock has grown %s; every tab carries its own tick, so there is no mode to switch", bespoke)
		}
	}
	if !strings.Contains(shell, "alwaysOn: true") {
		t.Error("the dock runs alwaysOn: the tick is the tab's own control, so there is no mode")
	}
}

// TestTheDockIsOneRowWideAndTwoRowsNarrow pins where the second row is allowed.
//
// One row could not hold a readable tab strip plus the keys that answer a
// selection plus the window controls on a phone: measured at 390px the tabs were
// down to 106px between them, narrower than the labels they carry, while the two
// key groups held 190px. So the narrow layout wraps: the strip takes a full line
// and the keys sit under it.
//
// The mistake this pins against is making that unconditional. Wrapping on a
// desktop costs 24px of terminal height at every width to hold a second row whose
// only content is a batch group that is EMPTY until something is ticked — the dock
// doubled in height, permanently, for keys nobody could see. The second row is a
// width problem, so it is scoped to the width block; the wide layout is one row
// with the strip absorbing the slack.
func TestTheDockIsOneRowWideAndTwoRowsNarrow(t *testing.T) {
	index := readAssetLF(t, "index.html")
	css := readAssetLF(t, "static/css/workspace.css")

	// Markup order is the fallback layout and the wrap order: strip, then the
	// batch group, then the window controls.
	dock := between(t, index, `class="sessdock" id="sessdock"`, `<div id="modal-forward"`)
	tabs := strings.Index(dock, `id="session-tabs-scroll"`)
	batch := strings.Index(dock, `id="dock-batch"`)
	actions := strings.Index(dock, `id="session-tabbar-actions"`)
	if tabs < 0 || batch < 0 || actions < 0 {
		t.Fatalf("the dock is missing a zone: tabs=%d batch=%d actions=%d", tabs, batch, actions)
	}
	if !(tabs < batch && batch < actions) {
		t.Errorf("the dock's zones must be ordered tabs < batch < actions (got %d, %d, %d)", tabs, batch, actions)
	}

	// The WIDE layout: one row, no wrap, and the strip is what flexes. The batch
	// group is ordered to the left end by CSS, so it can sit after the strip in
	// the markup and still lead the row.
	bar := between(t, css, ".sessdock-bar {", "}")
	if strings.Contains(bar, "flex-wrap: wrap") {
		t.Error("the base bar must not wrap: the second row belongs to the narrow block")
	}
	strip := between(t, css, ".sessdock-tabs {", "}")
	if !strings.Contains(strip, "flex: 1 1 auto") {
		t.Error("on a wide screen the strip is the flexible middle: `flex: 1 1 auto` is what absorbs the slack")
	}
	if strings.Contains(strip, "100%") {
		t.Error("`flex-basis: 100%` in the base rule would force the wrap at every width")
	}
	batchRule := between(t, css, ".sessdock-batch {", "}")
	if !strings.Contains(batchRule, "order: -1") {
		t.Error("the batch group leads the row by CSS order: it must not be the first child in the markup")
	}
	// The cap follows the shape: one row wide.
	dockRule := between(t, css, ".sessdock {", "}")
	if !strings.Contains(dockRule, "max-height: 42px") {
		t.Error("the wide dock is ONE row: its cap must be the one-row height (26px tab + padding)")
	}

	// The NARROW layout: the wrap, the two-row cap, and the right-pinned controls.
	// Anchored on the block's own comment rather than on the media query: there are
	// two narrow blocks on this page and only one of them is about the dock.
	narrow := between(t, css, "/* The dock goes to TWO rows here, and only here.", "\n}")
	if narrow == "" {
		t.Fatal("the dock's narrow block is gone")
	}
	if !strings.Contains(narrow, ".sessdock-bar { flex-wrap: wrap") {
		t.Error("the narrow block is what turns the dock into two rows")
	}
	if !strings.Contains(narrow, ".sessdock { max-height: 70px; }") {
		t.Error("two rows need the taller cap, and only the narrow block may raise it")
	}
	if !strings.Contains(narrow, ".sessdock-actions { margin-left: auto; }") {
		t.Error("with the strip on its own line, the window controls need the auto margin to reach the right edge")
	}
	if !strings.Contains(narrow, ".sessdock-empty { left: 10px") {
		t.Error("the empty note must clear the batch group on a wide screen and not on a narrow one")
	}
}

// TestTheDockTickIsATabControl pins the monitor glyph as the tick target.
//
// The tab does two things and needs two targets: the glyph ticks (for a batch
// close) and the rest of the tab raises the window. That split is what lets this
// bar have no selection mode at all — the same arrangement the archive's rows use.
// The glyph must therefore be a real button (keyboard and screen readers), and a
// click on the tab body must never tick.
func TestTheDockTickIsATabControl(t *testing.T) {
	shell := readAssetLF(t, "static/js/shell-windows.js")
	start := strings.Index(shell, "function refreshSessionTabbar(")
	if start < 0 {
		t.Fatal("refreshSessionTabbar is gone")
	}
	fn := shell[start : start+strings.Index(shell[start:], "\n}\n")]
	if !strings.Contains(fn, `'<button type="button" class="session-tab-tick"`) {
		t.Error("the tab's tick must be a real <button>: it is the only tick target on the row")
	}
	if !strings.Contains(fn, "e.target.closest('.session-tab-tick')") {
		t.Error("a click on the glyph must tick, and only that click")
	}
	// The tab body raises the window — always, with no mode to consult.
	if !strings.Contains(fn, "focusShellWindow(w);") {
		t.Error("a tab click must still raise its window")
	}
	if strings.Contains(fn, "_dockBatch.isOn()") {
		t.Error("the tab must not branch on a selection mode: this bar has none")
	}
}

// TestTheDockBatchCloseArchivesAndDoesNotDelete pins what the batch's verb does.
//
// The dock is the only list of open sessions, so its close key is the page's main
// way to end several at once. It must STOP AND ARCHIVE: POST /terminate keeps the
// transcript, DELETE erases it, and the page no longer offers to erase a session
// from anywhere but the archive column.
func TestTheDockBatchCloseArchivesAndDoesNotDelete(t *testing.T) {
	shell := readAssetLF(t, "static/js/shell-windows.js")
	start := strings.Index(shell, "function dockCloseWindows(")
	if start < 0 {
		t.Fatal("shell-windows.js no longer defines dockCloseWindows")
	}
	end := strings.Index(shell[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not delimit dockCloseWindows")
	}
	body := shell[start : start+end]
	if !strings.Contains(body, "archiveSessionById(") {
		t.Error("the batch close must archive through archiveSessionById")
	}
	if strings.Contains(body, "deleteSessionById(") {
		t.Error("the batch close must not delete: the archive column is where erasing lives")
	}
	// A window with no session behind it (a connecting window, a status card)
	// must not reach the archive endpoint at all.
	if !strings.Contains(body, "_placeholder") || !strings.Contains(body, "_extName") {
		t.Error("a placeholder or a status card must be closed, not archived: they carry no session id")
	}
}

// TestTheCloseDialogCountsEveryWindowItWillClose pins the number.
//
// The count and the action have to use the SAME predicate, and they did not. The
// action stopped every window carrying a session id; the count additionally excluded
// `_placeholder`, i.e. every window still dialling. With four windows mid-connect and
// two up, ticking all six and pressing close announced "stop and archive 2 running
// sessions" and then stopped six — and a confirmation whose number is wrong is worse
// than no number, because that number is read as the scope of what is being confirmed.
//
// `_placeholder` is the wrong test on its own anyway: a placeholder starts with
// `_sid = null` and gains the id when the dial succeeds, so a window can be a
// placeholder AND own a session. The id is the only thing that says whether there is
// something to stop.
func TestTheCloseDialogCountsEveryWindowItWillClose(t *testing.T) {
	js := readAssetLF(t, "static/js/shell-windows.js")
	fn := between(t, js, "function dockCloseWindows(ids) {", "\n}\n")
	if fn == "" {
		t.Fatal("dockCloseWindows is gone")
	}
	// The count: a session id and not an extension card.
	countPred := between(t, fn, "var stopping = wins.filter(function (w) {", "});")
	if countPred == "" {
		t.Fatal("the dialog's count predicate is gone")
	}
	if strings.Contains(countPred, "_placeholder") {
		t.Error("the count excludes placeholder windows while the action stops their sessions: the number in the dialog would be too small")
	}
	if !strings.Contains(countPred, "w._sid") {
		t.Error("the count must key on the session id — it is the only proof there is something to stop")
	}
	// The action: the same test, so the two can never drift again.
	act := between(t, fn, "wins.forEach(function (w) {", "/* No session behind it")
	if act == "" {
		t.Fatal("the close action is gone")
	}
	/* The slice includes the comment above the branch, which mentions
	   `_placeholder` by name while explaining why it is gone. Only the CODE matters,
	   so the condition line is what gets checked. */
	cond := between(t, act, "if (w._sid", "{")
	if cond == "" {
		t.Fatal("the close action's condition is gone")
	}
	if strings.Contains(cond, "_placeholder") {
		t.Error("the close action skips placeholder windows that already own a session, leaving an orphan running on the server")
	}
	// And the dialog counts WINDOWS: the question it asks is "how many of these am I
	// closing", which is the size of the selection the user made.
	if !strings.Contains(fn, "count: wins.length") {
		t.Error("the message must report the number of windows being closed, not a subset of them")
	}
}
