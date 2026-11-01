// companion-chip-footer.js -- the chips resting inside the footer bar, so they
// cover no working area. The bar's free room changes with its content and the
// width, so it is measured, and a row that doesn't fit is not put there: it
// covers nothing and pushes nothing.
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import { footerSpotFits, freeStretch } from './companion-model.js';

const COMPACT = 'compact';

// What the bar shows, as text and as the controls and pictures it holds. A
// span the bar stretches for layout says nothing about where its text ends.
function occupied(bar, doc) {
  const rects = [...bar.querySelectorAll('button, a, img, svg')].map((node) => node.getBoundingClientRect());
  const walker = doc.createTreeWalker(bar, 4);
  for (let text = walker.nextNode(); text; text = walker.nextNode()) {
    if (!text.nodeValue.trim()) continue;
    const range = doc.createRange();
    range.selectNodeContents(text);
    rects.push(...range.getClientRects());
  }
  return rects.filter((rect) => rect.width > 0);
}

// measureFooterRoom is the footer's widest free stretch, { left, right }, or null
// on a page without one.
export function measureFooterRoom(win) {
  const doc = win?.document;
  const bar = doc?.querySelector?.('footer .footer-inner');
  const box = bar?.getBoundingClientRect?.();
  if (!box?.width) return null;
  return freeStretch(occupied(bar, doc), box.left, box.right);
}

// compactWidth is how wide the row is in the bar, wherever it is now. It takes
// the bar's look for the length of this measure, before anything paints.
export function compactWidth(node) {
  const was = node.classList.contains(COMPACT);
  node.classList.add(COMPACT);
  const { width } = node.getBoundingClientRect();
  if (!was) node.classList.remove(COMPACT);
  return Math.ceil(width);
}

// useFooterRest says whether a row saved in the footer fits there now, and
// watches the bar and the row for as long as one is. It returns { room, fits,
// width }, measured before the row paints.
export function useFooterRest({ win, watching, row }) {
  const [rest, setRest] = useState({ room: null, width: 0 });
  const measure = useRef(() => {});
  measure.current = () => {
    const node = row.current;
    const room = measureFooterRoom(win);
    const width = node ? compactWidth(node) : 0;
    setRest((now) => (now.width === width && now.room?.left === room?.left && now.room?.right === room?.right ? now : { room, width }));
  };
  // Every render: what the row holds changes with it.
  useLayoutEffect(() => { if (watching) measure.current(); });
  useEffect(() => {
    if (!watching) return undefined;
    const bar = win?.document?.querySelector?.('footer');
    const again = () => measure.current();
    const observer = bar && typeof win.MutationObserver === 'function' ? new win.MutationObserver(again) : null;
    observer?.observe(bar, { childList: true, subtree: true, characterData: true });
    win?.addEventListener?.('resize', again);
    return () => {
      observer?.disconnect();
      win?.removeEventListener?.('resize', again);
    };
  }, [win, watching]);
  return { ...rest, fits: watching && footerSpotFits(rest.room, rest.width) };
}
