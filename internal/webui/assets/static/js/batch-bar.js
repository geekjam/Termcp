/* termcp Web UI — the batch bar, one implementation for every column that has one.
 *
 * The connections column and the extension column carry the same row of keys over
 * their cards: a switch that enters selection mode — its face becoming the ticked
 * count — an invert key, and a split key whose wide half runs the column's own
 * verb (dial the selection, open the selection's cards) while its caret unfolds
 * export and delete. The two columns differ only in what they list and what that
 * verb means, so they share this component instead of a copy each: the copy
 * drifted the first time one side was edited — the extension column shipped the
 * count without hiding the glyph under it, so a 34px square held both, and the
 * two toolbars no longer behaved alike.
 *
 * A caller supplies the DOM ids, how to list what is selectable, how to repaint
 * its own cards, and the three actions. The component owns the state (the mode and
 * the ticked set), paints the bar, and keeps the selection and the cards in step.
 *
 * Loaded as a plain synchronous <script> like every other module; it must come
 * before the first caller's own script (see index.html).
 */
function createBatchBar(cfg) {
  var state = { mode: false, ids: new Set() };

  function el(id) { return id ? document.getElementById(id) : null; }
  function body() { return el(cfg.bodyId); }
  function names() { return (cfg.names() || []).filter(function (n) { return !!n; }); }
  function count() {
    var n = 0;
    names().forEach(function (name) { if (state.ids.has(name)) n++; });
    return n;
  }
  /* What the column's verb acts on: the ticked names, in the order the grid shows
     them (name order), and nothing at all while the mode is off. */
  function selected() {
    if (!state.mode) return [];
    return names().filter(function (name) { return state.ids.has(name); })
      .sort(function (a, b) { return a.localeCompare(b); });
  }

  /* The switch's face: the count while there is one, the list glyph otherwise.
     Hiding the glyph is the step a copy is most likely to miss, and without it
     the two share a 34px square. */
  function paintToggle(n) {
    var key = el(cfg.toggleId);
    if (!key) return;
    var showCount = state.mode && n > 0;
    var icon = key.querySelector('svg');
    if (icon) icon.style.display = showCount ? 'none' : 'block';
    key.setAttribute('aria-pressed', String(state.mode));
    key.setAttribute('aria-label', showCount
      ? t('batch.count.selected', { count: n })
      : t(cfg.selectKey));
    var countEl = el(cfg.countId);
    if (countEl) {
      countEl.hidden = !showCount;
      countEl.textContent = String(n);
    }
  }

  /* The invert key reports whether the whole list is ticked — it shows the state
     it would flip, and its label follows. */
  function paintInvert(n) {
    var invert = el(cfg.invertId);
    if (!invert) return;
    var all = names();
    var allSelected = all.length > 0 && n === all.length;
    invert.disabled = all.length === 0;
    invert.classList.toggle('all-checked', allSelected);
    var label = allSelected ? t('section.batch.clearSelection') : t('section.batch.selectAll');
    invert.title = label;
    invert.setAttribute('aria-label', label);
  }

  /* The verb and the menu answer disabled while nothing is ticked: an always-live
     key that does nothing reads as broken. */
  function paintActions(n) {
    var split = el(cfg.splitId);
    if (split) split.classList.toggle('is-disabled', n === 0);
    (cfg.actionIds || []).forEach(function (id) {
      var item = el(id);
      if (item) item.disabled = n === 0;
    });
  }

  function update() {
    var n = count();
    paintToggle(n);
    paintInvert(n);
    paintActions(n);
    var b = body();
    if (b) b.classList.toggle('has-selection', n > 0);
  }

  /* Paint the ticked look from the set, which is the source of truth: this both
     marks the ticked cards and strips the class from every card the set no longer
     holds.
     prune drops ids the list has lost (a card deleted under the selection) and is
     the caller's to decide: a list that has not arrived yet is empty, and pruning
     against it would quietly throw away a tick someone just made. */
  function syncCards(prune) {
    if (prune) {
      var live = names();
      state.ids.forEach(function (name) { if (live.indexOf(name) < 0) state.ids.delete(name); });
    }
    var grid = el(cfg.gridId);
    if (!grid) return;
    grid.querySelectorAll(cfg.cardSelector).forEach(function (tile) {
      var name = tile.getAttribute(cfg.nameAttr);
      tile.classList.toggle('batch-selected', state.mode && state.ids.has(name));
    });
  }

  /* The per-card tooltip is the one bit of a card that follows the mode. */
  function refreshTitles() {
    var grid = el(cfg.gridId);
    if (!grid || !cfg.tipFor) return;
    grid.querySelectorAll(cfg.cardSelector).forEach(function (tile) {
      var inner = tile.querySelector('.entry-card-inner');
      if (!inner) return;
      var name = tile.getAttribute(cfg.nameAttr);
      inner.title = state.mode ? t('nethub.batch.selectOne', { name: name }) : cfg.tipFor(name);
    });
  }

  function repaint() { if (cfg.render) cfg.render(); }

  function setMode(on) {
    state.mode = !!on;
    if (!state.mode) state.ids.clear();
    var b = body();
    if (b) b.classList.toggle(cfg.selectingClass, state.mode);
    if (!state.mode) closeMenu();
    syncCards();
    refreshTitles();
    update();
  }

  function toggleSelection(name, tile) {
    if (!name) return;
    if (state.ids.has(name)) state.ids.delete(name);
    else state.ids.add(name);
    if (tile) tile.classList.toggle('batch-selected', state.mode && state.ids.has(name));
    update();
  }

  /* The caret's dropdown, and the two ways out of it: a click anywhere else, and
     Escape — which folds the menu first and the mode second, so one key walks back
     one step at a time instead of dropping both. */
  function menuOpen() { var m = el(cfg.menuId); return !!(m && !m.hidden); }
  function closeMenu() {
    var was = menuOpen();
    var m = el(cfg.menuId);
    var caret = el(cfg.caretId);
    if (m) m.hidden = true;
    if (caret) caret.setAttribute('aria-expanded', 'false');
    return was;
  }
  function toggleMenu() {
    var m = el(cfg.menuId);
    if (!m) return;
    m.hidden = !m.hidden;
    var caret = el(cfg.caretId);
    if (caret) caret.setAttribute('aria-expanded', String(!m.hidden));
  }

  (function wire() {
    var key = el(cfg.toggleId);
    if (key) key.addEventListener('click', function (e) { e.stopPropagation(); setMode(!state.mode); });

    var invert = el(cfg.invertId);
    if (invert) invert.addEventListener('click', function (e) {
      e.stopPropagation();
      var all = names();
      var allSelected = all.length > 0 && count() === all.length;
      all.forEach(function (name) {
        if (allSelected) state.ids.delete(name);
        else state.ids.add(name);
      });
      syncCards();
      update();
    });

    var caret = el(cfg.caretId);
    if (caret) caret.addEventListener('click', function (e) {
      e.preventDefault();
      e.stopPropagation();
      toggleMenu();
    });
    var menu = el(cfg.menuId);
    if (menu) menu.addEventListener('click', function (e) { e.stopPropagation(); });

    var main = el(cfg.mainId);
    if (main) main.addEventListener('click', function (e) {
      e.stopPropagation();
      if (cfg.onOpen) cfg.onOpen(selected());
    });
    var exportKey = el(cfg.exportId);
    if (exportKey) exportKey.addEventListener('click', function () {
      closeMenu();
      if (cfg.onExport) cfg.onExport(selected());
    });
    var deleteKey = el(cfg.deleteId);
    if (deleteKey) deleteKey.addEventListener('click', function () {
      closeMenu();
      if (cfg.onDelete) cfg.onDelete(selected());
    });

    document.addEventListener('click', function (e) {
      if (!menuOpen()) return;
      if (e.target && e.target.closest &&
          (e.target.closest('#' + cfg.menuId) || e.target.closest('#' + cfg.caretId))) return;
      closeMenu();
    });

    /* Escape walks back one step — the menu first, the mode second — and only for
       the column the key belongs to: two panels both listen, and folding the other
       column's mode because this one had a menu open is exactly the cross-talk a
       shared component should not introduce. "Belongs to" is the panel holding the
       focus, or nowhere in particular (a body with no focus): a key pressed while
       the user is working in the other column is that column's. */
    document.addEventListener('keydown', function (e) {
      if (e.key !== 'Escape') return;
      if (!state.mode && !menuOpen()) return;
      var t = e.target;
      var b = body();
      var loose = !t || t === document || t === document.body || t === document.documentElement;
      if (b && t && !loose && !b.contains(t)) return;
      e.stopImmediatePropagation();
      if (closeMenu()) return;
      setMode(false);
    }, true);
  })();

  update();
  return {
    isOn: function () { return state.mode; },
    has: function (name) { return state.mode && state.ids.has(name); },
    setMode: setMode,
    toggleSelection: toggleSelection,
    selected: selected,
    deselect: function (name) { state.ids.delete(name); },
    clear: function () { state.ids.clear(); },
    update: update,
    syncCards: syncCards,
    refreshTitles: refreshTitles,
    repaint: repaint,
    closeMenu: closeMenu,
    menuOpen: menuOpen,
  };
}
