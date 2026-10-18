// Agent rename titles: the client mirror of the daemon's isValidRenameTitle
// gate (handlers.go) plus the auto-correct the spawn dialog applies to names
// (normalizeSpawnName). The rename charset is wider than the spawn one —
// titles never become branch names — so normalization keeps spaces and
// brackets and only rewrites what the daemon would reject. Both the Groups
// tab's inline rename and the Edit agent dialog share it, gated on the same
// agent.spawn_name_normalize toggle as spawn.

export const MAX_AGENT_TITLE_LEN = 64;
const TITLE_CHAR = /[A-Za-z0-9_\-[\]{}() ]/;
const TITLE_CHARSET = 'letters, digits, space or _ - [ ] { } ( )';

export function validAgentTitle(value) {
  const title = String(value || '');
  return !!title && title.length <= MAX_AGENT_TITLE_LEN && !title.includes('  ')
    && /^[A-Za-z0-9_\-[\]{}() ]+$/.test(title);
}

export function normalizeAgentTitle(value) {
  let out = '';
  let previousSeparator = false;
  for (const character of String(value ?? '').replace(/\s+/g, ' ')) {
    if (TITLE_CHAR.test(character)) {
      out += character;
      previousSeparator = false;
    } else if (!previousSeparator) {
      out += '-';
      previousSeparator = true;
    }
  }
  out = out.replace(/ {2,}/g, ' ').replace(/^[-\s]+/, '').replace(/[-\s]+$/, '');
  if (out.length > MAX_AGENT_TITLE_LEN) {
    out = out.slice(0, MAX_AGENT_TITLE_LEN).replace(/[-\s]+$/, '');
  }
  return out;
}

// resolveAgentTitle returns the title a rename will send, or throws the
// reason it cannot be sent. An already-valid title is never rewritten.
export function resolveAgentTitle(value, normalizeEnabled = true) {
  const raw = String(value ?? '').trim();
  if (validAgentTitle(raw)) return raw;
  if (!normalizeEnabled) {
    throw new Error(`title must be 1-${MAX_AGENT_TITLE_LEN} chars of ${TITLE_CHARSET} — no double spaces`);
  }
  const title = normalizeAgentTitle(raw);
  if (!title) throw new Error('title has no usable characters');
  return title;
}

export function agentTitleHint(value, normalizeEnabled = true) {
  const raw = String(value ?? '').trim();
  if (!raw || validAgentTitle(raw)) return { text: '', warn: false };
  if (!normalizeEnabled) {
    return {
      text: `invalid — use ${TITLE_CHARSET} (max ${MAX_AGENT_TITLE_LEN}, no double spaces)`,
      warn: true,
    };
  }
  const normalized = normalizeAgentTitle(raw);
  return normalized
    ? { text: `will be renamed to “${normalized}”`, warn: false }
    : { text: 'no usable characters — the title is left unchanged', warn: true };
}
