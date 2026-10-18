// Shared entry point for palette commands and group cog actions.
let controller = null;
export function registerGitRepositoriesController(value) {
  controller = value;
  return () => { if (controller === value) controller = null; };
}
// node names another node's group (blank: the page's): its checkouts are
// scanned and updated there.
export function openGitRepositories(mode = 'pull', group = '', node = '') {
  if (!controller) throw new Error('Git repository dialog is not ready');
  return controller.open(mode, group, node);
}
