// palette-resources.js -- the command palette's "make a resource in a group"
// commands: a drawing board, a live page or a process for each group, the
// palette twins of a group's ＋ menu. (A group's shell is palette.js's own
// "Open terminal in group" command, which also finds it as a new shell.)
//
// Labels start with Make / Begin, words no older command leads with, so a
// bare "new" or "start" still finds Spawn agent and Power on first.
//
// The query names the kind and the group in any order and with filler words
// ("open drawing board in tclaude", "whiteboard tclaude", "new page tclaude");
// the 🧙 words find the same commands (slate, looking glass, rite).
//
// Pure: the seams in `make` do the work, so a node test drives it directly.

// buildResourceCreateCommands is one command per group and kind. make holds
// { drawingBoard(group), page(group), process(group) }; processes is whether this
// node offers processes; wiz(regular, wizard) picks the theme's wording.
export function buildResourceCreateCommands(groups = [], {
  wiz = (regular) => regular, processes = false, make = {}, touch = () => {},
} = {}) {
  const cmds = [];
  for (const g of groups) {
    const group = g.name;
    const run = (kind) => () => { touch(group); return make[kind]?.(g); };
    cmds.push({
      icon: wiz('✎', '🗺'),
      label: wiz(`Make a drawing board in ${group}`, `Unroll a scrying slate in ${group}`),
      hint: wiz(`a drawing board shared with ${group}'s agents, opened as a tab`,
        `a slate the heroes of ${group} draw on with you`),
      keywords: `new create make add open start drawing board whiteboard white board sketch draw diagram canvas excalidraw resource group ${group}`
        + ' slate scrying map chart party',
      run: run('drawingBoard'),
    });
    cmds.push({
      icon: wiz('▭', '🪞'),
      label: wiz(`Make a live page in ${group}`, `Raise a looking glass in ${group}`),
      hint: wiz(`a blank page you draw on; a member of ${group} takes it over with its own source`,
        `a blank glass you mark; a hero of ${group} fills it with its own vision`),
      keywords: `new create make add open start live page web page blank sketch mock resource group ${group}`
        + ' looking glass mirror vision party',
      run: run('page'),
    });
    if (processes) {
      cmds.push({
        icon: wiz('▶', '📜'),
        label: wiz(`Begin a process in ${group}…`, `Begin a rite in ${group}…`),
        hint: wiz(`open ${group}'s start form: run a saved process, or build a new one`,
          `open ${group}'s rite form: begin a written rite, or shape a new one`),
        keywords: `new create make add open start run launch process workflow resource group ${group}`
          + ' rite ritual labour party',
        run: run('process'),
      });
    }
  }
  return cmds;
}
