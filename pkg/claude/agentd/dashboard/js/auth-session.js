// Turn an expired HttpOnly dashboard session into a top-level sign-in rather
// than leaving the already-open SPA alive while every operation fails. This is
// a classic script loaded before the module graph so every dashboard feature,
// including modules that capture globalThis.fetch, receives the wrapper.
(() => {
  const nativeFetch = window.fetch.bind(window);
  let reloading = false;

  window.fetch = async (...args) => {
    const response = await nativeFetch(...args);
    if (!reloading && response.headers.get('X-Tclaude-Login-Required') === '1') {
      reloading = true;
      // Let terminal views disarm beforeunload prompts, then move to the
      // canonical login page with a validated same-origin return target. The
      // hash matters for standalone terminal popouts: it carries the pane seed.
      const detail = {
        returnTo: window.location.pathname + window.location.search + window.location.hash,
      };
      window.dispatchEvent(new CustomEvent('tclaude:auth-expired', { detail }));
      window.location.replace('/?return_to=' + encodeURIComponent(detail.returnTo));
    }
    return response;
  };
})();
