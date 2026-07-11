// This source module intentionally has no Preact imports. A missing or broken
// runtime module must not prevent the legacy dashboard module graph from
// linking; future feature islands can use the same load-then-claim boundary.
export async function mountPreactRuntimeProbe() {
  const host = document.createElement('span');
  host.id = 'preact-runtime-probe';
  host.hidden = true;
  host.dataset.state = 'loading';
  document.body.append(host);

  try {
    const { mountPreactProbe } = await import('./preact-probe.js');
    mountPreactProbe(host);
    // Preact and Signals schedule their render flush in microtasks. Two turns
    // prove both the initial render and the signal-driven update completed.
    await Promise.resolve();
    await Promise.resolve();
    const probe = host.querySelector('[data-preact-probe="ready"]');
    if (!probe || probe.textContent !== 'ready') {
      throw new Error('Preact/Signals runtime probe did not become ready');
    }
    host.dataset.state = 'ready';
  } catch (error) {
    host.dataset.state = 'failed';
    console.warn('Preact runtime probe unavailable; legacy dashboard remains active.', error);
  }
}
