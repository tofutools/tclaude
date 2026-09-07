const el = (tag, text) => { const node = document.createElement(tag); if (text !== undefined) node.textContent = text; return node; };

// Selection only changes the invoking form. Browsing never saves or launches work.
export function pickDirectory({api, initial = ''}) {
  return new Promise(resolve => {
    const dialog = el('dialog'); dialog.id = 'directory-picker';
    const heading = el('h2', 'Choose working directory'); heading.id = 'directory-picker-heading'; dialog.setAttribute('aria-labelledby', heading.id);
    const explanation = el('p', 'Browse directories on the backend host. Enter an absolute path to begin. Choosing a directory only fills the form.');
    const form = el('form'), label = el('label', 'Host directory path'), path = el('input'); path.value = initial; path.required = true; path.setAttribute('aria-label', 'Host directory path'); label.append(path);
    const browse = el('button', 'Browse'); browse.type = 'submit'; form.append(label, browse);
    const hiddenLabel = el('label', 'Show hidden directories'), hidden = el('input'); hidden.type = 'checkbox'; hidden.setAttribute('aria-label', 'Show hidden directories'); hiddenLabel.prepend(hidden);
    const status = el('p'); status.role = 'status'; const error = el('p'); error.role = 'alert';
    const list = el('div'); list.className = 'directory-list';
    const action = (text, run) => { const b = el('button', text); b.type = 'button'; b.onclick = run; return b; };
    let current = null, generation = 0, done = false;
    const finish = value => { if (done) return; done = true; generation++; window.removeEventListener('pagehide', cancel); document.removeEventListener('workspace-signout', cancel); dialog.close(); dialog.remove(); resolve(value); };
    const cancel = () => finish(null);
    const parent = action('Parent directory', () => load(current.Parent));
    const more = action('More directories', () => load(current.Path, current.NextAfter));
    const choose = action('Use this directory', () => { if (current) finish(current.Path); });
    const disable = () => { choose.disabled = true; parent.disabled = true; more.disabled = true; };
    async function load(target, after = '') {
      const sequence = ++generation; disable(); current = null; error.textContent = ''; status.textContent = 'Loading directories…'; list.replaceChildren();
      try {
        const query = new URLSearchParams({path: target, hidden: String(hidden.checked), limit: '100', after});
        const result = await api('/v2/directories?' + query);
        if (done || sequence !== generation) return;
        current = result; path.value = result.Path; choose.disabled = false; parent.disabled = result.Parent === result.Path; more.disabled = !result.NextAfter;
        status.textContent = result.Path + (result.NextAfter ? ' · More directories available' : '');
        for (const entry of result.Directories || []) list.append(action(entry.Name, () => load(entry.Path)));
        if (!list.childElementCount) list.append(el('p', 'No directories on this page.'));
      } catch (e) { if (!done && sequence === generation) { status.textContent = ''; error.textContent = e.code === 'unavailable' ? 'This directory cannot be browsed. Check the path and host permissions, or use a smaller directory.' : e.message || String(e); } }
    }
    form.onsubmit = e => { e.preventDefault(); load(path.value); };
    path.oninput = () => { generation++; current = null; disable(); status.textContent = ''; list.replaceChildren(); };
    hidden.onchange = () => { if (path.value) load(path.value); };
    dialog.oncancel = e => { e.preventDefault(); cancel(); }; dialog.onclose = cancel;
    dialog.append(heading, explanation, form, hiddenLabel, status, error, parent, list, more, choose, action('Cancel directory selection', cancel));
    document.body.append(dialog); disable(); dialog.showModal();
    window.addEventListener('pagehide', cancel); document.addEventListener('workspace-signout', cancel);
    if (initial) load(initial); else path.focus();
  });
}
