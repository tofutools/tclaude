import { h } from 'preact';
import htm from 'htm';

const html = htm.bind(h);

export function TemplatePlaceholderChips({ tokens, onInsert }) {
  return html`<span class="trigger-placeholder-chips">${tokens.map((token) => html`
    <button type="button" key=${token} onClick=${() => onInsert(token)}>${token}</button>`)}</span>`;
}

export function SpawnActionFields({
  value,
  onChange,
  profileOptions = [],
  placeholderTokens = [],
  fieldPrefix = 'spawn-action',
  instructionPlaceholder = '',
}) {
  const update = (patch) => onChange({ ...value, ...patch });
  const profileListID = `${fieldPrefix}-profiles`;
  return html`<div class="trigger-action-fields spawn-action-fields">
    <label>Profile<input id=${`${fieldPrefix}-profile`} value=${value.profile || ''} required
      list=${profileOptions.length ? profileListID : undefined} placeholder="spawn profile name"
      onInput=${(event) => update({ profile: event.currentTarget.value })} /></label>
    ${profileOptions.length ? html`<datalist id=${profileListID}>${profileOptions.map((profile) => html`
      <option key=${profile} value=${profile}></option>`)}</datalist>` : null}
    <label>Roles<input id=${`${fieldPrefix}-roles`} value=${(value.roles || []).join(', ')}
      placeholder="reviewer, read-only"
      onInput=${(event) => update({ roles: event.currentTarget.value.split(',').map((item) => item.trim()).filter(Boolean) })} /></label>
    <label>Name template<input id=${`${fieldPrefix}-name-template`} value=${value.nameTemplate || ''}
      placeholder="optional worker name"
      onInput=${(event) => update({ nameTemplate: event.currentTarget.value })} /></label>
    <label class="trigger-template-field">Instruction template<textarea id=${`${fieldPrefix}-instruction-template`}
      rows="5" required value=${value.instructionTemplate || ''} placeholder=${instructionPlaceholder}
      onInput=${(event) => update({ instructionTemplate: event.currentTarget.value })}></textarea>
      <${TemplatePlaceholderChips} tokens=${placeholderTokens}
        onInsert=${(token) => update({ instructionTemplate: `${value.instructionTemplate || ''}${token}` })} />
    </label>
    <label>Worker deadline (seconds)<input id=${`${fieldPrefix}-deadline`} type="number" min="0"
      value=${Number(value.workerDeadlineSeconds) || 0}
      onInput=${(event) => update({ workerDeadlineSeconds: Number(event.currentTarget.value) })} /></label>
  </div>`;
}
