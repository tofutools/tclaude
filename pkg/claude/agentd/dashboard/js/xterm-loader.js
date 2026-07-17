// The dashboard's terminal shell is mounted during ordinary page bootstrap,
// but most visits never open a web terminal. Keep the comparatively large
// classic xterm core out of that initial request set and fetch it only when a
// terminal is actually requested. The standalone terminals page still loads
// xterm in its HTML because opening that page is itself terminal intent.
// dashboard-imperative-boundary: browser-io

const XTERM_RUNTIME_SRC = '/static/vendor/xterm/xterm.min.js';

let runtimePromise = null;

export function loadXtermRuntime({
  documentRef = globalThis.document,
  globalRef = globalThis,
  fetchImpl = globalThis.fetch,
} = {}) {
  if (typeof globalRef.Terminal === 'function') return Promise.resolve(true);
  if (runtimePromise) return runtimePromise;
  if (!documentRef?.createElement || !documentRef.head?.appendChild) {
    return Promise.reject(new Error('xterm runtime requires a browser document'));
  }
  if (typeof fetchImpl !== 'function') {
    return Promise.reject(new Error('xterm runtime requires fetch'));
  }

  runtimePromise = (async () => {
    // Script elements do not pass responses through auth-session.js's wrapped
    // fetch. A cheap HEAD first makes an expired cookie take the dashboard's
    // normal auth-expired redirect path before the browser starts the core GET.
    const response = await fetchImpl(XTERM_RUNTIME_SRC, {
      method: 'HEAD', credentials: 'same-origin',
    });
    if (!response?.ok) {
      throw new Error(`xterm runtime preflight failed (${response?.status || 'unknown status'})`);
    }
    await new Promise((resolve, reject) => {
      const script = documentRef.createElement('script');
      script.src = XTERM_RUNTIME_SRC;
      script.async = true;
      script.dataset.tclaudeXtermRuntime = '1';
      script.onload = () => {
        if (typeof globalRef.Terminal === 'function') {
          resolve();
          return;
        }
        reject(new Error('xterm runtime loaded without installing Terminal'));
      };
      script.onerror = () => reject(new Error('failed to load xterm runtime'));
      documentRef.head.appendChild(script);
    });
    return true;
  })().catch((error) => {
    runtimePromise = null;
    throw error;
  });
  return runtimePromise;
}
