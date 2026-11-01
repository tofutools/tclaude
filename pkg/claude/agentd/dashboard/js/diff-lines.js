// diff-lines.js -- the body of a confirm-changes dialog. A diff echoes values
// whose own fields are masked, so each line marks what a capture must not read.

import { h } from 'preact';
import htm from 'htm';
import { splitDiffSecret } from './capture-snapshot.js';

const html = htm.bind(h);

export function DiffLines({ diff, sign }) {
  return diff.map((line, index) => {
    const [plain, secret] = splitDiffSecret(line.s);
    return html`<span key=${index} class=${`dl ${line.t}`}>${sign[line.t]} ${plain}${secret && html`<span data-capture-mask>${secret}</span>`}</span>`;
  });
}
