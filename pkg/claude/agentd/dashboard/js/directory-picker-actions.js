async function responsePayload(response) {
  try { return await response.json(); } catch (_) { return {}; }
}

export function createDirectoryPickerActions({ fetchImpl = fetch } = {}) {
  const mutate = async (url, body) => {
    const response = await fetchImpl(url, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    const payload = await responsePayload(response);
    if (!response.ok) throw new Error(payload.error || `HTTP ${response.status}`);
    return payload;
  };
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
    create(parent, name) {
      return mutate('/api/create-directory', {
        parent: String(parent || '').trim(), name: String(name || '').trim(),
      });
    },
    rename(path, name) {
      return mutate('/api/rename-directory', {
        path: String(path || '').trim(), name: String(name || '').trim(),
      });
    },
    remove(path, confirm) {
      return mutate('/api/delete-directory', {
        path: String(path || '').trim(), confirm: String(confirm || '').trim(),
      });
    },
  });
}
