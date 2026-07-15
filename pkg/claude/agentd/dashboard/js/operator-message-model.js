// Pure state boundary for the terminal operator-message composer. A submit may
// await a slow attachment upload, so capture every request field together
// before yielding; later UI edits can never create a hybrid message.
export function snapshotOperatorMessageDraft({ target, subject = '', body = '', files = [] } = {}) {
  return Object.freeze({
    to: target?.agent || '',
    subject: String(subject),
    body: String(body),
    files: Object.freeze(Array.from(files)),
  });
}
