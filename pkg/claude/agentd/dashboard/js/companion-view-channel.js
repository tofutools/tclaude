// companion-view-channel.js -- what keeps the operator's tab reachable by this
// node's agents: the capture presence loop, the notice each capture shows, and
// the marks agents draw. It sits apart from the companion window because a
// peer's page has this channel but no window.

import { h } from 'preact';
import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { MarksLayer, marksKey } from './companion-marks.js';
import { CAPTURE_NOTICE_MS, startCapturePresence } from './dashboard-capture.js';

const html = htm.bind(h);

export function CompanionViewChannel({ activeTab, notify, fetchImpl = globalThis.fetch, win = globalThis.window }) {
  // Stay reachable for `tclaude agent guide capture`, and tell the operator
  // each time their view is captured.
  const [captureNotice, setCaptureNotice] = useState('');
  const [marks, setMarks] = useState([]);
  const marksSeen = useRef('');
  const markReport = useRef([]);
  const receiveMarks = useCallback((next) => {
    const key = marksKey(next);
    if (key === marksSeen.current) return;
    marksSeen.current = key;
    // The layer leaves with the last mark, so it cannot report that itself.
    if (!next.length) markReport.current = [];
    setMarks(next);
  }, []);
  const postMark = (path, body) => fetchImpl(`/api/companion/marks/${path}`, {
    method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body || {}),
  });
  const endMark = useCallback(async (mark, action) => {
    receiveMarks(marks.filter((each) => each.id !== mark.id));
    try { await postMark(`${mark.id}/${action}`); } catch { notify?.('Could not end that mark', 'error'); }
  }, [marks]);
  const dismissAllMarks = useCallback(async () => {
    receiveMarks([]);
    try { await postMark('dismiss'); } catch { notify?.('Could not dismiss the marks', 'error'); }
  }, []);
  const replyToMark = useCallback(async (mark, text) => {
    try {
      const response = await postMark(`${mark.id}/reply`, { text });
      if (response?.ok) return true;
      const data = await response?.json?.().catch(() => null);
      notify?.(data?.error || 'Could not send that reply', 'error');
    } catch { notify?.('Could not send that reply', 'error'); }
    return false;
  }, []);
  useEffect(() => {
    if (!win?.document) return undefined;
    return startCapturePresence({
      win, fetchImpl, tab: () => activeTab.value,
      onCapture: (request) => setCaptureNotice(request.agent_name || 'An agent'),
      onMarks: receiveMarks, marks: () => markReport.current,
    });
  }, [win, fetchImpl]);
  useEffect(() => {
    if (!captureNotice) return undefined;
    const timer = setTimeout(() => setCaptureNotice(''), CAPTURE_NOTICE_MS);
    return () => clearTimeout(timer);
  }, [captureNotice]);

  return html`
    ${marks.length > 0 && win && html`<${MarksLayer} marks=${marks} win=${win} onEnd=${endMark} onDismissAll=${dismissAllMarks}
      onReply=${replyToMark} onReport=${(report) => { markReport.current = report; }} />`}
    ${captureNotice && html`<div class="companion-capture-notice" role="status" data-capture-skip>${captureNotice} captured this view</div>`}`;
}
