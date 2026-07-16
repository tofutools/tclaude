// Entry point for the standalone /terminals?solo=1 pop-out. The lifecycle
// controller preserves hash/auth/unload semantics while the shared Preact
// terminal shell owns the page's stable root.

import { initDashPrefs } from './prefs.js';
import { createStandaloneTerminalsPage } from './terminal-standalone.js';
import { initTerminalThemeSync } from './terminal-theme.js';

const page = createStandaloneTerminalsPage({
  host: document.getElementById('terminals-root'),
  initPrefs: initDashPrefs,
  initThemeSync: initTerminalThemeSync,
});

void page.start();
