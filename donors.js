/* Donor names come from donors.json beside this page, so the list grows
   without a markup edit. Entries are {name, kind?, url?, note?} — a bare
   string works too, and defaults to a human donor. `kind: "agent"` marks an
   AI maintainer; `note` is what they did, and it is read in the card the chip
   opens — a title tooltip truncates it, a card does not. Only the first 100
   are shown. Amounts are deliberately not shown. */
let donorData = null;
let openDonor = null;

function closeDonor() {
  if (!openDonor) return;
  openDonor.card.hidden = true;
  openDonor.chip.setAttribute('aria-expanded', 'false');
  openDonor = null;
}

/* The card hangs under its chip and is then nudged back inside the viewport:
   a chip on the row's right edge would open a card half off the screen, and
   one near the bottom would open it below the fold. Both nudges are classes,
   reset first so a re-open measures the default placement, not the last one. */
function placeDonorCard(chip, card) {
  const pad = 12;
  card.classList.remove('flip', 'up');
  let rect = card.getBoundingClientRect();
  if (rect.right > document.documentElement.clientWidth - pad) {
    card.classList.add('flip');
    rect = card.getBoundingClientRect();
  }
  if (rect.bottom > window.innerHeight - pad &&
      chip.getBoundingClientRect().top - rect.height - 8 > pad) {
    card.classList.add('up');
  }
}

function toggleDonorCard(chip, card) {
  if (openDonor && openDonor.card === card) { closeDonor(); return; }
  closeDonor();
  card.hidden = false;
  chip.setAttribute('aria-expanded', 'true');
  placeDonorCard(chip, card);
  openDonor = { chip, card };
}

/* A note is either one string or one per language. An entry written before the
   card existed carries the single string, so that shape has to keep working. */
function pickNote(note) {
  if (!note) return '';
  if (typeof note === 'string') return note;
  return note[currentLang] || note['zh-CN'] || note.en || '';
}

function renderDonors() {
  const box = document.getElementById('donor-list');
  if (!box) return;
  /* The list is rebuilt from scratch below, so whatever card was open is gone. */
  closeDonor();
  const list = (Array.isArray(donorData) ? donorData : []).slice(0, 100);
  box.textContent = '';
  if (!list.length) {
    const empty = document.createElement('span');
    empty.className = 'donors-empty';
    empty.textContent = t('donors_empty');
    box.appendChild(empty);
    return;
  }
  list.forEach((entry, index) => {
    const donor = typeof entry === 'string' ? { name: entry } : (entry || {});
    if (!donor.name) return;
    const kind = donor.kind === 'agent' ? 'agent' : 'human';
    /* Where the chip can go, if anywhere: an explicit url, else the GitHub
       handle a donor is named after. It is reached from the card now, not by
       clicking the chip — the chip answers "what did they do" first. */
    const href = donor.url || (/^[A-Za-z0-9-]+$/.test(donor.name) ? 'https://github.com/' + donor.name : null);
    /* What this person or agent actually did, from donors.json. A note may be
       one string or one per language ({'zh-CN': …, en: …}); the card shows it
       at length, so a Chinese-only line would be the whole card for a reader
       who picked English. Falls back to the generic line when there is none. */
    const note = pickNote(donor.note) || t('donor_hint');

    /* The card lives beside the chip rather than inside it: the chip is
       chamfered with a clip-path, and a clipped parent clips its popups. */
    const wrap = document.createElement('span');
    wrap.className = 'donor-wrap';

    const el = document.createElement('button');
    el.type = 'button';
    el.className = 'donor';
    el.setAttribute('data-kind', kind);
    el.setAttribute('aria-expanded', 'false');
    el.title = note;
    /* An icon, not a word: the same shape the transcript uses for the same
       kind of line. Role is still exposed as text for screen readers. */
    const kindTag = document.createElement('span');
    kindTag.className = 'donor-kind';
    kindTag.setAttribute('role', 'img');
    kindTag.setAttribute('aria-label', t(kind === 'agent' ? 'donor_agent' : 'donor_human'));
    kindTag.innerHTML = kind === 'agent'
      ? '<svg viewBox="0 0 24 24"><rect x="4" y="7" width="16" height="12" rx="2"/><path d="M12 7V4M9 12v2M15 12v2M2 11v4M22 11v4"/></svg>'
      : '<svg viewBox="0 0 24 24"><circle cx="12" cy="8" r="3.5"/><path d="M5 20a7 7 0 0 1 14 0"/></svg>';
    const nameTag = document.createElement('span');
    nameTag.className = 'donor-name';
    nameTag.textContent = donor.name;
    el.appendChild(kindTag);
    el.appendChild(nameTag);

    const card = document.createElement('div');
    card.className = 'donor-card';
    card.id = 'donor-card-' + index;
    card.hidden = true;
    el.setAttribute('aria-controls', card.id);

    const noteEl = document.createElement('p');
    noteEl.className = 'donor-card-note';
    noteEl.textContent = note;
    card.appendChild(noteEl);

    if (href) {
      const link = document.createElement('a');
      link.className = 'donor-card-link';
      link.href = href;
      link.target = '_blank';
      link.rel = 'noopener';
      /* "主页", not "GitHub 主页": a url in donors.json may point anywhere, and
         the handle-derived fallback is only one of the places it can go. */
      link.textContent = t('donor_home');
      const arrow = document.createElement('span');
      arrow.className = 'donor-card-arrow';
      arrow.setAttribute('aria-hidden', 'true');
      arrow.textContent = '↗';
      link.appendChild(arrow);
      card.appendChild(link);
    }

    el.addEventListener('click', () => toggleDonorCard(el, card));
    wrap.appendChild(el);
    wrap.appendChild(card);
    box.appendChild(wrap);
  });
}

/* Clicking anywhere else, or Escape, dismisses the card — the same way the
   language menu closes. A click inside the wrapper is the chip's own click or
   the card's profile link, and both keep it open. */
document.addEventListener('click', (e) => {
  if (!(e.target.closest && e.target.closest('.donor-wrap'))) closeDonor();
});
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') closeDonor();
});
/* A resize can put an open card back over an edge it was nudged away from. */
window.addEventListener('resize', () => {
  if (openDonor) placeDonorCard(openDonor.chip, openDonor.card);
});

async function fetchDonors() {
  try {
    /* no-store: the file edits on its own schedule, so neither the browser
       nor an intermediate cache may serve a stale list. */
    const res = await fetch('donors.json', { cache: 'no-store' });
    if (!res.ok) return;
    const data = await res.json();
    donorData = Array.isArray(data) ? data : null;
  } catch (err) {
    // Offline or opened over file:// — the section falls back to its empty state.
  }
  renderDonors();
}
