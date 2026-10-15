const API = '/api/saved-prompts';

async function request(path = '', options = {}) {
  const response = await fetch(API + path, { credentials: 'same-origin', ...options });
  if (!response.ok) throw new Error((await response.text()) || `HTTP ${response.status}`);
  return response.status === 204 ? null : response.json();
}

export const loadSavedPrompts = () => request();
export const deleteSavedPrompt = (name) => request('/' + encodeURIComponent(name), { method: 'DELETE' });
export const saveSavedPrompt = (previousName, prompt) => request(
  previousName ? '/' + encodeURIComponent(previousName) : '', {
    method: previousName ? 'PATCH' : 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(prompt),
  },
);
