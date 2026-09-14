// Profile shortcuts share the ordinary spawn dialog and its editable group.
export function buildProfileSpawnCommands(snapshot, {
  defaultGroup = '', wiz = (regular) => regular, openSpawn,
}) {
  const commands = [];
  for (const profile of snapshot.profiles || []) {
    const harness = (snapshot.harnesses || []).find((entry) => entry.name === profile.harness);
    const supportsFast = !!harness?.can_fast_mode;
    const savedFast = supportsFast && profile.fast_mode === true;
    const add = (fastOverride) => {
      const fast = savedFast || fastOverride;
      const suffix = fast ? ' (fast)' : '';
      commands.push({
        icon: wiz('＋', '🔮'),
        label: wiz(`Spawn ${profile.name}${suffix}…`, `Summon ${profile.name}${suffix}…`),
        hint: wiz('open the spawn dialog with this profile', 'open the summoning circle with this pattern')
          + (defaultGroup ? ` · ${defaultGroup}` : '')
          + (fast ? ' · fast mode (higher credit cost)' : ''),
        keywords: 'spawn summon agent familiar profile pattern launch '
          + (profile.aliases || []).join(' ') + ' ' + (profile.model || '') + ' ' + (profile.harness || ''),
        run: () => openSpawn({ profileName: profile.name, defaultGroup, ...(fastOverride ? { fastMode: true } : {}) }),
      });
    };
    add(false);
    if (supportsFast && !savedFast) add(true);
  }
  return commands;
}
