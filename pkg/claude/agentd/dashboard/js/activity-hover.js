import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';

const html = htm.bind(h);

function plural(count, singular, pluralForm = `${singular}s`) {
  return `${count} ${count === 1 ? singular : pluralForm}`;
}

function ActivityState({ state, wizard }) {
  const label = wizard ? (state.wizardLabel || state.label) : state.label;
  return html`
    <div class=${`activity-hover-state activity-hover-state-${state.key}`}>
      <div class="activity-hover-state-heading">
        <span class="activity-hover-state-label">${label}</span>
        <span class="activity-hover-state-count">${state.members.length}</span>
      </div>
      <ul class="activity-hover-workers">
        ${state.members.map((member) => html`
          <li key=${member.key}>
            <span class="activity-hover-worker-name">${member.name}</span>
            ${member.annotation ? html`<span class="activity-hover-worker-annotation"> — ${member.annotation}</span>` : null}
            ${member.detail ? html`<span class="activity-hover-worker-detail"> — ${member.detail}</span>` : null}
          </li>
        `)}
      </ul>
    </div>
  `;
}

function ActivityHoverPanel({ details, wizard, panelID, headingID }) {
  if (!details?.groups?.length) return null;
  return html`
    <div class="activity-hover-panel" id=${panelID} role="dialog" aria-labelledby=${headingID}>
      <div class="activity-hover-panel-heading" id=${headingID}>Worker activity</div>
      <div class="activity-hover-panel-summary">
        ${plural(details.total || 0, 'worker')} · grouped by current state
      </div>
      <div class="activity-hover-groups">
        ${details.groups.map((group, groupIndex) => html`
          <section class="activity-hover-group" key=${`${group.key}:${groupIndex}`}>
            <h3>${group.name}</h3>
            ${group.states.map((state) => html`
              <${ActivityState} key=${state.key} state=${state} wizard=${wizard} />
            `)}
          </section>
        `)}
      </div>
      ${details.suppressedOffline ? html`
        <p class="activity-hover-note">
          ${plural(details.suppressedOffline, 'offline worker')} are listed here even though the offline pulse is hidden while live work is present.
        </p>
      ` : null}
    </div>
  `;
}

// ActivityHover owns only the global/header affordance. The Groups tab keeps
// rendering ActivityModes directly, so its existing modeTitles/title contract
// remains unchanged. Hover, keyboard focus, and an explicit click all expose
// the same panel; title + aria-label remain on the trigger for text-only and
// assistive-technology users.
export function ActivityHover({
  id,
  className = '',
  label,
  title,
  details,
  wizard = false,
  children,
}) {
  const rootRef = useRef(null);
  const [hovered, setHovered] = useState(false);
  const [focused, setFocused] = useState(false);
  const [pinned, setPinned] = useState(false);
  const panelID = `${id || 'activity'}-popover`;
  const headingID = `${panelID}-heading`;
  const open = hovered || focused || pinned;

  const close = () => {
    setPinned(false);
    setHovered(false);
    setFocused(false);
  };

  useEffect(() => {
    if (!pinned) return undefined;
    const onPointerDown = (event) => {
      if (!rootRef.current?.contains(event.target)) close();
    };
    document.addEventListener('pointerdown', onPointerDown);
    return () => document.removeEventListener('pointerdown', onPointerDown);
  }, [pinned]);

  const onBlur = (event) => {
    if (!rootRef.current?.contains(event.relatedTarget)) setFocused(false);
  };
  const onKeyDown = (event) => {
    if (event.key !== 'Escape') return;
    event.preventDefault();
    event.stopPropagation();
    close();
  };
  const onTriggerClick = () => {
    if (pinned) close();
    else setPinned(true);
  };

  return html`
    <span
      ref=${rootRef}
      class=${`activity-hover${className ? ` ${className}` : ''}${open ? ' is-open' : ''}`}
      onMouseEnter=${() => setHovered(true)}
      onMouseLeave=${() => setHovered(false)}
      onFocusIn=${() => setFocused(true)}
      onFocusOut=${onBlur}
    >
      <button
        id=${id}
        type="button"
        class="activity-hover-trigger"
        aria-haspopup="dialog"
        aria-expanded=${open ? 'true' : 'false'}
        aria-controls=${panelID}
        aria-label=${label}
        title=${title || null}
        onClick=${onTriggerClick}
        onKeyDown=${onKeyDown}
      >
        <span class="activity-hover-trigger-visual" aria-hidden="true">${children}</span>
      </button>
      <${ActivityHoverPanel}
        details=${details}
        wizard=${wizard}
        panelID=${panelID}
        headingID=${headingID}
      />
    </span>
  `;
}

export { ActivityHoverPanel, ActivityState };
