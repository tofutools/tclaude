// The Messages reader's "⤓ download ▾" split button: hand a whole human
// notification to someone else.
//
// The main half does the common thing in one click — the message with every
// file it carries (a .zip), or just the Markdown when it carries none. The
// caret opens the variants: the zip, the Markdown alone, and "Copy as
// Markdown" for pasting straight into a chat or a PR. The daemon renders the
// Markdown for all three (human-attachments.js messageExportHref), so they
// always agree.
import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { attachmentSize, messageAttachments, messageExportHref } from './human-attachments.js';

const html = htm.bind(h);

// triggerDownload hands an authenticated same-origin URL to the browser's own
// download path, so a large zip streams to disk instead of through JS memory.
function triggerDownload(href) {
  const link = document.createElement('a');
  link.href = href;
  link.download = '';
  link.rel = 'noopener';
  document.body.appendChild(link);
  link.click();
  link.remove();
}

export function MessageDownloadMenu({
  message, toast, download = triggerDownload, fetchImpl = globalThis.fetch?.bind(globalThis),
  clipboard = globalThis.navigator?.clipboard,
}) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef(null);
  const caretRef = useRef(null);
  const attachments = messageAttachments(message);
  const count = attachments.length;
  const totalBytes = attachments.reduce((sum, attachment) => sum + Number(attachment.size_bytes || 0), 0);

  useEffect(() => {
    if (!open) return undefined;
    rootRef.current?.querySelector('[role="menuitem"]')?.focus();
    const dismiss = (event) => {
      if (!rootRef.current?.contains(event.target)) setOpen(false);
    };
    document.addEventListener('mousedown', dismiss);
    return () => document.removeEventListener('mousedown', dismiss);
  }, [open]);
  // A different message in the reader closes a menu left open for the last one.
  useEffect(() => setOpen(false), [message.id]);

  const close = () => {
    setOpen(false);
    caretRef.current?.focus();
  };
  const run = (action) => {
    close();
    action();
  };
  const copyMarkdown = async () => {
    try {
      if (!clipboard?.writeText) throw new Error('clipboard unavailable');
      const response = await fetchImpl(messageExportHref(message.id, false));
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      await clipboard.writeText(await response.text());
      toast?.(`Message #${message.id} copied as Markdown`);
    } catch (err) {
      toast?.(`Copy failed: ${err.message}`);
    }
  };
  const onMenuKeyDown = (event) => {
    if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      close();
      return;
    }
    if (event.key !== 'ArrowDown' && event.key !== 'ArrowUp') return;
    event.preventDefault();
    event.stopPropagation();
    const items = [...rootRef.current.querySelectorAll('[role="menuitem"]')];
    const at = items.indexOf(document.activeElement);
    const next = event.key === 'ArrowDown' ? at + 1 : at - 1;
    items[(next + items.length) % items.length]?.focus();
  };

  const filesNote = count ? `${count} file${count === 1 ? '' : 's'} · ${attachmentSize(totalBytes)}` : '';
  // Tabbing out of the open menu closes it, like a click elsewhere does.
  const onFocusOut = (event) => {
    if (open && !rootRef.current?.contains(event.relatedTarget)) setOpen(false);
  };
  return html`<span class="mail-download" ref=${rootRef} onFocusOut=${onFocusOut}>
    <button type="button" class="mail-download-main"
      title=${count ? `Download this message with its ${filesNote} as a .zip` : 'Download this message as Markdown'}
      onClick=${() => download(messageExportHref(message.id, count > 0))}>⤓ download</button><button
      type="button" class="mail-download-caret" ref=${caretRef} aria-haspopup="menu" aria-expanded=${String(open)}
      aria-label="More download options" title="More download options"
      onClick=${() => setOpen(!open)}>▾</button>
    ${open && html`<div class="mail-download-menu" role="menu" aria-label="Download message" onKeyDown=${onMenuKeyDown}>
      ${count > 0 && html`<button type="button" role="menuitem" data-export="zip"
        onClick=${() => run(() => download(messageExportHref(message.id, true)))}>
        <span class="mail-download-icon" aria-hidden="true">📦</span><span>Message + attachments (.zip)<small>message.md + ${filesNote}</small></span></button>`}
      <button type="button" role="menuitem" data-export="md"
        onClick=${() => run(() => download(messageExportHref(message.id, false)))}>
        <span class="mail-download-icon" aria-hidden="true">📄</span><span>Message only (.md)<small>${count ? 'headers + body, attachments listed by name' : 'headers + body'}</small></span></button>
      <div class="mail-download-sep" role="separator"></div>
      <button type="button" role="menuitem" data-export="copy" onClick=${() => run(copyMarkdown)}>
        <span class="mail-download-icon" aria-hidden="true">📋</span><span>Copy as Markdown<small>paste straight into a chat or a PR</small></span></button>
    </div>`}
  </span>`;
}
