import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';
const grant = () => ({id:1, agent_id:'agt_worker', conv_id:'conv-worker', conv_title:'worker', expires_at:new Date(Date.now()+1800000).toISOString()});

test('break-glass defaults to 30m, prevents duplicate grants, and Cancel closes',async t => {
 const harness = await createPreactHarness(t);
 const [{ createMessageAccessDialogState },{ MessageAccessDialogApp }] = await Promise.all([
  harness.importDashboardModule('js/message-access-dialog-state.js'),harness.importDashboardModule('js/message-access-dialog-island.js'),
 ]);
 const state = createMessageAccessDialogState(); state.openBreakGlass({agentID:'agt_worker'});
 const calls = []; let reject, pending;
 const actions = {breakGlass:args => { calls.push(args); return pending = new Promise((_,fail) => { reject = fail; }); }};
 const snapshot = {agents:[{agent_id:'agt_worker',title:'worker'}],break_glass:[]};
 const mounted = await harness.mount(harness.html`<${MessageAccessDialogApp} state=${state} actions=${actions} snapshot=${snapshot}/>`);
 const dialog = mounted.container.querySelector('#break-glass-modal');
 assert.match(dialog.textContent,/Agents can never request/);
 const submit = dialog.querySelector('.break-glass-confirm');
 await harness.act(() => { submit.click(); submit.click(); });
 assert.deepEqual(calls,[{target:'agt_worker',duration:'30m',reason:'',revoke:false}]);
 reject(new Error('grant failed'));
 await harness.act(async () => { try { await pending; } catch (_) {} });
 assert.match(dialog.textContent,/grant failed/);
 await harness.act(() => dialog.querySelector('.modal-buttons button').click());
 assert.equal(state.view.value.dialog,null);
 await mounted.unmount();
});

test('header stays visible after banner dismissal; peer views never advertise break-glass',async t => {
 const harness = await createPreactHarness(t);
 const [{ BreakGlassIndicator, BreakGlassBadge },{ createDashboardState }] = await Promise.all([
  harness.importDashboardModule('js/break-glass.js'),harness.importDashboardModule('js/snapshot-store.js'),
 ]);
 const state = createDashboardState(); state.beginRequest(); state.commitRequest(1,{break_glass:[grant()]});
 const mounted = await harness.mount(harness.html`<div><${BreakGlassIndicator} state=${state}/><${BreakGlassIndicator} state=${state} banner=${true}/><${BreakGlassBadge} member=${{agent_id:'agt_worker'}} snapshot=${state.snapshot.value}/></div>`);
 assert.match(mounted.container.querySelector('.break-glass-header').textContent,/1 live/);
 assert.ok(mounted.container.querySelector('.break-glass-banner'));
 assert.match(mounted.container.querySelector('[data-act="break-glass"]').title,/until/);
 await harness.act(() => mounted.container.querySelector('[aria-label="Dismiss break-glass banner for this page session"]').click());
 assert.equal(mounted.container.querySelector('.break-glass-banner'),null);
 assert.ok(mounted.container.querySelector('.break-glass-header'));
 state.beginRequest(); await harness.act(() => state.commitRequest(2,{peer_view:{},break_glass:[grant()]}));
 assert.equal(mounted.container.querySelector('.break-glass-header'),null);
 await mounted.unmount();
});

test('break-glass mutations use dedicated operator endpoints',async t => {
 const harness = await createPreactHarness(t);
 const {createMessageAccessDialogActions} = await harness.importDashboardModule('js/message-access-dialog-actions.js');
 const calls = [];
 const actions = createMessageAccessDialogActions({fetchImpl:async (url,options) => { calls.push({url,options}); return {ok:true,text:async () => '{}'}; }});
 await actions.breakGlass({target:'agt_worker',duration:'4h',reason:'host setup',revoke:false});
 assert.equal(calls[0].url,'/api/break-glass/grant');
 assert.deepEqual(JSON.parse(calls[0].options.body),{target:'agt_worker',duration:'4h',reason:'host setup'});
 await actions.breakGlass({target:'agt_worker',revoke:true}); assert.equal(calls[1].url,'/api/break-glass/revoke');
});
