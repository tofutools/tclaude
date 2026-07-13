import { h, render } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { normaliseFollowUp } from './action-dialog-actions.js';
import { registerActionDialogController } from './action-dialog-controller.js';
import { shortCwd } from './helpers.js';

const html = htm.bind(h);
const WT_NEW = '__new__';

function errorMessage(error) { return error?.message || String(error); }
function shortID(value) { return String(value || '').slice(0, 8); }

// Render both presentation vocabularies so an open dialog follows a live
// regular ↔ wizard theme flip without resetting its Preact-owned form state.
function Words({ plain, wizard }) {
  return html`<span class="theme-copy-regular">${plain}</span><span class="theme-copy-wizard">${wizard}</span>`;
}

function ErrorBanner({ id, error, onDismiss }) {
  const ref = useRef(null);
  useEffect(() => {
    const element = ref.current;
    if (!element || !error) return undefined;
    element.classList.remove('flash');
    void element.offsetWidth;
    element.classList.add('flash');
    element.scrollIntoView({ block: 'nearest' });
    const timer = setTimeout(() => element.classList.remove('flash'), 900);
    return () => clearTimeout(timer);
  }, [error]);
  if (!error) return html`<div ref=${ref} class="cron-create-error" id=${id} role="alert"></div>`;
  return html`
    <div ref=${ref} class="cron-create-error dismissible" id=${id} role="alert">
      <span class="cron-create-error-msg">${error}</span>
      <button type="button" class="cron-create-error-x" aria-label="Dismiss error" onClick=${onDismiss}>×</button>
    </div>
  `;
}

function WorktreeFields({ repo, actions, value, setValue, branch, setBranch, base, setBase }) {
  const [request, setRequest] = useState({ phase: 'loading', data: null, error: '' });
  useEffect(() => {
    const controller = new AbortController();
    setRequest({ phase: 'loading', data: null, error: '' });
    actions.loadWorktrees(repo, { signal: controller.signal }).then(
      (data) => setRequest({ phase: 'ready', data, error: '' }),
      (error) => {
        if (error?.name !== 'AbortError') setRequest({ phase: 'error', data: null, error: errorMessage(error) });
      },
    );
    return () => controller.abort();
  }, [repo]);

  const data = request.data || {};
  const isRepo = !!data.is_repo;
  const hasCommits = data.has_commits !== false;
  const branches = data.branches || [];
  useEffect(() => {
    if (!base && data.default_branch) setBase(data.default_branch);
  }, [data.default_branch]);

  let emptyLabel = '(no worktree — same directory as source)';
  if (request.phase === 'loading') emptyLabel = 'loading…';
  else if (request.phase === 'error') emptyLabel = '(could not load worktrees)';
  else if (!isRepo) emptyLabel = '(not a git repo — worktrees unavailable)';
  const selectedWorktree = value.startsWith('wt:')
    ? (data.worktrees || []).find((worktree) => worktree.path === value.slice(3))
    : null;
  const selectTitle = selectedWorktree
    ? `${selectedWorktree.branch || '(detached)'}${selectedWorktree.is_main ? ' [main]' : ''} — ${selectedWorktree.path}`
    : request.error || emptyLabel;

  return html`
    <label class="cron-create-row">
      <span class="cron-create-label">Worktree</span>
      <select
        id="clone-agent-worktree"
        value=${value}
        disabled=${request.phase !== 'ready' || !isRepo}
        title=${selectTitle}
        onChange=${(event) => setValue(event.currentTarget.value)}
      >
        <option value="">${emptyLabel}</option>
        ${(data.worktrees || []).map((worktree) => {
          const displayBranch = worktree.branch || '(detached)';
          const main = worktree.is_main ? ' [main]' : '';
          return html`
            <option
              key=${worktree.path}
              value=${`wt:${worktree.path}`}
              title=${`${displayBranch}${main} — ${worktree.path}`}
            >${displayBranch}${main} — ${shortCwd(worktree.path)}</option>
          `;
        })}
        ${isRepo && html`<option value=${WT_NEW}>+ create new worktree…</option>`}
      </select>
    </label>
    ${value === WT_NEW && html`
      <label class="cron-create-row" id="clone-agent-wt-new-row">
        <span class="cron-create-label">New branch</span>
        <input
          id="clone-agent-wt-branch"
          type="text"
          value=${branch}
          placeholder="branch name for the new worktree"
          autocomplete="off"
          spellcheck="false"
          onInput=${(event) => setBranch(event.currentTarget.value)}
        />
      </label>
      ${hasCommits ? html`
        <label class="cron-create-row" id="clone-agent-wt-base-row">
          <span class="cron-create-label">Base branch</span>
          <select id="clone-agent-wt-base" value=${base} onChange=${(event) => setBase(event.currentTarget.value)}>
            ${branches.map((name) => html`<option key=${name} value=${name}>${name}</option>`)}
          </select>
        </label>
      ` : html`
        <p class="wt-orphan-warn" id="clone-agent-wt-orphan-hint">
          ⚠ This repo has no commits yet, so the worktree will be cut as an
          <strong>orphan branch</strong> (no base to branch off). That's fine for
          bootstrapping a fresh repo — once you make the first commit, later
          worktrees branch off it normally.
        </p>
      `}
    `}
  `;
}

function CloneAgentDialog({ descriptor, actions, confirmDiscard }) {
  const [followUp, setFollowUp] = useState('');
  const [copyConversation, setCopyConversation] = useState(true);
  const [worktree, setWorktree] = useState('');
  const [branch, setBranch] = useState('');
  const [base, setBase] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const source = descriptor.label || shortID(descriptor.conv);
  const dirty = !!followUp || !copyConversation || !!worktree || !!branch;

  const submit = async () => {
    if (busy) return;
    setError('');
    setBusy(true);
    try {
      let cwd = '';
      if (worktree.startsWith('wt:')) cwd = worktree.slice(3);
      if (worktree === WT_NEW) {
        const cleanBranch = branch.trim();
        if (!cleanBranch) throw new Error('enter a branch name for the new worktree');
        const created = await actions.createWorktree({ repo: descriptor.cwd, branch: cleanBranch, fromBranch: base });
        cwd = created.path || '';
      }
      await actions.cloneAgent({
        conv: descriptor.conv,
        label: source,
        followUp,
        copyConversation,
        cwd,
      });
    } catch (cause) { setError(errorMessage(cause)); }
    finally { setBusy(false); }
  };

  return html`
    <${Overlay}
      id="clone-agent-modal"
      labelledby="clone-agent-title"
      onClose=${actions.close}
      onSubmitHotkey=${submit}
      dirty=${dirty}
      blocked=${busy}
      confirmDiscard=${confirmDiscard}
      resizeKey="tclaude.dash.modalSize.clone-agent"
    >
      <h3 id="clone-agent-title"><${Words} plain="Clone agent" wizard="⧉ Mirror familiar"/></h3>
      <div class="modal-meta" id="clone-agent-meta">
        source: ${source}${descriptor.cwd ? `  ·  ${descriptor.cwd}` : ''}
      </div>
      <label class="cron-create-row">
        <span class="cron-create-label">Follow-up</span>
        <textarea
          id="clone-agent-followup"
          rows="3"
          value=${followUp}
          placeholder="optional — typed into the new pane as a handoff message (no newlines, ≤4096 chars)"
          spellcheck="false"
          onInput=${(event) => setFollowUp(event.currentTarget.value)}
        ></textarea>
      </label>
      <label class="cron-create-enabled" title="When checked, copies the source conv.jsonl onto the clone so it starts with the full history. Uncheck for a fresh CC instance that only inherits identity.">
        <input
          id="clone-agent-copy-conv"
          type="checkbox"
          checked=${copyConversation}
          onChange=${(event) => setCopyConversation(event.currentTarget.checked)}
        /> Copy conversation history (jsonl)
      </label>
      <${WorktreeFields}
        repo=${descriptor.cwd}
        actions=${actions}
        value=${worktree}
        setValue=${setWorktree}
        branch=${branch}
        setBranch=${setBranch}
        base=${base}
        setBase=${setBase}
      />
      <${ErrorBanner} id="clone-agent-error" error=${error} onDismiss=${() => setError('')} />
      <div class="modal-buttons">
        <button id="clone-agent-cancel" type="button" disabled=${busy} onClick=${actions.close}><${Words} plain="Cancel" wizard="Dispel"/></button>
        <span class="spacer"></span>
        <button id="clone-agent-submit" class="primary" type="button" disabled=${busy} onClick=${submit}>
          <${Words} plain=${busy ? 'Cloning…' : 'Clone'} wizard=${busy ? 'Mirroring…' : 'Mirror familiar'}/>
        </button>
      </div>
    </${Overlay}>
  `;
}

function ReincarnateAgentDialog({ descriptor, actions, confirmDiscard }) {
  const [mode, setMode] = useState('self');
  const [focusHint, setFocusHint] = useState('');
  const [followUp, setFollowUp] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const focusHintRef = useRef(null);
  const followUpRef = useRef(null);
  const target = descriptor.label || shortID(descriptor.conv);
  const force = mode === 'force';
  const dirty = mode !== 'self' || !!focusHint || !!followUp;
  useEffect(() => {
    const timer = setTimeout(() => (force ? followUpRef.current : focusHintRef.current)?.focus(), 0);
    return () => clearTimeout(timer);
  }, [force]);

  const submit = async () => {
    if (busy || (force && !normaliseFollowUp(followUp))) return;
    setError('');
    setBusy(true);
    try { await actions.reincarnateAgent({ conv: descriptor.conv, label: target, mode, focusHint, followUp }); }
    catch (cause) { setError(errorMessage(cause)); }
    finally { setBusy(false); }
  };

  return html`
    <${Overlay}
      id="reincarnate-agent-modal"
      labelledby="reincarnate-agent-title"
      onClose=${actions.close}
      onSubmitHotkey=${submit}
      dirty=${dirty}
      blocked=${busy}
      confirmDiscard=${confirmDiscard}
    >
      <h3 id="reincarnate-agent-title"><${Words} plain="Reincarnate agent" wizard="Reincarnate familiar"/></h3>
      <div class="modal-meta" id="reincarnate-agent-meta">target: ${target}</div>
      <div class="reincarnate-mode" id="reincarnate-mode" role="radiogroup" aria-label="Reincarnate mode">
        <label>
          <input type="radio" name="reincarnate-mode" value="self" checked=${!force} onChange=${() => setMode('self')} />
          <span><${Words} plain="Ask the agent to reincarnate itself" wizard="Ask the familiar to reincarnate itself"/></span>
          <span class="opt-note"><${Words}
            plain="— graceful. Sends the agent a message; at its next clean point it writes its own handoff, collecting the context that matters, then reincarnates. May take a moment. Recommended."
            wizard="— graceful. Sends the familiar a missive; at its next clean point it writes its own handoff, gathers the lore that matters, then returns in a fresh vessel. Recommended."
          /></span>
        </label>
        <label>
          <input type="radio" name="reincarnate-mode" value="force" checked=${force} onChange=${() => setMode('force')} />
          <span>Force reincarnate now</span>
          <span class="opt-note"><${Words}
            plain="— immediate. The daemon spawns the successor and soft-exits the original right away. The agent gets no chance to write its own handoff — use only when it is stuck or unresponsive."
            wizard="— immediate. The daemon summons the successor and dismisses the original at once. The familiar cannot write its own handoff — use only when it is stuck or unresponsive."
          /></span>
        </label>
      </div>
      ${!force ? html`
        <div id="reincarnate-self-fields">
          <p class="modal-hint"><${Words}
            plain="The agent is messaged and reincarnates itself at a clean point. Because it collects its own context, the successor inherits a handoff that reflects the agent's actual working state."
            wizard="The familiar receives a missive and reincarnates at a clean point. Because it gathers its own lore, the successor inherits a handoff reflecting the familiar's true working state."
          /></p>
          <label class="cron-create-row">
            <span class="cron-create-label">Focus hint</span>
            <textarea ref=${focusHintRef} id="reincarnate-agent-focus" rows="3" maxlength="4000" value=${focusHint} placeholder="optional — what should it concentrate on while gathering context for its handoff? e.g. focus on the open questions about X, or capture the current state of subsystem Y. Leave blank for a general handoff." spellcheck="false" onInput=${(event) => setFocusHint(event.currentTarget.value)}></textarea>
          </label>
        </div>
      ` : html`
        <div id="reincarnate-force-fields">
          <p class="modal-hint"><${Words}
            plain=${html`Spawns a fresh CC instance that inherits identity (groups, perms, ownership). The original is soft-exited. The successor's title is auto-renamed to <code>${'<prev>-r-<N>'}</code>.`}
            wizard=${html`Summons a fresh familiar that inherits identity (parties, boons, ownership). The original is dismissed. The successor's title is auto-renamed to <code>${'<prev>-r-<N>'}</code>.`}
          /></p>
          <label class="cron-create-row">
            <span class="cron-create-label">Follow-up</span>
            <textarea ref=${followUpRef} id="reincarnate-agent-followup" rows="4" value=${followUp} placeholder="required — what should the successor pick up? Summarise the current task, where the relevant files are, what's next (no newlines, ≤16384 chars)" spellcheck="false" onInput=${(event) => setFollowUp(event.currentTarget.value)}></textarea>
          </label>
        </div>
      `}
      <${ErrorBanner} id="reincarnate-agent-error" error=${error} onDismiss=${() => setError('')} />
      <div class="modal-buttons">
        <button id="reincarnate-agent-cancel" type="button" disabled=${busy} onClick=${actions.close}><${Words} plain="Cancel" wizard="Dispel"/></button>
        <span class="spacer"></span>
        <button id="reincarnate-agent-submit" class="primary" type="button" disabled=${busy || (force && !normaliseFollowUp(followUp))} onClick=${submit}>
          <${Words}
            plain=${busy ? (force ? 'Reincarnating…' : 'Asking…') : (force ? 'Force reincarnate' : 'Ask agent')}
            wizard=${busy ? (force ? 'Reincarnating…' : 'Asking…') : (force ? 'Force reincarnate' : 'Ask familiar')}
          />
        </button>
      </div>
    </${Overlay}>
  `;
}

function NestGroupDialog({ descriptor, actions, confirmDiscard }) {
  const model = useMemo(() => actions.nestModel(descriptor.group), [descriptor.group]);
  const [parent, setParent] = useState(model.currentParent);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const dirty = parent !== model.currentParent;
  const submit = async () => {
    if (busy) return;
    setError('');
    setBusy(true);
    try { await actions.nestGroup({ group: descriptor.group, parent }); }
    catch (cause) { setError(errorMessage(cause)); }
    finally { setBusy(false); }
  };
  return html`
    <${Overlay} id="group-nest-modal" labelledby="group-nest-title" onClose=${actions.close} onSubmitHotkey=${submit} onSubmitEnter=${submit} dirty=${dirty} blocked=${busy} confirmDiscard=${confirmDiscard}>
      <h3 id="group-nest-title"><${Words} plain=${`Nest group: ${descriptor.group}`} wizard=${`Nest party: ${descriptor.group}`}/></h3>
      <div class="modal-meta"><${Words}
        plain="Nest this group under another so it draws inside it on the board — collapse the parent to tuck the whole subgroup away, expand it to bring it back. Board layout only: nesting doesn't change messaging, permissions or spawns. A group can't nest under itself or one of its own descendants."
        wizard="Nest this party under another so it draws inside it on the board — collapse the parent to tuck the whole subparty away, expand it to bring it back. Board layout only: nesting changes neither missives, boons, nor summons. A party cannot nest under itself or one of its descendants."
      /></div>
      <label class="cron-create-row">
        <span class="cron-create-label">Parent</span>
        <select id="group-nest-parent" value=${parent} onChange=${(event) => setParent(event.currentTarget.value)}>
          <option value="">— top level (no parent) —</option>
          ${model.candidates.map((name) => html`<option key=${name} value=${name}>${name}</option>`)}
        </select>
      </label>
      <${ErrorBanner} id="group-nest-error" error=${error} onDismiss=${() => setError('')} />
      <div class="modal-buttons">
        <button id="group-nest-cancel" type="button" disabled=${busy} onClick=${actions.close}><${Words} plain="Cancel" wizard="Dispel"/></button>
        <span class="spacer"></span>
        <button id="group-nest-submit" class="primary" type="button" disabled=${busy} onClick=${submit}><${Words} plain=${busy ? 'Saving…' : 'Save'} wizard=${busy ? 'Nesting…' : 'Nest party'}/></button>
      </div>
    </${Overlay}>
  `;
}

export function ActionDialogApp({ state, actions, confirmDiscard }) {
  const descriptor = state.view.value.dialog;
  if (!descriptor) return null;
  if (descriptor.kind === 'clone-agent') return html`<${CloneAgentDialog} key=${`clone:${descriptor.conv}`} descriptor=${descriptor} actions=${actions} confirmDiscard=${confirmDiscard} />`;
  if (descriptor.kind === 'reincarnate-agent') return html`<${ReincarnateAgentDialog} key=${`reincarnate:${descriptor.conv}`} descriptor=${descriptor} actions=${actions} confirmDiscard=${confirmDiscard} />`;
  if (descriptor.kind === 'nest-group') return html`<${NestGroupDialog} key=${`nest:${descriptor.group}`} descriptor=${descriptor} actions=${actions} confirmDiscard=${confirmDiscard} />`;
  return null;
}

export function mountActionDialogIsland({ host, state, actions, confirmDiscard, registerCleanup }) {
  render(html`<${ActionDialogApp} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} />`, host);
  const unregister = registerActionDialogController(actions);
  registerCleanup(() => { unregister(); render(null, host); });
}
