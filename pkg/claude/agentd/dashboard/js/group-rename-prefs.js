import { dashPrefs } from './prefs.js';

const GROUP_ORDER_KEY = 'tclaude.dash.groupOrder';
const LAST_GROUP_KEY = 'tclaude.dash.spawn.lastGroup';
const MAILBOX_KEY = 'tclaude.dash.mail.mailbox';

const GROUP_KEY_PREFIXES = Object.freeze([
  'tclaude.dash.group.',
  'tclaude.dash.group.offline.',
  'tclaude.dash.quickpin.',
  'tclaude.dash.forcefold.',
  'tclaude.dash.mail.groupexp.',
]);

function movePref(prefs, oldKey, newKey) {
  const value = prefs.getItem(oldKey);
  if (value === null) return;
  prefs.removeItem(oldKey);
  prefs.setItem(newKey, value);
}

// Group names are embedded in several durable dashboard preferences. Keep all
// of them attached to the same logical group whichever rename surface was
// used, and remove the old keys so a later group cannot inherit stale state.
export function migrateGroupRenamePrefs(oldName, newName, prefs = dashPrefs) {
  if (!oldName || !newName || oldName === newName) return;
  for (const prefix of GROUP_KEY_PREFIXES) {
    movePref(prefs, prefix + oldName, prefix + newName);
  }

  const rawOrder = prefs.getItem(GROUP_ORDER_KEY);
  if (rawOrder) {
    try {
      const order = JSON.parse(rawOrder);
      if (Array.isArray(order) && order.includes(oldName)) {
        const renamed = [];
        for (const name of order.map((name) => name === oldName ? newName : name)) {
          if (!renamed.includes(name)) renamed.push(name);
        }
        prefs.setItem(GROUP_ORDER_KEY, JSON.stringify(renamed));
      }
    } catch (_) { /* leave malformed preferences for their owning reader */ }
  }

  if (prefs.getItem(LAST_GROUP_KEY) === oldName) prefs.setItem(LAST_GROUP_KEY, newName);
  if (prefs.getItem(MAILBOX_KEY) === `group:${oldName}`) {
    prefs.setItem(MAILBOX_KEY, `group:${newName}`);
  }
}
