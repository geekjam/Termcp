package webui

import (
	"strings"
	"testing"
)

// TestExtensionPanelWearsTheBatchChrome pins that the extension column is the
// connections column's other side in the chrome as well as in the data: the same
// batch keys on top (select, invert, open a terminal per ticked extension, export
// the selection, delete it) and the same pair of bottom keys (author one, import
// a set). The classes are the ones workspace.css already dresses, so the two
// columns cannot drift into looking like different controls.
func TestExtensionPanelWearsTheBatchChrome(t *testing.T) {
	index := readAssetLF(t, "index.html")
	for _, want := range []string{
		`id="ext-select-toggle"`,
		`class="nethub-select-toggle" id="ext-select-toggle"`,
		`id="ext-toggle-count"`,
		`id="ext-sel-invert"`,
		`id="ext-open-split"`,
		`id="ext-batch-open"`,
		`id="ext-open-caret"`,
		`id="ext-menu-export"`,
		`id="ext-menu-delete"`,
		`id="ext-add"`,
		`id="ext-import-open"`,
		`class="drawer-add drawer-add-grow" id="ext-add"`,
		`class="drawer-add drawer-import" id="ext-import-open"`,
	} {
		if !strings.Contains(index, want) {
			t.Errorf("the extension panel is missing %s", want)
		}
	}
	// The sub-keys (keyboard help, the disabled state) restate nothing: they are
	// the same plate and the same icon set the host toolbar uses.
	if !strings.Contains(index, `class="nethub-batch-icon" id="ext-sel-invert"`) ||
		!strings.Contains(index, `class="nethub-menu-item nethub-menu-danger" id="ext-menu-delete"`) {
		t.Error("the extension batch keys must wear the host toolbar's own classes")
	}
}

// TestBothColumnsWearOneBatchBar pins the shared component and the drift that
// motivated it: the extension column's copy of the host column's batch bar had
// shipped the ticked count without hiding the glyph underneath, so one 34px
// square held both, and the two toolbars had stopped behaving alike. There is one
// implementation now, instantiated once per column.
func TestBothColumnsWearOneBatchBar(t *testing.T) {
	bar := readAssetLF(t, "static/js/batch-bar.js")
	for _, want := range []string{
		// The step a copy is most likely to miss.
		"icon.style.display = showCount ? 'none' : 'block'",
		// The selection is a set, never the DOM.
		"var state = { mode: false, ids: new Set() };",
		// The switch's face, the invert key's all-checked state, the disabled verb.
		"key.setAttribute('aria-pressed', String(state.mode))",
		"invert.classList.toggle('all-checked', allSelected)",
		"split.classList.toggle('is-disabled', n === 0)",
		// Escape walks back one step — the menu, then the mode — and only for the
		// column the key belongs to (the panel holding the focus, or nowhere in
		// particular), so two bars cannot fold each other.
		"if (closeMenu()) return;",
		"setMode(false)",
		"var loose = !t || t === document || t === document.body",
		"if (b && t && !loose && !b.contains(t)) return;",
	} {
		if !strings.Contains(bar, want) {
			t.Errorf("batch-bar.js is missing %q", want)
		}
	}
	// Both columns build their bar from it, and neither keeps a copy.
	for _, f := range []string{"static/js/dialogs.js", "static/js/extensions.js"} {
		src := readAssetLF(t, f)
		if !strings.Contains(src, "createBatchBar({") {
			t.Errorf("%s must build its bar from the shared component", f)
		}
		for _, dead := range []string{"_nodeSelIds", "_extSelIds", "selectableNodeNames", "selectedExtensionNames", "updateNodeBatchBar", "updateExtBatchBar", "syncNodeCardSelection", "syncExtCardSelection"} {
			if strings.Contains(src, dead) {
				t.Errorf("%s still carries its own %q; the component owns the selection", f, dead)
			}
		}
	}
	// It must be defined before the panels that instantiate it.
	index := readAssetLF(t, "index.html")
	barAt := strings.Index(index, "static/js/batch-bar.js")
	firstCaller := strings.Index(index, "static/js/dialogs.js")
	if barAt < 0 || firstCaller < 0 || barAt > firstCaller {
		t.Error("batch-bar.js must load before the first panel that builds one")
	}
}

// TestExtensionPanelBatchBehaviour pins what this column's verbs do, by the calls
// they make: the export goes through the shared TOML download, the delete is the
// same comma-batch the session and profile routes use — so an extension is deleted
// exactly like a profile is — and the verb itself opens the selection's cards.
func TestExtensionPanelBatchBehaviour(t *testing.T) {
	ext := readAssetLF(t, "static/js/extensions.js")
	for _, want := range []string{
		// Export: one shared download helper, one export route.
		"return downloadTOML(path, 'termcp-extensions.toml')",
		"apiPath('/api/extensions/batch')",
		// Delete: the comma batch the other resources already share.
		"deleteResourcesBatch('extensions', names)",
		// Import: the panel's other bottom key.
		"apiPath('/api/extensions/batch'), {\n      method: 'POST'",
		"showModal('modal-ext-import')",
		// The verb the split key runs, handed the selection by the component.
		"function openSelectedExtensions(names) {",
		"onOpen: function (names) { openSelectedExtensions(names); },",
	} {
		if !strings.Contains(ext, want) {
			t.Errorf("extensions.js is missing %q", want)
		}
	}
	// A card click opens its card (a window); in select mode the same click ticks.
	// A readout opens centred — null is the cascade, not the pointer's coordinates,
	// so a status card never lands wherever the mouse happened to be.
	if !strings.Contains(ext, "if (_extBatch.isOn()) { _extBatch.toggleSelection(name, tile); return; }") ||
		!strings.Contains(ext, "openExtensionWindow(name, null);") {
		t.Error("a card click must open the extension's card, centred, unless the column is selecting")
	}
	// The batch key opens one card per ticked extension — the same act as clicking
	// each of them, which is what a host batch does for sessions.
	if !strings.Contains(ext, "names.forEach(function (name) { openExtensionWindow(name, null); });") {
		t.Error("the batch open key must open one card per selected extension")
	}
	if !strings.Contains(ext, "openExtensionDialog(null)") {
		t.Error("the New key must open the configuration editor")
	}
	// A removed template takes its own card with it, on the BATCH path too: the
	// single delete closes the card (deleteOneExtension's second argument), and the
	// batch used to leave one orphan window per name saying the extension is gone.
	batchStart := strings.Index(ext, "function deleteSelectedExtensions(names) {")
	if batchStart < 0 {
		t.Fatal("extensions.js no longer defines deleteSelectedExtensions")
	}
	batch := ext[batchStart:]
	if end := strings.Index(batch, "\n}\n"); end >= 0 {
		batch = batch[:end]
	}
	if !strings.Contains(batch, "var win = extWinFor(r.id);") || !strings.Contains(batch, "if (win) closeShellWindow(win);") {
		t.Error("the batch delete must close the card of each extension it removed, as the single delete does")
	}
}

// TestExtensionCardIsAWindowAndConfigIsAModal pins the two shapes apart: a card
// click opens the extension's status as a WINDOW — the kind a session opens, so
// several coexist, drag and tile together — and the gear opens the configuration
// as a modal, which is one thing at a time and one TOML document.
//
// The window is a session window with the terminal taken out, so what it must NOT
// do is as load-bearing as what it does: no session behind it means no _parentSid
// (a session frame would close it), no _placeholder (the Sessions plate would grow
// a card for it) and no _channels, and its close is its own (the shared binding
// would offer the archive/delete dialog for a session that does not exist).
func TestExtensionCardIsAWindowAndConfigIsAModal(t *testing.T) {
	index := readAssetLF(t, "index.html")
	for _, want := range []string{
		`id="ext-f-name"`,
		`id="ext-f-toml"`,
		`id="modal-ext-delete"`,
		`id="modal-ext-save"`,
	} {
		if !strings.Contains(index, want) {
			t.Errorf("the configuration modal is missing %s", want)
		}
	}
	// What the panel retired along the way: the tabs, the field form, the key that
	// raised a terminal, and the status scaffold that lived inside the modal before
	// the status became a window.
	for _, dead := range []string{
		`id="ext-tab-status"`, `id="ext-tab-edit"`, `id="ext-tabs"`,
		`id="ext-page-status"`, `id="ext-page-edit"`, `id="ext-page-config"`,
		`id="ext-form-view"`, `id="ext-config-view"`, `id="ext-toggle-view"`,
		`id="ext-status-open"`, `id="ext-status-close"`,
		`id="ext-f-kind"`, `id="ext-f-command"`, `id="ext-f-allow"`, `id="ext-f-enabled"`,
	} {
		if strings.Contains(index, dead) {
			t.Errorf("the markup still carries %s, which the panel no longer shows", dead)
		}
	}

	ext := readAssetLF(t, "static/js/extensions.js")
	win := between(t, ext, "function openExtensionWindow(name, pos) {", "\n/* paintExtensionWindow")
	for _, want := range []string{
		// ext-window is what the sizing rule hangs off: without it the card keeps the
		// shared 240px terminal floor and sits in half a window of empty glass.
		"win.className = 'shell-window ext-window'",
		"win._sid = name",
		"win._extName = name",
		"win._streamDone = true",
		"bindShellWindowMinButton(",
		"bindShellWindowMaxButton(",
		"setupShellWindowDrag(",
		"setupShellWindowResize(",
		"closeShellWindow(win)",
		// Both dimensions are the content's: placement's 640 is measured against the
		// settled width and the box is moved to the centre line the click asked for.
		// Both writes are inside the desktop branch — an inline width/height beats a
		// media query, so writing either on touch shrinks a full-screen card to its
		// content (the .win-fullscreen rule owns 100vw x 100dvh there).
		"if (!isMobileViewport()) {",
		"win.style.width = 'max-content'",
		"win.style.height = 'auto'",
		"recentreShellWindowForSize(win)",
	} {
		if !strings.Contains(win, want) {
			t.Errorf("the card window is missing %q", want)
		}
	}
	// The height write must sit after the guard, not before it: a stray
	// `win.style.height` above the branch is the regression, and it is invisible to
	// the string assertions above.
	if h := strings.Index(win, "win.style.height"); h >= 0 {
		if g := strings.Index(win, "if (!isMobileViewport())"); g < 0 || h < g {
			t.Error("the card's inline height is written outside the desktop branch; on touch it would defeat .win-fullscreen")
		}
	}
	for _, never := range []string{"_parentSid", "_placeholder", "_channels", "bindShellWindowCloseButton", "_initShellWindowUI"} {
		if strings.Contains(win, never) {
			t.Errorf("a window with no session must not carry %s", never)
		}
	}
	// The gear opens the configuration; the card opens its window.
	if !strings.Contains(ext, "openExtensionDialog(name)") || !strings.Contains(ext, `class="conn-edit-btn"`) {
		t.Error("the configuration must be reached through the gear the host cards also wear")
	}
	// Removal is the configuration's business (and the batch row's); the card
	// carries no trash key of its own.
	if strings.Contains(ext, "ext-del-btn") || !strings.Contains(ext, "deleteOneExtension") {
		t.Error("removal belongs to the configuration modal and the batch, not to the card")
	}
	// The page reads and writes the file itself: one content type, and the name in
	// the query (a document has no name key — the directory is the name).
	if !strings.Contains(ext, "encodeURIComponent(name) + '?format=toml'") {
		t.Error("the configuration must read the file the store holds")
	}
	if !strings.Contains(ext, "headers: { 'Content-Type': 'application/toml; charset=utf-8' }") {
		t.Error("the configuration must save the document itself")
	}
	if !strings.Contains(ext, "query = '?name=' + encodeURIComponent(newName)") {
		t.Error("a create must carry its name, which the file itself has no place for")
	}
	if !strings.Contains(ext, "area.value = extTomlSeed();") {
		t.Error("a create must start from the default template")
	}
	if !strings.Contains(ext, "# A termcp extension. kind + target is enough") {
		t.Error("the default template must explain its own fields")
	}
}

// TestExtensionPanelIsResponsive pins the narrow behaviour the column shares with
// the connections panel — the same drawer, the same trigger, the same shared
// scrim — plus the card shape the extension cards need at that width.
func TestExtensionPanelIsResponsive(t *testing.T) {
	ext := readAssetLF(t, "static/js/extensions.js")
	if !strings.Contains(ext, "return (window.innerWidth || 0) < 800;") {
		t.Error("the extension column must become a drawer below the same breakpoint the host list uses")
	}
	if !strings.Contains(ext, "paintDrawerScrim()") {
		t.Error("the drawer must share the page scrim with the host list")
	}
	css := readAssetLF(t, "static/css/workspace.css")
	for _, want := range []string{
		// The cards fill the column at both widths rather than wrapping at the
		// entry card's own minimum.
		"#ext-grid .conn-tile.entry-card { width: 100%; min-width: 0; max-width: none; }",
		// The keys are revealed by THIS column's class, and the host row's by its
		// own: one selector list, two hosts, no cross-lighting.
		".nethub-selecting .nethub-batch-icon,\n.ext-selecting .nethub-batch-icon { display: inline-flex;",
		".nethub-selecting .nethub-open-split,\n.ext-selecting .nethub-open-split { display: block;",
		".nethub-selecting .conn-tile.node-card.batch-selected,\n.ext-selecting .conn-tile.ext-card.batch-selected {",
		// A ticked extension card reads as armed, exactly as a ticked host does.
		".ext-selecting .conn-tile.ext-card .entry-card-inner { cursor: pointer; }",
		// The dialog's status readout folds its labels on a phone.
		".ext-status-label { flex: 0 0 68px; font-size: .56rem; }",
		"#ext-f-toml { width: 100%; min-height: 240px;",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("workspace.css is missing %q", want)
		}
	}
	// Neither column overrides the split key: both carry the same two halves, so a
	// panel-scoped rule for its width or its divider is a bug waiting to happen (it
	// survived one round of the panel's open key coming and going).
	if strings.Contains(css, ".extpanel-body .nethub-open-split") ||
		strings.Contains(css, ".extpanel-body .batch-open-caret") {
		t.Error("the split key is shared; neither column re-dresses it on its own")
	}
	// The keys the rework retired: the card's own field editor (the gear opens the
	// configuration now), its trash key (removal is the configuration's business and
	// the batch row's), and a New key of its own (the column closes with the shared
	// bottom-row pair).
	//
	// Plus two shapes the rework left behind: .ext-openable dressed a cursor that no
	// element ever set (every card here opens its own readout), and .ext-status-section
	// divided a multi-extension readout that a dialog was going to list — the panel
	// opens one window per extension instead, so nothing ever wore it. .ext-check-row
	// belonged to the field form the TOML editor replaced.
	for _, dead := range []string{".ext-edit-btn", ".ext-del-btn", ".ext-add-btn", ".ext-openable", ".ext-status-section", ".ext-check-row"} {
		if strings.Contains(css, dead) {
			t.Errorf("workspace.css still dresses %s, which no element wears", dead)
		}
	}
	// The card's body inside its window, and the window's own size: both dimensions
	// are the content's — the shared 640x480 default and the 240px terminal floor
	// are a terminal's, and a five-row readout sat in half a window of empty glass.
	// The ceilings keep a long value or a long failure history inside the window,
	// where the body scrolls.
	for _, want := range []string{
		`.shell-window.ext-window:not(.win-fullscreen) {
  width: max-content; max-width: min(640px, calc(100vw - 16px));
  height: auto; min-width: 0; min-height: 0; max-height: 70vh;
}`,
		".ext-window .ext-win-body { flex: 1 1 auto; min-height: 0; overflow: auto; }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("workspace.css is missing %q", want)
		}
	}
	// The cyberpunk theme is a full copy of the panel's sheet, so it must carry
	// the same two-host selectors.
	cyber := readAssetLF(t, "themes/cyberpunk/assets/static/css/workspace.css")
	for _, want := range []string{
		".ext-selecting .nethub-batch-icon {",
		".ext-selecting .nethub-open-split {",
		".ext-selecting .conn-tile.ext-card .entry-card-inner { cursor: pointer; }",
		".ext-status-row {",
		// The window's size, carried as a copy: a theme that misses it shows the
		// fixed box again.
		`.shell-window.ext-window:not(.win-fullscreen) {
  width: max-content; max-width: min(640px, calc(100vw - 16px));
  height: auto; min-width: 0; min-height: 0; max-height: 70vh;
}`,
	} {
		if !strings.Contains(cyber, want) {
			t.Errorf("the cyberpunk theme is missing %q", want)
		}
	}
	// A full copy of the sheet is also a full copy of its dead code: the theme must
	// not keep dressing what the panel retired.
	for _, dead := range []string{".ext-openable", ".ext-status-section", ".ext-check-row"} {
		if strings.Contains(cyber, dead) {
			t.Errorf("the cyberpunk theme still dresses %s, which no element wears", dead)
		}
	}
}

// TestExtensionToolbarHasOneTick pins the toolbar's iconography at the level it
// broke: the invert key carries both of its icons (hollow and solid) and shows one
// at a time. The rule that hides the inactive one was written against the host
// panel's id, so the extension panel's own key rendered both — two ticks in one
// key. It is scoped by the class both panels share now, which is what this pins.
func TestExtensionToolbarHasOneTick(t *testing.T) {
	for _, sheet := range []string{
		"static/css/workspace.css",
		"themes/cyberpunk/assets/static/css/workspace.css",
	} {
		css := readAssetLF(t, sheet)
		if !strings.Contains(css, ".nethub-batch-icon .ic-solid { display: none; }") {
			t.Errorf("%s: the invert key must show one icon per state, scoped by class", sheet)
		}
		for _, idScoped := range []string{"#nethub-sel-invert", "#nethub-batch-open", "#nethub-open-caret"} {
			if strings.Contains(css, idScoped) {
				t.Errorf("%s: %s scopes shared chrome to one panel's id; the other panel's keys miss the rule", sheet, idScoped)
			}
		}
	}
	// Both panels' keys are addressed by name, and both carry the same glyphs.
	index := readAssetLF(t, "index.html")
	// Both columns carry an open key, and they mean different things: the host
	// list's dials a session for each ticked host, this column's opens the CARDS of
	// the selection — an extension's session is already up, so its card is what
	// there is to open, and with several ticked that is one window each.
	panel := between(t, index, `<aside class="extpanel" id="extpanel">`, "</aside>")
	if !strings.Contains(panel, `class="batch-open-main" id="ext-batch-open"`) {
		t.Error("the extension batch row must carry its open key")
	}
	if n := strings.Count(index, `class="batch-open-main"`); n != 2 {
		t.Errorf("both columns open something, found %d open keys", n)
	}
	if n := strings.Count(index, `class="batch-open-caret"`); n != 2 {
		t.Errorf("both panels' caret keys need the shared class, found %d", n)
	}
	host := between(t, index, `<aside class="nethub" id="nethub">`, "</aside>")
	const switchGlyph = `M3.1 4.1l1.1 1.1 2.2-2.4`
	if strings.Contains(panel, switchGlyph) != strings.Contains(host, switchGlyph) {
		t.Error("the two panels' select switches must wear the same glyph")
	}
}

// TestExtensionEmptyStateHasNoKeyOfItsOwn pins that the empty column says what an
// extension is and nothing else: the column already closes with New and Import,
// and a third "create one" in the middle is the same action twice.
func TestExtensionEmptyStateHasNoKeyOfItsOwn(t *testing.T) {
	ext := readAssetLF(t, "static/js/extensions.js")
	empty := between(t, ext, "function renderExtensionEmpty()", "\n}\n")
	if strings.Contains(empty, "<button") || strings.Contains(empty, "ext-empty-add") {
		t.Errorf("the empty state must not carry a key of its own:\n%s", empty)
	}
	if !strings.Contains(empty, "t('ext.empty.text')") {
		t.Error("the empty state still has to say what an extension is")
	}
	// The styles and the string that dressed that key are gone with it.
	css := readAssetLF(t, "static/css/workspace.css")
	if strings.Contains(css, "ext-empty-btn") {
		t.Error("workspace.css still dresses the retired empty-state key")
	}
	cyber := readAssetLF(t, "themes/cyberpunk/assets/static/css/workspace.css")
	if strings.Contains(cyber, "ext-empty-btn") {
		t.Error("the cyberpunk theme still dresses the retired empty-state key")
	}
}
