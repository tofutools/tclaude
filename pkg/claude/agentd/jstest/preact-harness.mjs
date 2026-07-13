import assert from 'node:assert/strict';
import { cp, mkdir, mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises';
import { dirname, join, relative, sep } from 'node:path';
import { tmpdir } from 'node:os';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { HTMLClasses, parseHTML } from './vendor/linkedom.mjs';

const dashboardDir = fileURLToPath(new URL('../dashboard/', import.meta.url));
const testUtilsSource = fileURLToPath(new URL('./vendor/preact-test-utils.mjs', import.meta.url));

function moduleSpecifier(fromFile, toFile) {
  let value = relative(dirname(fromFile), toFile).split(sep).join('/');
  if (!value.startsWith('.')) value = `./${value}`;
  return value;
}

function rewriteBareImports(source, fromFile, targets) {
  for (const [specifier, target] of targets) {
    const replacement = moduleSpecifier(fromFile, target);
    source = source
      .replaceAll(`"${specifier}"`, `"${replacement}"`)
      .replaceAll(`'${specifier}'`, `'${replacement}'`);
  }
  return source;
}

async function walkJS(dir) {
  const files = [];
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) files.push(...await walkJS(path));
    else if (entry.isFile() && entry.name.endsWith('.js')) files.push(path);
  }
  return files;
}

// Node does not implement browser import maps. Mirror the committed dashboard
// modules into a temporary ESM package, then rewrite only the map's bare
// specifiers to their exact same-origin targets. Relative application imports
// keep working unchanged, so tests exercise production module boundaries.
async function materializeDashboardModules(t) {
  const dashboardHTML = await readFile(join(dashboardDir, 'dashboard.html'), 'utf8');
  const match = dashboardHTML.match(/<script type="importmap">\s*([\s\S]*?)\s*<\/script>/);
  assert.ok(match, 'dashboard contains an import map');
  const importMap = JSON.parse(match[1]);
  assert.equal(typeof importMap.imports, 'object', 'import map contains imports');

  const workDir = await mkdtemp(join(tmpdir(), 'tclaude-preact-dom-'));
  t.after(() => rm(workDir, { recursive: true, force: true }));
  await writeFile(join(workDir, 'package.json'), '{"type":"module"}\n');
  await cp(join(dashboardDir, 'js'), join(workDir, 'js'), { recursive: true });

  const targets = new Map();
  for (const [specifier, target] of Object.entries(importMap.imports)) {
    assert.match(target, /^\/static\/vendor\/preact\/[^/]+\.js$/);
    const source = join(dashboardDir, target.slice('/static/'.length));
    const output = join(workDir, target.slice('/static/'.length));
    await mkdir(dirname(output), { recursive: true });
    await cp(source, output);
    targets.set(specifier, output);
  }

  const testUtils = join(workDir, 'vendor/preact/preact-test-utils.mjs');
  await cp(testUtilsSource, testUtils);
  targets.set('preact/test-utils', testUtils);

  for (const file of await walkJS(workDir)) {
    const source = await readFile(file, 'utf8');
    await writeFile(file, rewriteBareImports(source, file, targets));
  }
  // test-utils uses .mjs, so it is outside walkJS's production .js set.
  await writeFile(
    testUtils,
    rewriteBareImports(await readFile(testUtils, 'utf8'), testUtils, targets),
  );

  return { workDir, targets };
}

function installDOM(t) {
  const { window } = parseHTML('<!doctype html><html><body></body></html>');
  const { document } = window;
  const globals = [...new Set([
    'window', 'document', 'Node', 'Element', 'HTMLElement', 'Event',
    'CustomEvent', 'InputEvent', 'MutationObserver', 'navigator',
    ...Object.keys(HTMLClasses),
  ])];
  const previous = new Map(globals.map((name) => [
    name,
    Object.getOwnPropertyDescriptor(globalThis, name),
  ]));
  for (const name of globals) {
    if (window[name] !== undefined) {
      Object.defineProperty(globalThis, name, {
        configurable: true,
        writable: true,
        value: window[name],
      });
    }
  }
  Object.defineProperty(globalThis, 'window', { configurable: true, writable: true, value: window });
  Object.defineProperty(globalThis, 'document', { configurable: true, writable: true, value: document });

  // LinkeDOM exposes HTMLDetailsElement but currently constructs <details> as
  // its HTMLElement base. Preserve the browser's instanceof contract used by
  // refresh.js and dock.js without making unrelated elements match.
  const detailsHasInstance = Object.getOwnPropertyDescriptor(
    window.HTMLDetailsElement,
    Symbol.hasInstance,
  );
  Object.defineProperty(window.HTMLDetailsElement, Symbol.hasInstance, {
    configurable: true,
    value: (candidate) => candidate instanceof window.HTMLElement && candidate.localName === 'details',
  });

  // LinkeDOM intentionally does not emulate browsing-context focus. This tiny
  // patch supplies the activeElement contract needed to prove that Preact's
  // keyed reconciliation preserves the focused node; all DOM/events/querying
  // still come from the maintained runtime.
  let activeElement = document.body;
  Object.defineProperty(document, 'activeElement', {
    configurable: true,
    get() {
      if (!activeElement?.isConnected) activeElement = document.body;
      return activeElement;
    },
  });
  const focusDescriptor = Object.getOwnPropertyDescriptor(window.HTMLElement.prototype, 'focus');
  const blurDescriptor = Object.getOwnPropertyDescriptor(window.HTMLElement.prototype, 'blur');
  Object.defineProperty(window.HTMLElement.prototype, 'focus', {
    configurable: true,
    value() {
      const previous = document.activeElement;
      if (previous === this) return;
      if (previous && previous !== document.body) {
        previous.dispatchEvent(new window.Event('blur'));
      }
      activeElement = this;
      this.dispatchEvent(new window.Event('focus'));
    },
  });
  Object.defineProperty(window.HTMLElement.prototype, 'blur', {
    configurable: true,
    value() {
      if (document.activeElement !== this) return;
      this.dispatchEvent(new window.Event('blur'));
      activeElement = document.body;
    },
  });

  t.after(() => {
    if (detailsHasInstance) {
      Object.defineProperty(window.HTMLDetailsElement, Symbol.hasInstance, detailsHasInstance);
    } else {
      delete window.HTMLDetailsElement[Symbol.hasInstance];
    }
    if (focusDescriptor) Object.defineProperty(window.HTMLElement.prototype, 'focus', focusDescriptor);
    else delete window.HTMLElement.prototype.focus;
    if (blurDescriptor) Object.defineProperty(window.HTMLElement.prototype, 'blur', blurDescriptor);
    else delete window.HTMLElement.prototype.blur;
    for (const [name, descriptor] of previous) {
      if (descriptor === undefined) delete globalThis[name];
      else Object.defineProperty(globalThis, name, descriptor);
    }
  });
  return { window, document };
}

function normalize(text) {
  return String(text ?? '').replace(/\s+/g, ' ').trim();
}

function labelFor(root, element) {
  if (!element.id) return null;
  return [...root.querySelectorAll('label')]
    .find((label) => label.getAttribute('for') === element.id) ?? null;
}

function accessibleName(root, element) {
  const ariaLabel = element.getAttribute('aria-label');
  if (ariaLabel) return normalize(ariaLabel);
  const labelledBy = element.getAttribute('aria-labelledby');
  if (labelledBy) {
    return normalize(labelledBy.split(/\s+/)
      .map((id) => root.querySelector(`[id="${id}"]`)?.textContent)
      .join(' '));
  }
  return normalize(labelFor(root, element)?.textContent ?? element.textContent);
}

const implicitRoleSelectors = {
  button: 'button,input[type="button"],input[type="submit"],input[type="reset"]',
  textbox: 'textarea,input:not([type]),input[type="text"],input[type="search"],input[type="email"]',
};

function matchesName(actual, expected) {
  return expected instanceof RegExp ? expected.test(actual) : actual === normalize(expected);
}

export function getByRole(root, role, options = {}) {
  const selector = [`[role="${role}"]`, implicitRoleSelectors[role]].filter(Boolean).join(',');
  const matches = [...root.querySelectorAll(selector)]
    .filter((element) => options.name === undefined || matchesName(accessibleName(root, element), options.name));
  assert.equal(matches.length, 1, `expected one ${role} named ${String(options.name)}, found ${matches.length}`);
  return matches[0];
}

export function getByLabelText(root, expected) {
  const labels = [...root.querySelectorAll('label')]
    .filter((label) => matchesName(normalize(label.textContent), expected));
  assert.equal(labels.length, 1, `expected one label ${String(expected)}, found ${labels.length}`);
  const label = labels[0];
  const control = label.getAttribute('for')
    ? root.querySelector(`[id="${label.getAttribute('for')}"]`)
    : label.querySelector('input,textarea,select,button');
  assert.ok(control, `label ${String(expected)} has a control`);
  return control;
}

export async function createPreactHarness(t) {
  const { workDir, targets } = await materializeDashboardModules(t);
  const { window, document } = installDOM(t);
  const loadTarget = (specifier) => import(pathToFileURL(targets.get(specifier)));
  const [preact, hooks, signals, htmModule, testUtils] = await Promise.all([
    loadTarget('preact'),
    loadTarget('preact/hooks'),
    loadTarget('@preact/signals'),
    loadTarget('htm'),
    loadTarget('preact/test-utils'),
  ]);
  const html = htmModule.default.bind(preact.h);

  const importDashboardModule = (path) => import(pathToFileURL(join(workDir, path)));
  const replaceDashboardModule = (path, source) => writeFile(join(workDir, path), source);
  const fireEvent = (element, type, init = {}) => {
    const event = new window.Event(type, { bubbles: true, cancelable: true });
    Object.assign(event, init);
    element.dispatchEvent(event);
    return event;
  };
  const input = async (element, value) => {
    element.value = value;
    await testUtils.act(() => fireEvent(element, 'input'));
  };
  const mount = async (vnode, container) => {
    const ownsContainer = container === undefined;
    if (ownsContainer) container = document.body.appendChild(document.createElement('div'));
    await testUtils.act(() => preact.render(vnode, container));
    return {
      container,
      rerender: async (next) => testUtils.act(() => preact.render(next, container)),
      unmount: async () => {
        await testUtils.act(() => preact.render(null, container));
        if (ownsContainer) container.remove();
      },
    };
  };

  return {
    window,
    document,
    html,
    preact,
    hooks,
    signals,
    act: testUtils.act,
    fireEvent,
    input,
    mount,
    importDashboardModule,
    replaceDashboardModule,
    getByRole,
    getByLabelText,
  };
}
