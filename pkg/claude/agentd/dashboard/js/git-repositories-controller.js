// Shared entry point for palette commands and group cog actions.
let controller = null;
export function registerGitRepositoriesController(value) {
  controller = value;
  return () => { if (controller === value) controller = null; };
}
export function openGitRepositories(mode = 'pull', group = '') {
  if (!controller) throw new Error('Git repository dialog is not ready');
  return controller.open(mode, group);
}
