// phone-access-facts.js -- the facts the phone's access-request reader lists
// beyond its card, as plain strings: when it was asked, when it is declined
// unanswered and until when the agent waits, and what each permission slug
// means. No DOM: phone-mail-island.js renders them.

// clockTime is a moment as the phone's local clock time in the phone's own
// locale ("18:34:49", or "6:34:49 PM" where that is the custom), or '' for
// one it cannot read.
export function clockTime(at) {
  const when = new Date(at || '');
  if (Number.isNaN(when.getTime())) return '';
  return when.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
}

// accessTiming is a pending request's two clocks as clock times, the time
// left to the first in brackets when now (ms) is given: "Auto-denies 18:35:49
// (0:40) · agent waits until 18:44:49", or only the first when the agent named
// no limit; when the two coincide, that the agent stops waiting then.
export function accessTiming(request, now = null) {
  const deadline = clockTime(request?.deadline);
  if (!deadline) return '';
  const due = Date.parse(request.deadline);
  const left = now === null ? '' : ` (${remaining(Math.max(0, Math.round((due - now) / 1000)))})`;
  const ceiling = Date.parse(request?.wait_ceiling || '');
  if (!Number.isFinite(ceiling)) return `Auto-denies ${deadline}${left}`;
  if (ceiling - due <= 1000) return `Auto-denies ${deadline}${left} · the agent stops waiting then`;
  return `Auto-denies ${deadline}${left} · agent waits until ${clockTime(request.wait_ceiling)}`;
}

function remaining(secs) {
  return `${Math.floor(secs / 60)}:${String(secs % 60).padStart(2, '0')}`;
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
