// Entry point for the "Usage limits & budgets" manager from outside the
// Preact-owned management root (the Usage tab, Config, the limit banner).
// The manager renders in the management island.
import { managementController } from './management-controller.js';

export function openUsageRulesManager(options = {}) { return managementController().openUsageRules(options); }
