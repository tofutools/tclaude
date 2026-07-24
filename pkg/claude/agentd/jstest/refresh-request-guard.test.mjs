// refresh.js owns the dashboard's authoritative snapshot poll, and its request
// guards decide what a SUPERSEDED generation is still allowed to touch: shared
// paging offsets, the published snapshot, and two independent request
// generations (the shared store's and the Jobs feature's own token). Those
// guards exist because a slow older refresh resuming last used to clobber the
// newer page and reset the stored offset.
//
// Until now nothing exercised them — the Go guards pin refresh.js's SOURCE
// STRINGS, which match equally whether the staleness check runs before or after
// the failure report. These tests drive the real refresh() against a scripted
// transport instead.
//
// Everything that PAINTS is left real but unasserted (the renderers no-op on a
// bare document); everything that GUARDS is real and asserted. Only the app
// entry is replaced: dashboard.js has top-level side effects and pulls in the
// whole module graph, while refresh.js needs just the shared-snapshot slot it
// owns.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const DASHBOARD_STUB = `
export let lastSnapshot = null;
export function setLastSnapshot(next) { lastSnapshot = next; }
`;

const SNAPSHOT = { static_version: 'v1', agents: [] };

// listPage builds one windowed list endpoint's body (the /api/retired shape).
const listPage = (offset) => ({ rows: [], offset, limit: 50, total: 500, total_unfiltered: 500 });

const jsonResponse = (body) => ({ ok: true, status: 200, json: async () => body });

// rejectsOnAbort mirrors the platform: a fetch whose signal is aborted rejects
// with the abort reason. refresh() supersedes an older generation this way.
const rejectsOnAbort = (signal) => new Promise((_, reject) => {
  if (signal.aborted) reject(signal.reason);
  else signal.addEventListener('abort', () => reject(signal.reason), { once: true });
});

const flush = () => new Promise((resolve) => { setImmediate(resolve); });

// refreshHarness stands refresh.js up against a scripted transport. `transport`
// receives (path, signal) and returns a Response-ish; `calls` records every
// request path in order.
async function refreshHarness(t, transport) {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/dashboard.js', DASHBOARD_STUB);
  // The Groups tab is the one whose virtual groups gate the windowed lists.
  harness.document.body.innerHTML =
    '<nav></nav><main><section id="tab-groups" class="active"></section></main>';

  // LinkeDOM has no browsing context, so it supplies no rAF. A renderer on the
  // commit path schedules through it; without this the cycle would fail for a
  // reason that has nothing to do with the guards under test.
  const previousRAF = globalThis.requestAnimationFrame;
  const previousCAF = globalThis.cancelAnimationFrame;
  globalThis.requestAnimationFrame = (callback) => setTimeout(() => callback(0), 0);
  globalThis.cancelAnimationFrame = (handle) => clearTimeout(handle);

  const calls = [];
  const previousFetch = globalThis.fetch;
  // async so the transport ALWAYS hands back a promise, as the platform does —
  // list-paging's sub-fetch wrapper calls .catch() on whatever fetch returns.
  globalThis.fetch = async (path, init = {}) => {
    calls.push(path);
    return transport(path, init.signal);
  };
  t.after(() => {
    globalThis.fetch = previousFetch;
    globalThis.requestAnimationFrame = previousRAF;
    globalThis.cancelAnimationFrame = previousCAF;
  });

  const { refresh } = await harness.importDashboardModule('js/refresh.js');
  const { dashboardState } = await harness.importDashboardModule('js/snapshot-store.js');
  const { registerFeatureState } = await harness.importDashboardModule('js/feature-state-registry.js');
  const listPaging = await harness.importDashboardModule('js/list-paging.js');
  const { signal } = harness.signals;

  // The whole of the Groups feature contract the poll path touches: the filter
  // query and virtual-group visibility that gate the list fetches, plus the
  // publish/rerender sinks the renderers call. Stand in for those rather than
  // booting the island.
  registerFeatureState('groups', {
    query: signal(''),
    visibility: signal({ retired: true, conversations: false, replaced: false }),
    publish() {},
    rerender() {},
  });

  return { harness, refresh, dashboardState, registerFeatureState, listPaging, calls };
}

// The window the guard has to cover: refresh A began while the Jobs tab was
// active, so it holds the jobs token. The operator switches away — which does
// not itself refresh — so the superseding tick B has jobsActive === false and
// never re-begins that token. A's abort must therefore not be reported as a
// jobs FAILURE: nothing would rewrite it, and the operator would find a stale
// error waiting the next time they opened the tab.
test('a superseded refresh discards the jobs request instead of failing it', async (t) => {
  let round = 0;
  const h = await refreshHarness(t, (path, signal) => {
    // Round one hangs until superseded; everything after answers normally.
    if (round === 0) return rejectsOnAbort(signal);
    return jsonResponse(path.startsWith('/api/snapshot') ? SNAPSHOT : listPage(0));
  });
  const { createJobsState } = await h.harness.importDashboardModule('js/jobs-state.js');
  const jobs = createJobsState();
  h.registerFeatureState('jobs', jobs);

  h.dashboardState.setActiveTab('jobs');
  const superseded = h.refresh();
  await flush();
  assert.equal(jobs.request.value.phase, 'loading', 'the jobs token belongs to the first refresh');
  const jobsToken = jobs.request.value.requestId;

  // Switching away does not refresh, so the next tick simply runs with the Jobs
  // tab inactive — and leaves that token exactly where it was.
  h.dashboardState.setActiveTab('groups');
  round = 1;
  await h.refresh();
  await superseded;

  assert.equal(jobs.request.value.requestId, jobsToken, 'no successor re-begins the jobs token');
  assert.equal(
    jobs.request.value.phase,
    'idle',
    'a superseded generation must discard the jobs request, not fail it',
  );
  assert.equal(jobs.request.value.error, null, 'an abort must not surface as a jobs error');
});

// The pager-clobber guard: a stale generation that resumes LAST must not write
// the offset the newer page has already moved past, and must not publish the
// snapshot it fetched. Superseding aborts the fetches, but a generation whose
// responses ALREADY ARRIVED is past the point an abort can reach.
//
// The staleness re-check that matters here is the one AFTER the list bodies are
// read: this generation is still current when its snapshot body resolves and
// only loses the race while stitching, which is the last async boundary before
// the offset write. Superseding it any earlier would prove nothing about that
// guard — an earlier check would already have caught it.
test('a superseded refresh publishes nothing and cannot move a served offset', async (t) => {
  let releaseListBody;
  let round = 0;
  const h = await refreshHarness(t, (path) => {
    if (path.startsWith('/api/retired')) {
      // Headers are in, body not yet read. Round one's page claims a deep
      // offset that the newer generation has already moved away from.
      if (round === 0) {
        return {
          ok: true,
          status: 200,
          json: () => new Promise((r) => { releaseListBody = () => r(listPage(400)); }),
        };
      }
      return jsonResponse(listPage(0));
    }
    return jsonResponse(SNAPSHOT);
  });

  const stale = h.refresh();
  await flush();
  assert.equal(typeof releaseListBody, 'function', 'the first generation is mid-stitch');

  // A newer generation lands and commits while the older one is still stitching.
  round = 1;
  await h.refresh();
  const published = h.dashboardState.snapshot.value;
  assert.equal(h.listPaging.listOffset('retired'), 0, 'the newer generation owns the offset');

  releaseListBody();
  await stale;

  assert.equal(h.dashboardState.snapshot.value, published, 'a stale generation must not publish');
  assert.equal(
    h.listPaging.listOffset('retired'),
    0,
    'a stale generation must not write the offset the newer page moved past',
  );

  // Positive control: the same payload through a CURRENT generation does sync,
  // so the assertions above are about staleness and not about the plumbing.
  round = 0;
  releaseListBody = undefined;
  const current = h.refresh();
  await flush();
  releaseListBody();
  await current;
  assert.equal(h.listPaging.listOffset('retired'), 400, 'a current generation syncs the served offset');
});

// Boot narrowing, at the refresh() end: the paint curtain must not wait on the
// Groups tab's heavy paginated lists. The virtual groups above are VISIBLE and
// the Groups tab is active, so a full cycle does fetch them — the narrowing is
// what keeps them off the boot path.
test('includeLists:false fetches the snapshot alone and still commits', async (t) => {
  const h = await refreshHarness(t, (path) =>
    jsonResponse(path.startsWith('/api/snapshot') ? SNAPSHOT : listPage(0)));

  await h.refresh({ includeLists: false });
  assert.deepEqual(h.calls, ['/api/snapshot'], 'boot must ask for the snapshot alone');
  assert.equal(h.dashboardState.snapshot.value?.static_version, 'v1', 'and must still commit it');

  h.calls.length = 0;
  await h.refresh();
  assert.ok(
    h.calls.some((path) => path.startsWith('/api/retired')),
    'a normal cycle still fetches the visible list, so the narrowing is what suppressed it',
  );
});
