// command-registry.js -- extension seam for the dashboard command palette.
//
// Feature islands register command builders, while palette.js remains the one
// global shortcut/search surface. Builders run only when the palette opens (or
// is rebuilt for a theme change), so contextual commands can inspect live
// feature state without coupling the shell island to that state.

export const COMMAND_PALETTE_OPEN_EVENT = 'tclaude:command-palette-open';

const providers = new Map();

export function registerCommandProvider(key, builder) {
  if (!key) throw new TypeError('command provider needs a key');
  if (typeof builder !== 'function') throw new TypeError('command provider needs a builder');
  providers.set(key, builder);
  return () => {
    if (providers.get(key) === builder) providers.delete(key);
  };
}

export function buildRegisteredCommands(context = {}) {
  const commands = [];
  for (const builder of providers.values()) {
    const built = builder(context);
    if (Array.isArray(built)) commands.push(...built);
  }
  return commands;
}

export function requestCommandPalette(documentRef = document) {
  documentRef.dispatchEvent(new CustomEvent(COMMAND_PALETTE_OPEN_EVENT));
}
