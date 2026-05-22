// Slop theme — a purely cosmetic re-skin of the agent dashboard,
// tagged onto the URL with ?slop=1. Same data, same routes, same
// auth; just a different paint job. The server preserves the param
// through the auth redirect (see handleDashboardRoot in agentd/dashboard.go)
// so the bare-path URL still carries it.

export function applySlopThemeIfRequested() {
  const params = new URLSearchParams(window.location.search);
  if (params.get('slop') !== '1') return;
  document.body.classList.add('slop');
  document.title = '🎰 The slop machine';
  const favicon = document.querySelector('link[rel="icon"]');
  if (favicon) {
    favicon.setAttribute(
      'href',
      'data:image/svg+xml,<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"><text x="50" y="52" font-size="78" text-anchor="middle" dominant-baseline="central">🎰</text></svg>',
    );
  }
  const h1 = document.querySelector('header h1');
  if (h1) h1.textContent = '🎰 The slop machine';
}
