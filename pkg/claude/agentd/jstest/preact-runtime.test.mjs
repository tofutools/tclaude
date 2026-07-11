import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { basename, join } from 'node:path';
import { tmpdir } from 'node:os';
import { fileURLToPath, pathToFileURL } from 'node:url';

const dashboardDir = fileURLToPath(new URL('../dashboard/', import.meta.url));

// The probe only needs this small DOM subset. Keeping it test-local lets CI
// execute Preact's real renderer without adding jsdom/linkedom or shipping a
// browser-test dependency in the embedded dashboard.
class TestNode {
  constructor(nodeType) {
    this.nodeType = nodeType;
    this.parentNode = null;
    this.childNodes = [];
  }

  get firstChild() {
    return this.childNodes[0] ?? null;
  }

  get nextSibling() {
    if (!this.parentNode) return null;
    const siblings = this.parentNode.childNodes;
    return siblings[siblings.indexOf(this) + 1] ?? null;
  }

  insertBefore(child, reference) {
    if (child.parentNode) child.parentNode.removeChild(child);
    const index = reference === null ? this.childNodes.length : this.childNodes.indexOf(reference);
    assert.notEqual(index, -1, 'insertBefore reference belongs to parent');
    this.childNodes.splice(index, 0, child);
    child.parentNode = this;
    return child;
  }

  appendChild(child) {
    return this.insertBefore(child, null);
  }

  removeChild(child) {
    const index = this.childNodes.indexOf(child);
    assert.notEqual(index, -1, 'removeChild target belongs to parent');
    this.childNodes.splice(index, 1);
    child.parentNode = null;
    return child;
  }

  get textContent() {
    return this.childNodes.map((child) => child.textContent).join('');
  }
}

class TestText extends TestNode {
  constructor(data) {
    super(3);
    this.data = String(data);
  }

  get textContent() {
    return this.data;
  }
}

class TestElement extends TestNode {
  constructor(localName, namespaceURI = 'http://www.w3.org/1999/xhtml') {
    super(1);
    this.localName = localName;
    this.namespaceURI = namespaceURI;
    this.attributes = new Map();
    this.style = { setProperty() {} };
  }

  setAttribute(name, value) {
    this.attributes.set(name, String(value));
  }

  removeAttribute(name) {
    this.attributes.delete(name);
  }

  getAttribute(name) {
    return this.attributes.get(name) ?? null;
  }

  addEventListener() {}
  removeEventListener() {}
}

function installTestDOM(t) {
  const document = {
    createElement: (name) => new TestElement(name),
    createElementNS: (namespace, name) => new TestElement(name, namespace),
    createTextNode: (data) => new TestText(data),
  };
  globalThis.document = document;
  globalThis.window = { document };
  t.after(() => {
    delete globalThis.document;
    delete globalThis.window;
  });
  return document;
}

// Node does not implement browser import maps. Materialize the committed map
// into a temporary relative ESM graph so CI executes the exact vendored bytes
// and the same package-to-file resolution that the browser receives.
async function materializeImportMap(t) {
  const html = await readFile(join(dashboardDir, 'dashboard.html'), 'utf8');
  const match = html.match(/<script type="importmap">\s*([\s\S]*?)\s*<\/script>/);
  assert.ok(match, 'dashboard contains an import map');
  const { imports } = JSON.parse(match[1]);
  assert.equal(typeof imports, 'object');

  const workDir = await mkdtemp(join(tmpdir(), 'tclaude-preact-esm-'));
  t.after(() => rm(workDir, { recursive: true, force: true }));

  const outputNames = new Map(
    Object.entries(imports).map(([specifier, target]) => [
      specifier,
      basename(target).replace(/\.js$/, '.mjs'),
    ]),
  );
  const rewriteBareImports = (source) => {
    for (const [specifier, outputName] of outputNames) {
      source = source
        .replaceAll(`"${specifier}"`, `"./${outputName}"`)
        .replaceAll(`'${specifier}'`, `'./${outputName}'`);
    }
    return source;
  };

  for (const [specifier, target] of Object.entries(imports)) {
    assert.match(target, /^\/static\/vendor\/preact\/[^/]+\.js$/);
    const sourcePath = join(dashboardDir, target.slice('/static/'.length));
    const source = rewriteBareImports(await readFile(sourcePath, 'utf8'));
    await writeFile(join(workDir, outputNames.get(specifier)), source);
  }

  const probe = rewriteBareImports(
    await readFile(join(dashboardDir, 'js/preact-probe.js'), 'utf8'),
  );
  const probePath = join(workDir, 'preact-probe.mjs');
  await writeFile(probePath, probe);

  return { workDir, outputNames, probePath };
}

test('vendored import-map graph links and supports HTM components plus Signals', async (t) => {
  const { workDir, outputNames, probePath } = await materializeImportMap(t);
  const load = (specifier) => import(pathToFileURL(join(workDir, outputNames.get(specifier))));

  const document = installTestDOM(t);

  // Importing the real application module links the complete transitive graph:
  // Preact, Hooks, Signals, Signals Core, and HTM.
  const probe = await import(pathToFileURL(probePath));
  assert.equal(typeof probe.mountPreactProbe, 'function');

  const host = document.createElement('span');
  const unmount = probe.mountPreactProbe(host);
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(host.firstChild?.localName, 'span');
  assert.equal(host.firstChild?.getAttribute('data-preact-probe'), 'ready');
  assert.equal(host.firstChild?.textContent.trim(), 'ready');
  unmount();
  assert.equal(host.firstChild, null);

  const [preact, hooks, signals, htmModule] = await Promise.all([
    load('preact'),
    load('preact/hooks'),
    load('@preact/signals'),
    load('htm'),
  ]);
  assert.equal(typeof hooks.useState, 'function');

  const html = htmModule.default.bind(preact.h);
  const state = signals.signal('booting');
  const seen = [];
  const dispose = signals.effect(() => seen.push(state.value));
  state.value = 'ready';
  dispose();
  assert.deepEqual(seen, ['booting', 'ready']);

  function Leaf({ value }) {
    return html`<span>${value}</span>`;
  }
  const component = html`<${Leaf} value=${state} />`;
  assert.equal(component.type, Leaf);
  assert.equal(component.props.value, state);
  assert.equal(Leaf(component.props).type, 'span');
});
