import { signal } from '@preact/signals';
import { h, render } from 'preact';
import htm from 'htm';

const html = htm.bind(h);

function RuntimeProbe({ state }) {
  return html`
    <span data-preact-probe=${state.value} aria-hidden="true">
      ${state}
    </span>
  `;
}

// mountPreactProbe is deliberately tiny: TCL-340 proves that Preact, HTM,
// and Signals can render from offline embedded modules without handing a real
// dashboard feature to the new runtime yet.
export function mountPreactProbe(host) {
  const state = signal('booting');
  render(html`<${RuntimeProbe} state=${state} />`, host);
  state.value = 'ready';
  return () => render(null, host);
}
