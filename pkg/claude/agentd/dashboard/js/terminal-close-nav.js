// What the dashboard does when its last open terminal or board tab closes. An
// empty Terminals tab has nothing to act on, so by default the operator is moved
// to Groups; config dashboard.stay_on_terminals_after_last_close keeps them put.
// The dashboard snapshot refreshes the setting each poll (refresh.js).
//
// This is an in-page tab switch through the nav anchor — the same route a
// click takes, so history and per-tab side effects stay owned by
// nav-history.js — not a browser navigation, so nothing can block it.

let stay = false;

export function setStayOnTerminalsAfterLastClose(value) {
  stay = !!value;
}

// leaveTerminalsAfterLastClose only acts while the Terminals tab is showing:
// a terminal closed from elsewhere (an agent hidden from the Groups tab) must
// never pull the operator off the tab they are on, and the URL there already
// names that tab. `stayHere` keeps the operator put whatever config says, for a
// board that moved to the overlay and is still being worked on.
export function leaveTerminalsAfterLastClose({ stayHere = false, documentRef = globalThis.document } = {}) {
  if (!documentRef?.getElementById('tab-terminals')?.classList.contains('active')) return false;
  // The current history entry may still name the tab that just closed, and a
  // deep link reopens it, on a reload as much as on Back. Correct it to the bare
  // tab whether or not the operator stays, so neither brings it back.
  documentRef.dispatchEvent(new CustomEvent('tclaude:navigated', {
    detail: { location: { tab: 'terminals' }, correction: true },
  }));
  if (stay || stayHere) return false;
  const groups = documentRef.querySelector('nav [data-tab="groups"]');
  if (!groups) return false;
  groups.click();
  groups.focus();
  return true;
}
