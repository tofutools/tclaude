// Entry point for the saved-prompt manager from outside the Preact-owned
// management root (the dock). The manager renders in the management island.
import { managementController } from './management-controller.js';

export function openSavedPromptsManager(options = {}) { return managementController().openSavedPrompts(options); }
