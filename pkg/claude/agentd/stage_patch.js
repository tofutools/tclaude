// tclaude live-patch applier (tcl-14z3fq).
//
// One function, tclaudeApplyPatch(doc, patch), that applies one patch of a
// page's journal (pages_patches.go) to a document and returns how many
// elements it touched. The probe (stage_probe.js) runs it for the patches its
// parent relays; a `page snapshot` runs it for the journal before the
// screenshot. The stage inlines this file in front of the probe, inside the
// probe's own scope, so it adds nothing to the page's globals.
//
//   {kind:'css', n, css}                       a <style> after everything else
//   {kind:'dom', n, selector, op:'set-text', value}
//   {kind:'dom', n, selector, op:'set-attr', name, value}
//   {kind:'dom', n, selector, op:'remove'}
//   {kind:'dom', n, selector, op:'insert-html', value}   at the end of each match
//
// It throws on a patch it cannot apply (an invalid selector, an unknown op).
//
// This file must never contain a closing script tag: it is inlined verbatim.
var tclaudeApplyPatch = function (doc, p) {
  'use strict';
  if (!p || typeof p !== 'object') throw new Error('malformed patch');
  if (p.kind === 'css') {
    var style = doc.createElement('style');
    style.setAttribute('data-tclaude-patch', String(p.n));
    style.textContent = String(p.css || '');
    // Last in the document, so it wins over the page's own sheets, even
    // those a dev server adds to the head later.
    doc.documentElement.appendChild(style);
    return 1;
  }
  if (p.kind !== 'dom') throw new Error('unknown patch kind');
  var list = doc.querySelectorAll(String(p.selector || ''));
  var value = String(p.value == null ? '' : p.value);
  for (var i = 0; i < list.length; i++) {
    var el = list[i];
    switch (p.op) {
      case 'set-text': el.textContent = value; break;
      case 'set-attr': el.setAttribute(String(p.name), value); break;
      case 'remove': el.remove(); break;
      case 'insert-html': el.insertAdjacentHTML('beforeend', value); break;
      default: throw new Error('unknown DOM patch');
    }
  }
  return list.length;
};
