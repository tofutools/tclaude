// The operator's simple reference implementation process (tk_c324d5a4,
// example 2), clicked together in the inline editor on the Groups tab in a
// real browser: + after, ⑂ branch, the inspector's name, text, Who, outcome
// and Which-agent fields, and the params row. The saved template must be the
// one jstest/process-reference-recipes.mjs builds with the same operations
// (the fixture the Go tests validate and run). SHOTS=<dir> also saves the
// finished graph, the advanced example's graph (from the recipe's fixture,
// shown in the same editor), and a composite of the two.
//
//   scripts/sandbox/ui.sh smoke process-reference-inline
import { test, expect } from '@playwright/test';
import { readFileSync } from 'node:fs';
import path from 'node:path';

const GROUP = 'webshop';
const recipes = JSON.parse(readFileSync(new URL('../../../../pkg/claude/agentd/jstest/process-reference-recipes.json', import.meta.url), 'utf8')).cases;

async function booted(page) {
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
}

function collectErrors(page) {
  const errors = [];
  page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
  page.on('console', (message) => {
    if (message.type() === 'error' && !/429 \/api\/companion\/presence/.test(message.text())) errors.push(`console: ${message.text()}`);
  });
  return errors;
}

const getTemplate = (page, id) => page.evaluate(async (url) => (await fetch(url, { credentials: 'same-origin' })).json(),
  `/v1/process/templates/${encodeURIComponent(id)}`);

// saveView saves a new version of template id, as the editor's save does.
const saveView = (page, id, view, sourceHash) => page.evaluate(async ({ id, view, sourceHash }) => {
  const path = `/v1/process/templates/${encodeURIComponent(id)}`;
  const body = JSON.stringify({ template: { ...view.template, id }, edges: view.edges, layout: view.layout, sourceHash });
  const digest = [...new Uint8Array(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(`POST\x00${path}\x00${body}`)))]
    .map((b) => b.toString(16).padStart(2, '0')).join('');
  const response = await fetch(path, {
    method: 'POST', credentials: 'same-origin', body,
    headers: { 'Content-Type': 'application/json', 'Idempotency-Key': crypto.randomUUID(), 'X-Tclaude-Request-Digest': digest },
  });
  return response.status;
}, { id, view, sourceHash });

// shape is what the recipe and the clicks must agree on: the steps, who
// does each, what they are asked, their answers and where those go, loops,
// and the params. Layout and the build step's role (the sandbox group has
// its own roles) aside.
function shape(view) {
  const nodes = {};
  for (const [id, node] of Object.entries(view.template.nodes)) {
    const performer = node.performer ? { ...node.performer } : undefined;
    if (performer && id === 'build-the-feature') delete performer.role;
    nodes[id] = { type: node.type, name: node.name, performer, loops: node.loops, result: node.result };
  }
  const edges = view.edges.filter((edge) => edge.from).map((edge) => `${edge.from} -${edge.outcome}-> ${edge.to}`).sort();
  return { nodes: JSON.parse(JSON.stringify(nodes)), edges, params: view.template.params };
}

test.use({ viewport: { width: 1600, height: 1000 } });
test.setTimeout(180_000);

test('the simple reference process clicks together inline into what its recipe builds', async ({ page }) => {
  const errors = collectErrors(page);
  await page.goto('/groups');
  await booted(page);
  const section = page.locator(`[data-fused-processes="${GROUP}"]`);
  if (!(await section.isVisible())) await page.locator(`strong[data-group-name="${GROUP}"]`).click();
  await expect(section).toBeVisible();
  await section.locator(`[data-resource-add="${GROUP}"]`).first().click();
  await page.locator('[data-resource-new-process]').click();
  const editor = section.locator('[data-mini-editor]');
  await expect(editor).toBeVisible();
  const templateId = await editor.getAttribute('data-mini-editor');
  await page.keyboard.type('Simple implementation');
  await page.keyboard.press('Enter');

  const inspector = editor.locator('[data-mini-inspector]');
  const selected = async () => inspector.getAttribute('data-mini-inspector');
  // pick lands one kind from the open picker; the new step is selected.
  const pick = async (kind) => {
    await page.locator(`[data-mini-picker] [data-mini-kind="${kind}"]`).click();
    await expect(inspector).toBeVisible();
    return selected();
  };
  const handle = async (id, op, kind) => {
    await editor.locator(`[data-process-node="${id}"]`).hover();
    await editor.locator(`[data-mini-handles="${id}"] [data-mini-handle="${op}"]`).click();
    return pick(kind);
  };
  const field = async (locator, value) => {
    await locator.fill(value);
    await locator.blur();
  };
  const choose = async (select, preferred) => {
    const values = await select.locator('option').evaluateAll((options) => options.map((option) => option.value));
    const value = preferred.find((want) => values.includes(want));
    expect(value, `one of ${preferred} in ${values}`).toBeTruthy();
    await select.selectOption(value);
  };
  // fill gives the selected step its name (its id follows), text and Who.
  const fill = async ({ name, text, who }) => {
    await field(inspector.locator('[data-mini-name]'), name);
    // A draft's step ids follow their names (process-step-ids.js stepSlug).
    const slug = name.toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/^[^a-z0-9]+/, '').replace(/-+$/, '');
    await expect(inspector).toHaveAttribute('data-mini-inspector', slug);
    await field(inspector.locator('[data-mini-text]'), text);
    if (who) await choose(inspector.locator('.gq-who-select'), who);
    return selected();
  };
  const outcome = (name) => inspector.locator(`[data-mini-outcome="${name}"]`);
  const rename = async (from, to) => {
    await field(outcome(from).locator('.gm-outcome-name'), to);
    await expect(outcome(to)).toBeVisible();
  };
  const aim = async (name, to) => outcome(name).locator('select').selectOption(to);
  const select = async (id) => {
    await editor.locator(`[data-process-node="${id}"]`).click();
    await expect(inspector).toHaveAttribute('data-mini-inspector', id);
  };

  // The operator and the lead discuss the idea; the operator decides it.
  await editor.locator('[data-process-node="mini-first-step"]').click();
  await pick('agent');
  const discuss = await fill({ name: 'Discuss the idea', text: 'Discuss {{idea}} with the operator until the high-level idea is clear. Write it down.', who: ['lead'] });
  await handle(discuss, 'branch', 'decide-you');
  const decided = await fill({ name: 'Idea decided', text: 'Is the high-level idea decided?' });
  await rename('yes', 'decided');
  await rename('no', 'not-yet');
  await aim('not-yet', discuss);
  // The lead provisions an implementor, who builds it.
  await handle(decided, 'after', 'agent');
  const provision = await fill({ name: 'Provision an implementor', text: 'Bring in an implementor for the idea and brief it.', who: ['lead'] });
  await handle(provision, 'after', 'agent');
  const build = await fill({ name: 'Build the feature', text: 'Build the agreed idea on a branch and open a draft PR.', who: ['role:implementor', 'role:dev', 'anyone'] });
  // One cold reviewer, not the one who built it.
  await handle(build, 'after', 'agent');
  const review = await fill({ name: 'Cold review', text: 'Review the PR cold, as someone who did not build it. List what you find.', who: ['anyone'] });
  await inspector.locator(`[data-mini-constraint="${review}"] select`).selectOption(`different:${build}`);
  // The lead decides whether the review warrants another round.
  await handle(review, 'after', 'decide');
  const decide = await fill({ name: 'Iterate or move on', text: 'Does the review warrant another round of changes, or does it move ahead?', who: ['lead'] });
  await rename('yes', 'move-on');
  await rename('no', 'iterate');
  await aim('iterate', build);
  // PR ready and notify, then merge if the run allows it.
  await handle(decide, 'after', 'agent');
  const ready = await fill({ name: 'Ready the PR', text: 'Mark the PR ready for review and notify the operator with a short summary.', who: ['lead'] });
  await handle(ready, 'after', 'decide');
  const merge = await fill({ name: 'Merge', text: 'Auto-merge is {{auto_merge}}. Answer merge only if it is true and CI on the PR passed; otherwise leave it for the operator.', who: ['lead'] });
  await rename('yes', 'merge');
  await rename('no', 'leave');
  await handle(merge, 'after', 'agent');
  await fill({ name: 'Merge the PR', text: 'Merge the PR now that CI passed.', who: ['lead'] });
  // The params the steps use are declared already; Params sets the rest.
  await expect(editor.locator('[data-mini-params]')).toHaveAttribute('data-mini-params', 'idea,auto_merge');
  await editor.locator('[data-mini-params-edit]').click();
  const dialog = page.locator('.process-param-dialog');
  await expect(dialog).toBeVisible();
  const idea = dialog.locator('[data-process-param="idea"]');
  await idea.locator('.process-param-description').fill('The idea to build');
  const autoMerge = dialog.locator('[data-process-param="auto_merge"]');
  await autoMerge.locator('.process-param-type').fill('boolean');
  await autoMerge.locator('.process-param-description').fill('Merge on green CI without asking');
  await autoMerge.locator('.process-param-required').uncheck();
  await autoMerge.locator('.process-param-default-enabled').check();
  await autoMerge.locator('.process-param-default').fill('false');
  await dialog.getByRole('button', { name: 'Apply' }).click();
  await expect(dialog).toHaveCount(0);
  await expect(editor.locator('[data-mini-param="auto_merge"]')).toHaveText('auto_merge: boolean = false');
  await select(merge);
  await inspector.getByRole('button', { name: 'done' }).click();

  // It saves itself, as the recipe builds it, with no problems.
  await expect(editor.locator('[data-mini-autosave]')).toHaveText(`draft · autosaved to ${GROUP}`, { timeout: 15_000 });
  await expect(editor.locator('[data-mini-problems]')).toHaveCount(0);
  await expect.poll(async () => shape(await getTemplate(page, templateId)), { timeout: 15_000 }).toEqual(shape(recipes.simple));
  const saved = await getTemplate(page, templateId);
  // graphShot saves the whole graph: a viewport wide enough not to scroll
  // it, and the sandbox's access banner out of the way.
  const graphShot = async (name) => {
    await page.setViewportSize({ width: 3400, height: 1000 });
    await page.evaluate(() => {
      for (const element of document.querySelectorAll('body *')) {
        if (element.children.length === 0 && /is requesting access/.test(element.textContent || '')) {
          let box = element;
          while (box.parentElement && getComputedStyle(box).position !== 'fixed') box = box.parentElement;
          box.style.display = 'none';
        }
      }
    });
    await editor.locator('.pk-mini-graph').screenshot({ path: path.join(process.env.SHOTS, name) });
    await page.setViewportSize({ width: 1600, height: 1000 });
  };
  if (process.env.SHOTS) await graphShot('simple-clicked.png');

  // The advanced example's graph, from its recipe, in the same editor: an
  // agent saving a newer version shows here.
  if (process.env.SHOTS) {
    expect([200, 201]).toContain(await saveView(page, templateId, recipes.advanced, saved.sourceHash));
    await expect(editor.locator('[data-process-node="review"]')).toBeVisible({ timeout: 20_000 });
    await graphShot('advanced-recipe.png');
    const data = (name) => `data:image/png;base64,${readFileSync(path.join(process.env.SHOTS, name)).toString('base64')}`;
    const composite = await page.context().newPage();
    await composite.setViewportSize({ width: 3400, height: 800 });
    await composite.setContent(`<body style="margin:0;background:#0d1117;color:#c9d1d9;font:14px sans-serif">
      <div style="padding:12px 16px">Example 2, simple: clicked together inline</div><img src="${data('simple-clicked.png')}" style="display:block;max-width:100%">
      <div style="padding:12px 16px">Example 1, advanced: built by its inline recipe</div><img src="${data('advanced-recipe.png')}" style="display:block;max-width:100%">
    </body>`);
    await composite.screenshot({ path: path.join(process.env.SHOTS, 'reference-processes.png'), fullPage: true });
    await composite.close();
  }
  expect(errors).toEqual([]);
});
