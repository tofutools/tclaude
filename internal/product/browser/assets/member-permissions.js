// Shared authoring control for permissions copied at agent creation.
export function memberPermissions(initial = [], {allowDeny=true, title="Member permissions", description} = {}) {
  const host = document.createElement('fieldset');
  host.setAttribute('aria-label', title);
  const legend = document.createElement('legend'); legend.textContent = title;
  const help = document.createElement('p');
  help.textContent = description || 'Copied to each new member when deployed. Later edits or revocations use the Permissions workspace. Empty constraints grant the action on all resources; deny overrides grants and roles.';
  host.append(legend, help);
  const rows = [];
  const field = (parent, title, control) => { const label = document.createElement('label'); label.textContent = title; control.setAttribute('aria-label', title); label.append(control); parent.append(label); return control; };
  const select = (parent, title, values, value) => { const input = document.createElement('select'); for (const v of values) { const option = document.createElement('option'); option.value = v; option.textContent = v; input.append(option); } if(value&&!values.includes(value)){const option=document.createElement('option');option.value=value;option.textContent='Retained: '+value;input.append(option);} input.value = value; return field(parent, title, input); };
  const add = (permission = {}) => {
    const row = document.createElement('fieldset'); row.dataset.memberPermission = '';
    const action = select(row, 'Permission action', AUTHORITY_ACTIONS, permission.Action || 'group.members.spawn');
    const effect = select(row, 'Permission effect', allowDeny ? ['grant', 'deny'] : ['grant'], allowDeny && permission.Denied ? 'deny' : 'grant');
    const details = document.createElement('details'), summary = document.createElement('summary');
    summary.textContent = 'Named constraints'; details.append(summary); row.append(details);
    const scopes = {};
    for (const [key, label] of Object.entries({group:'Group names',spawn_profile:'Spawn profile names',sandbox_profile:'Sandbox profile names',process_template:'Process template names',remote:'Remotes',linear_team:'Linear teams',awb_workspace:'AWB workspaces',target_agent:'Target agents'})) {
      const input = document.createElement('textarea'); input.value = (permission.Scope?.[key] || []).join('\n');
      scopes[key] = field(details, label + ' (one per line)', input);
    }
    const sync = () => { details.hidden = effect.value === 'deny'; }; effect.onchange = sync; sync();
    const remove = document.createElement('button'); remove.type = 'button'; remove.textContent = 'Remove permission';
    const record = {row, action, effect, scopes}; rows.push(record);
    remove.onclick = () => { rows.splice(rows.indexOf(record), 1); row.remove(); host.dispatchEvent(new Event('input', {bubbles:true})); };
    row.append(remove); host.insertBefore(row, addButton);
  };
  const addButton = document.createElement('button'); addButton.type = 'button'; addButton.textContent = 'Add permission';
  addButton.onclick = () => { add(); host.dispatchEvent(new Event('input', {bubbles:true})); }; host.append(addButton);
  initial.forEach(add);
  return {host, read() {
    const seen = new Set();
    return rows.map(({action,effect,scopes}) => {
      if (seen.has(action.value)) throw new Error('Choose each permission action only once.'); seen.add(action.value);
      const value = {Action:action.value};
      if (effect.value === 'deny') value.Denied = true;
      else {
        const scope = {};
        for (const [key, input] of Object.entries(scopes)) { const values = input.value.split('\n').map(v=>v.trim()).filter(Boolean); if(values.length)scope[key]=values; }
        if(Object.keys(scope).length)value.Scope=scope;
      }
      return value;
    });
  }};
}
