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
      // Reload the current app/deep-link URL. Its unauthenticated GET is the
      // canonical server-rendered login page, and preserving the path keeps the
      // operator's intended dashboard location available after signing in.
      window.location.reload();
    }
    return response;
  };
})();
