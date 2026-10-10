// drag.js: drag an agent row onto a group in "Groups · all nodes", for
// record.sh. A synthetic HTML5 drag shows no drag image, so a pill follows the
// pointer path on camera. Evaluated by `drive eval` as drag.js(agent, group key).
(async (agentName, groupKey) => {
  const row = [...document.querySelectorAll('tr.dnd-draggable[data-dnd-agent]')].find((r) => r.offsetParent && r.getAttribute('data-dnd-label') === agentName);
  const group = document.querySelector(`details[data-group-key="${groupKey}"]`);
  const target = group && group.querySelector(':scope > summary');
  if (!row || !target) return 'NOTFOUND';
  row.scrollIntoView({ block: 'center' });
  await new Promise((r) => setTimeout(r, 400));
  const dt = new DataTransfer();
  const fire = (el, type, x, y) => el.dispatchEvent(new DragEvent(type, { bubbles: true, cancelable: true, dataTransfer: dt, clientX: x, clientY: y }));
  const a = row.querySelector('.rowname') || row; const ra = a.getBoundingClientRect();
  const ghost = document.createElement('div');
  ghost.textContent = `⠿ ${agentName}`;
  Object.assign(ghost.style, { position: 'fixed', zIndex: 99999, pointerEvents: 'none', padding: '4px 10px', borderRadius: '6px', background: '#1f6feb', color: '#fff', font: '600 13px system-ui', boxShadow: '0 4px 14px rgba(0,0,0,.5)', left: `${ra.left}px`, top: `${ra.top}px`, transition: 'left 1.2s ease-in-out, top 1.2s ease-in-out' });
  document.body.appendChild(ghost);
  fire(row, 'dragstart', ra.left, ra.top);
  await new Promise((r) => setTimeout(r, 300));
  const rb = target.getBoundingClientRect();
  const x = rb.left + 120, y = rb.top + rb.height / 2;
  ghost.style.left = `${x}px`; ghost.style.top = `${y - 10}px`;
  for (let i = 0; i < 6; i++) { await new Promise((r) => setTimeout(r, 200)); fire(target, 'dragover', x, y); }
  await new Promise((r) => setTimeout(r, 900));
  fire(target, 'drop', x, y);
  fire(row, 'dragend', x, y);
  ghost.remove();
  return 'ok';
})
