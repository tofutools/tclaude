import { cycleSort } from './sort.js';
import {
  listPagerNav,
  setListPageSize,
} from './list-paging.js';

export function createGroupsActions({ state, refresh }) {
  if (!state) throw new TypeError('groups actions require state');
  if (typeof refresh !== 'function') throw new TypeError('groups actions require refresh');

  return Object.freeze({
    refresh,
    sort(table, column) {
      cycleSort(table, column);
      state.rerender();
    },
    page(kind, action, total) {
      if (!listPagerNav(kind, action, total)) return false;
      void refresh();
      return true;
    },
    setPageSize(kind, value) {
      setListPageSize(kind, Number(value) || 50);
      void refresh();
    },
  });
}
