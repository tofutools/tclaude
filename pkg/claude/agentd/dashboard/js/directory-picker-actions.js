async function responsePayload(response) {
  try { return await response.json(); } catch (_) { return {}; }
}

export function createDirectoryPickerActions({ fetchImpl = fetch } = {}) {
  return Object.freeze({
    async browse(path) {
      const response = await fetchImpl('/api/browse-directories', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ path: String(path || '').trim() }),
      });
      const payload = await responsePayload(response);
      if (!response.ok) throw new Error(payload.error || `HTTP ${response.status}`);
      return payload;
    },
  });
}
