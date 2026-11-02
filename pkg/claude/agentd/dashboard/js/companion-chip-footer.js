// companion-chip-footer.js -- the chips resting inside the footer bar, so they
// cover no working area. The bar's free room changes with its content and the
// width, so it is measured, and a row that doesn't fit is not put there: it
// covers nothing and pushes nothing.
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import { signal } from '@preact/signals';
import { FOOTER_CHIP_MARGIN, footerRoom, footerSpotFits, freeStretch } from './companion-model.js';

const COMPACT = 'compact';

// What the bar shows, as text and as the controls and pictures it holds. A
// span the bar stretches for layout says nothing about where its text ends, and
// the popover opening above the bar lives in it without taking room on it: its
// list reports rows scrolled out of sight wherever they would be, the bar included.
// The overlay dock's measuring row (overlay-dock.js) is off screen and takes none either.
const POPOVER = '.open-prs-popover, .overlay-dock-measure';

// footerChipReserve is the room the chips take in the footer while they rest
// there, margins included, and the spot they rest at: { width, spot }. The
// overlay dock beside them in the bar keeps it free, so its pills give way
// before the chips have to float.
export const footerChipReserve = signal({ width: 0, spot: '' });

export const chipReserve = (width) => (width > 0 ? width + 2 * FOOTER_CHIP_MARGIN : 0);

function occupied(bar, doc) {
  const rects = [...bar.querySelectorAll('button, a, img, svg')].filter((node) => !node.closest(POPOVER)).map((node) => node.getBoundingClientRect());
  const walker = doc.createTreeWalker(bar, 4);
  for (let text = walker.nextNode(); text; text = walker.nextNode()) {
    if (!text.nodeValue.trim() || text.parentElement?.closest(POPOVER)) continue;
    const range = doc.createRange();
    range.selectNodeContents(text);
    rects.push(...range.getClientRects());
  }
  return rects.filter((rect) => rect.width > 0);
}

// measureFooterRoom is the footer's free stretch, { left, right }, that a row
// width wide rests in at spot (footerRoom), or with neither given its widest; null
// on a page without one.
export function measureFooterRoom(win, spot = '', width = 0) {
  const doc = win?.document;
  const bar = doc?.querySelector?.('footer .footer-inner');
  const box = bar?.getBoundingClientRect?.();
  if (!box?.width) return null;
  const rects = occupied(bar, doc);
  return spot ? footerRoom(rects, box.left, box.right, spot, width) : freeStretch(rects, box.left, box.right);
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
// width }, measured before the row paints, and measure, for a row about to land
// there.
export function useFooterRest({ win, watching, row, spot }) {
  const [rest, setRest] = useState({ room: null, width: 0 });
  const measure = useRef(() => {});
  measure.current = () => {
    const node = row.current;
    const width = node ? compactWidth(node) : 0;
    const room = measureFooterRoom(win, spot, width);
    setRest((now) => (now.width === width && now.room?.left === room?.left && now.room?.right === room?.right ? now : { room, width }));
  };
  // Every render: what the row holds changes with it.
  useLayoutEffect(() => { if (watching) measure.current(); });
  useEffect(() => {
    if (!watching) return undefined;
    const bar = win?.document?.querySelector?.('footer');
    const again = () => measure.current();
    const observer = bar && typeof win.MutationObserver === 'function' ? new win.MutationObserver(again) : null;
    // Attributes too: the overlay dock moves its pills by restyling itself, as
    // when it makes room for this very row (footerChipReserve), and a pill moved
    // so is a pill measured where it was, under the row.
    observer?.observe(bar, { childList: true, subtree: true, characterData: true, attributes: true, attributeFilter: ['style', 'class', 'hidden'] });
    win?.addEventListener?.('resize', again);
    return () => {
      observer?.disconnect();
      win?.removeEventListener?.('resize', again);
    };
  }, [win, watching]);
  return { ...rest, fits: watching && footerSpotFits(rest.room, rest.width), measure: () => measure.current() };
}
