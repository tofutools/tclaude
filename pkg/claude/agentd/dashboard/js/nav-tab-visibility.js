// nav-tab-visibility.js -- whether a dashboard tab's nav anchor is shown, the
// test the router (nav-history.js) and tab cycling (refresh.js) use to tell an
// available tab from one the dashboard hides (no plugins, no usage yet, a peer
// page that does not offer it). Those gates hide the anchor itself.
//
// On the desktop an anchor is shown when it renders (offsetParent). On a phone
// the whole desktop tab bar is hidden, because the phone shell replaces it, so
// nothing would render: there the anchor counts as shown unless a gate hides
// it.

import { isPhone } from './layout.js';

const computedStyle = (element) => element.ownerDocument.defaultView.getComputedStyle(element);

export function navTabShown(button, phone = isPhone, styleOf = computedStyle) {
  if (!button) return false;
  if (!phone) return button.offsetParent !== null;
  return styleOf(button).display !== 'none';
}
