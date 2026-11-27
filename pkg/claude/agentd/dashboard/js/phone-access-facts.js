// phone-access-facts.js -- the facts the phone's access-request reader lists
// beyond its card, as plain strings: when it was asked, when it is declined
// unanswered and until when the agent waits, and what each permission slug
// means. No DOM: phone-mail-island.js renders them.

// clockTime is a moment as the phone's local clock time ("18:34:49"), or ''
// for one it cannot read.
export function clockTime(at) {
  const when = new Date(at || '');
  if (Number.isNaN(when.getTime())) return '';
  return when.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
}

// accessTiming is a pending request's two clocks as clock times: "Auto-denies
// 18:35:49 · agent waits until 18:44:49", or only the first when the agent
// named no limit, or when the two coincide (the agent stops waiting then).
export function accessTiming(request) {
  const deadline = clockTime(request?.deadline);
  if (!deadline) return '';
  const ceiling = Date.parse(request?.wait_ceiling || '');
  const due = Date.parse(request?.deadline || '');
  if (!Number.isFinite(ceiling) || ceiling - due <= 1000) return `Auto-denies ${deadline}`;
  return `Auto-denies ${deadline} · agent waits until ${clockTime(request.wait_ceiling)}`;
}

// slugDescriptions maps each registered slug to the first sentence of its
// description, from the snapshot's slug registry (the rest is for the Access
// page, not a decision on a phone); empty when the snapshot carries none.
export function slugDescriptions(slugs) {
  const out = new Map();
  for (const row of Array.isArray(slugs) ? slugs : []) {
    if (row?.slug && row.description) out.set(row.slug, firstSentence(String(row.description)));
  }
  return out;
}

function firstSentence(text) {
  const end = text.search(/\.(\s|$)/);
  return end < 0 ? text : text.slice(0, end + 1);
}
