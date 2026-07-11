// scribe-groups.js — visibility policy for daemon-created scribe groups.

// Live scribes are active work, so their groups must stay visible without a
// preference toggle. The opt-in only controls dormant/offline scribe groups.
// Keep this helper pure so the Groups tab and command palette cannot drift.
function scribeGroupVisible(group, showOfflineScribes = false) {
  return !group?.scribe || (group.online || 0) > 0 || showOfflineScribes;
}

export { scribeGroupVisible };
