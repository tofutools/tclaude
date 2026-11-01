// diff-lines.js -- the body of a confirm-changes dialog. A diff echoes values
// whose own fields are masked, so each line marks what a capture must not read.

import { h } from 'preact';
import htm from 'htm';
import { diffSecrets } from './capture-snapshot.js';

const html = htm.bind(h);

// unreadable is for text that may not be JSON at all, where no line can be
// told apart from a secret.
export function DiffLines({ diff, sign, unreadable = false }) {
  const parts = diffSecrets(diff);
  return diff.map((line, index) => {
    const [plain, secret] = unreadable ? ['', line.s] : parts[index];
    return html`<span key=${index} class=${`dl ${line.t}`}>${sign[line.t]} ${plain}${secret && html`<span data-capture-mask>${secret}</span>`}</span>`;
  });
}
