// What the dashboard does when its last open terminal closes. An empty
// Terminals tab has nothing to act on, so by default the operator is moved to
// Groups; config dashboard.stay_on_terminals_after_last_close keeps them put.
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
// never pull the operator off the tab they are on.
export function leaveTerminalsAfterLastClose(documentRef = globalThis.document) {
  if (stay || !documentRef?.getElementById('tab-terminals')?.classList.contains('active')) return false;
  const groups = documentRef.querySelector('nav [data-tab="groups"]');
  if (!groups) return false;
  // The current history entry may still name the terminal that just closed,
  // and a deep link to a terminal reopens it. Correct it to the bare tab first
  // so Back from Groups lands on the empty Terminals tab, not a resurrection.
  documentRef.dispatchEvent(new CustomEvent('tclaude:navigated', {
    detail: { location: { tab: 'terminals' }, correction: true },
  }));
  groups.click();
  groups.focus();
  return true;
}
