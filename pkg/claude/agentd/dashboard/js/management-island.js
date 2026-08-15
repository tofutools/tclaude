import { h, render } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { profileSummary, profileAliasesLabel } from './profiles.js';
import { roleSummary } from './roles.js';
import { AUTO_MEMORY_TRI_OPTIONS, CODEX_APP_SERVER_TRI_OPTIONS, COPILOT_API_TRI_OPTIONS, FAST_MODE_TRI_OPTIONS, dirtyDraft, harnessByName, harnessDefaults, profileDraft, profileHarnessDefaults, profilePayload, readTri, roleDraft, rolePayload, TRI_OPTIONS } from './management-model.js';
import { registerManagementController } from './management-controller.js';
import { grantListToOverrides, grantOverridesToList } from './permission-grant-list.js';
import {
  sandboxAccessAxes,
  sandboxAccessDraftErrors,
  sandboxConstructedRootWarning,
  sandboxNetworkAuthoring,
  sandboxOtherAssignmentWarnings,
  sandboxOtherContextRefusals,
  sandboxProfileSummary,
	sandboxResourceLimitErrors,
	sandboxResourceLimitsForWire,
  sandboxRuleBuckets,
  sandboxTargetLabel,
  sandboxTargetRefusal,
} from './sandbox-profiles-data.js';
import { CODEX_BUILTIN_FILTERED_NETWORK_DETAIL } from './sandbox-network-disclosure.js';
import { pickDirectory } from './helpers.js';
import { lineDiff } from './line-diff.js';
import { useDialogFocus } from './dialog-focus.js';
import { wizWord } from './slop.js';
import { ManagementOverlay as Overlay, useGuardedOverlayClose } from './management-overlay.js';
import { GroupCloneDialog, GroupContextDialog, GroupImportDialog, TemplateDeployDialog, TemplateDuplicateDialog, TemplateEditor, TemplateFromGroupDialog, TemplateImportDialog, TemplateManager, TemplateStartersDialog } from './template-management-island.js';
import { approvalPolicyLabel, approvalReviewerHelp, approvalReviewerOptions } from './approval-controls.js';
import { HelpDisclosure, HelpField } from './help-field.js';
import { SandboxImplHint } from './sandbox-impl-hint.js';
import {
  sandboxPreLaunchEditorRows,
  sandboxPreLaunchForWire,
  sandboxPreLaunchNewEditorRow,
  sandboxPreLaunchValidation,
} from './sandbox-pre-launch.js';
import {
  approvalControlsVisibleFor, autoCompactWindowHintFor, contextWindowMaxHintFor, harnessBuiltinModeHelpForImplementation,
  sandboxImplHintFor, sandboxImplCaveatFor, sandboxImplClearedNoticeFor, sandboxImplOptionsFor,
  harnessBuiltinModeControlLabel, harnessBuiltinModeOptionsForImplementation,
} from './agent-spawn-model.js';
import {
  RESOLVED_DEFAULTS_CHAIN, RESOLVED_DEFAULTS_CHAIN_PREVIEW, RESOLVED_DEFAULTS_LABEL,
  SANDBOX_PROFILE_COMPOSITION, SANDBOX_PROFILE_LAYERS_LABEL,
  harnessBuiltinModeDetail, harnessBuiltinModeOptionLabel, sandboxProfileLayersText,
} from './resolved-defaults.js';

// Mirrors the spawn dialog's copy: which layer owns the wall, the experimental
// framing, and the platform requirement stated rather than implied. A profile
// may pin the layer on a host that cannot run it — that is legitimate authoring
// — so the editor discloses instead of refusing.
const SANDBOX_IMPL_TITLE = 'Which layer owns OS-level containment for agents launched from this '
  + 'profile. harness-builtin is offered only when the selected harness owns a real OS sandbox. '
  + "tclaude's built-in OS sandbox is EXPERIMENTAL: it runs the "
  + "whole harness process inside a tclaude-owned bubblewrap namespace and turns the harness's own "
  + 'sandbox off inside it. Linux only, and it needs bwrap plus unprivileged user namespaces — a '
  + 'host without them refuses the launch instead of falling back. '
  + `Unset leaves the choice to the resolved defaults at spawn. ${RESOLVED_DEFAULTS_CHAIN}`;

// Shared with the spawn dialog's own copy of this control: the two consequences
// an operator has to know are the cap and the status-line decoupling.
const AUTO_COMPACT_WINDOW_TITLE = 'Context capacity in tokens for Claude Code\'s auto-compaction '
  + '(CLAUDE_CODE_AUTO_COMPACT_WINDOW). Accepts 450000, 450k or 0.5M; blank uses the model default. '
  + 'Pin it below a 1M model\'s real window so a long-lived agent compacts while it is still sharp. '
  + 'Capped at the model\'s actual context window.';
const CONTEXT_WINDOW_MAX_TITLE = 'Configured/assumed context cap for the Copilot context meter. '
  + 'Copilot does not report its context limit; a blank value uses the observed model\'s static assumption.';
const NETWORK_ACCESS_HELP = 'Choose Allow or Deny independently for built-in packs and manual '
  + 'destinations. Deny all starts closed; Allow rules release matching traffic and Deny rules can '
  + 'narrow those releases. Allow all starts open; Deny rules restrict matching traffic and Allow '
  + 'rules are redundant. Deny wins whenever traffic matches both an Allow and a Deny rule; rule '
  + 'order does not matter. Pack references are stored by stable ID and expand from the current '
  + 'tclaude release at policy resolution; their expanded rows are read-only in this editor. '
  + 'No override leaves the choice to other profile layers and carries no network rules. '
  + 'Host matches one exact DNS '
  + 'name. Domain matches the named domain and can optionally include its subdomains. CIDR matches '
  + 'an IP network. Loopback covers connections to the local machine. Ports are optional integer '
  + 'ports; blank allows all ports for that destination. The authored connection contract is '
  + 'ordinary IPv4/IPv6 TCP and UDP; QUIC is UDP. Raw and packet sockets are not an authored '
  + 'class. For Linux tclaude-layer filtered networking: '
  + 'Host and domain rules allow IP addresses returned by DNS. '
  + 'The sandbox can also reach other sites hosted on that same IP until the DNS '
  + 'answer expires. Only a new DNS lookup '
  + 'refreshes the allowed IP. Existing connections may continue after the DNS answer expires; '
  + 'new connections need another lookup. CIDR and local-machine deny rules are enforced directly. '
  + 'DNS-name denies are fully enforced under Deny all, but are partial under Allow all: tclaude '
  + 'blocks addresses observed through the sandbox DNS broker, while another address for the same '
  + 'service or encrypted DNS that bypasses the broker can remain reachable. A blocked shared '
  + 'address also affects other names until its DNS lease expires. At launch, bubblewrap, pasta, '
  + 'and nft must pass live '
  + 'checks. If any check fails, these rules are not enforced and outbound traffic is open. '
  + 'Applied global, group, and explicit list '
  + 'policies compose by intersection: destinations and ports must be allowed by every applicable '
  + 'list, and compatible destination selectors and port sets are intersected. The Effective '
  + 'policy preview reports enforcement capability limits for every destination on the selected '
  + 'implementation, harness, and platform. The Local access pack is intended for host-loopback model servers such as '
  + 'Ollama, LM Studio, or llama.cpp, Codex OSS mode, OpenCode local providers, and host-local '
  + 'development services. OpenCode local-provider launches are currently refused because those '
  + 'presets name no explicit provider endpoint to resolve. On Linux, '
  + 'local-machine rules use host.tclaude.internal. Inside the sandbox, 127.0.0.1 and ::1 refer '
  + 'to the sandbox itself. On macOS it is '
  + 'the real host loopback at 127.0.0.1 and ::1, so it also reopens host-local services including '
  + 'the IDE bridge. A cloud-backed harness resolves its provider endpoints at launch. Deny all '
  + 'refuses unless an Access list, directly or through Includes, covers them; Allow all with denies '
  + 'refuses when a Deny rule blocks one. No model endpoint is added implicitly. The Anthropic API '
  + 'and OpenAI API packs add the direct API-key endpoints separately. '
  + 'Linux enforces those lists. macOS does not yet enforce mixed destination lists '
  + 'and launches with the existing Not enforced disclosure and open outbound network. '
  + 'ChatGPT-auth Codex is refused in filtered mode; custom providers, web search, plugins, MCP '
  + 'servers, and agent commands need their own Access list destinations.'
  + ` ${CODEX_BUILTIN_FILTERED_NETWORK_DETAIL}`;
const NETWORK_PACKS_HELP = 'Release-owned destinations are stored as stable Allow or Deny pack '
  + 'references and expand from the current tclaude release. Choose Off to remove a pack; '
  + 'future endpoint updates follow the stored pack reference. Deny enforcement depends on the '
  + 'selected launch target and is shown in the Effective policy preview. Expanded destinations '
  + 'below are read-only.';
const NETWORK_DESTINATIONS_HELP = 'The Effective policy preview below evaluates composed launch '
  + 'behavior after Includes and assignment intersections. Its rule buckets show whether the '
  + 'selected target fully supports, partially supports, or does not apply each deny destination.';
const UNIX_SOCKETS_HELP = 'Unix-socket policy composes by intersection across profile layers. '
  + 'The tclaude agentd socket is always reachable and is not an editable row.';
const FILESYSTEM_HELP = 'Directory grants widen the sandbox. Included and assignment-layer rules '
  + 'can further constrain the effective launch policy; inherited global rows shown here are '
  + 'read-only context and are never copied into this profile.';
const ENVIRONMENT_HELP = 'Environment values are injected when the agent launches. Values are '
  + 'ordinary non-secret configuration; do not store credentials here.';
const INCLUDES_HELP = 'Included profiles apply first, in order; this profile overrides their '
  + 'exact-same-path or same-variable values.';
const AGENT_DIRECTORIES_HELP = 'Environment-variable names backed by fresh isolated writable '
  + 'directories created for each spawned agent.';
const PRE_LAUNCH_HELP = 'Named bash blocks for setup that declarative environment or directory '
  + 'fields cannot express. They run inside the sandbox after the profile environment and before '
  + 'the harness starts. Declared exports are checked after each block and make its contract visible.';
const EFFECTIVE_POLICY_HELP = 'Evaluates the composed policy for the selected implementation, '
  + 'harness, platform, and assignment context. This preview reports enforcement capability '
  + 'limits without changing the authored profile. '
  + `${SANDBOX_PROFILE_COMPOSITION} The launch target those layers are evaluated against is a `
  + `separate question, answered by the target controls. ${RESOLVED_DEFAULTS_CHAIN} `
  + RESOLVED_DEFAULTS_CHAIN_PREVIEW;

// The three target selectors share one native tooltip. It goes on the controls
// rather than into a paragraph beneath them: this section already has a [?]
// disclosure carrying the full explanation, and a permanent block of prose
// between a control and its result is what help-field.js exists to avoid.
const EVALUATION_TARGET_TITLE = `${RESOLVED_DEFAULTS_LABEL} evaluate the launch a real spawn `
  + `would resolve; override any control to try another target. ${RESOLVED_DEFAULTS_CHAIN_PREVIEW}`;

const html = htm.bind(h);

function message(error) { return error?.message || String(error); }
function clone(value) { return JSON.parse(JSON.stringify(value)); }
function change(setDraft, key, value) { setDraft((draft) => ({ ...draft, [key]: value })); }

function SegmentedControl({ label, value, onChange, options, disabled = false, className = '' }) {
  const activate = (event, index) => {
    if (disabled) return;
    let nextIndex = index;
    switch (event.key) {
      case 'ArrowLeft':
      case 'ArrowUp':
        nextIndex = (index - 1 + options.length) % options.length;
        break;
      case 'ArrowRight':
      case 'ArrowDown':
        nextIndex = (index + 1) % options.length;
        break;
      case 'Home':
        nextIndex = 0;
        break;
      case 'End':
        nextIndex = options.length - 1;
        break;
      default:
        return;
    }
    event.preventDefault();
    event.currentTarget.parentElement?.children[nextIndex]?.focus();
    onChange(options[nextIndex][0]);
  };
  return html`<div class=${`sbx-segmented-control${className ? ` ${className}` : ''}`}
      role="radiogroup" aria-label=${label} aria-disabled=${disabled || null}>
    ${options.map(([key, text], index) => {
      const selected = key === value;
      return html`<button key=${key} type="button" role="radio" aria-checked=${selected}
        data-value=${key} disabled=${disabled} tabindex=${selected ? '0' : '-1'}
        class=${`sbx-segmented-option sbx-state-${key}${selected ? ' is-selected' : ''}`}
        onClick=${() => onChange(key)} onKeyDown=${(event) => activate(event, index)}>${text}</button>`;
    })}
  </div>`;
}

function SandboxHelp({ children }) {
  return html`<span class="sbx-section-help" onClick=${(event) => event.stopPropagation()}>${children}</span>`;
}

function SandboxSection({ id, label, help = '', helpID = `${id}-help`, hidden = false, className = '', attention = false, entryCount = null, children }) {
  const [helpOpen, setHelpOpen] = useState('');
  const sectionRef = useRef(null);
  const hadAttention = useRef(false);
  useEffect(() => {
    if (attention && !hadAttention.current && sectionRef.current) sectionRef.current.open = true;
    hadAttention.current = !!attention;
  }, [attention]);
  return html`<details ref=${sectionRef} id=${id} class=${`sbx-section${className ? ` ${className}` : ''}`} hidden=${hidden}>
    <summary class="sbx-section-summary sbx-section-legend"><span>${label}</span>
      ${help && html`<${SandboxHelp}><${HelpDisclosure} id=${helpID} label=${label} help=${help}
        open=${helpOpen === helpID} setOpen=${setHelpOpen}/></${SandboxHelp}>`}
      ${entryCount !== null && html`<span class=${`sbx-section-count${entryCount === 0 ? ' sbx-section-count-empty' : ''}`}>${entryCount} ${entryCount === 1 ? 'entry' : 'entries'}</span>`}
    </summary>
    <div class="sbx-section-body">${children}</div>
  </details>`;
}

const SANDBOX_EVALUATION_FALLBACK_HARNESSES = [
  { name: 'claude', display_name: 'Claude Code', can_builtin_os_sandbox: true, can_tclaude_layer: true, can_stacked: true },
  { name: 'codex', display_name: 'Codex', can_builtin_os_sandbox: true, can_tclaude_layer: true, can_stacked: true },
  // OpenCode's access-control mode is a command filter, not an OS sandbox.
  // Its agentd-owned server boundary is the one supported evaluation target.
  { name: 'opencode', display_name: 'OpenCode', can_builtin_os_sandbox: false, can_tclaude_layer: true, can_stacked: false },
];

function sandboxEvaluationHarnesses(catalog) {
  const candidates = Array.isArray(catalog) && catalog.length
    ? catalog
    : SANDBOX_EVALUATION_FALLBACK_HARNESSES;
  return candidates.filter((entry) =>
    entry.can_builtin_os_sandbox || entry.can_tclaude_layer || entry.can_stacked);
}

function sandboxEvaluationImplementations(harness, platform, catalog) {
  const entry = sandboxEvaluationHarnesses(catalog)
    .find((candidate) => candidate.name === harness);
  if (!entry) return [];
  const options = [];
  if (entry.can_builtin_os_sandbox) {
    // Names the implementation, nothing else — Codex's missing filtered-network
    // sandbox is stated on the evaluation RESULT (sandboxTargetLabel and the
    // rule notices), which is where it changes what an operator should do.
    options.push([
      'harness-builtin',
      harness === 'codex' ? 'Codex built-in sandbox' : 'Harness built-in sandbox',
    ]);
  }
  if (entry.can_tclaude_layer) options.push(['tclaude-layer', 'tclaude sandbox']);
  if (entry.can_stacked && platform === 'linux') options.push(['stacked', 'Stacked sandboxes']);
  return options;
}

function sandboxEvaluationTarget(harness, implementation, platform) {
  if (!harness) return null;
  const target = { implementation, harness, platform };
  if (harness === 'opencode') target.sandbox = 'tclaude-layer';
  return target;
}

function sandboxRuleOutcomeHelp(item, targetLabel) {
  /* 'not_evaluated' is listed FIRST and explicitly, because the fallback arm of
     this chain is 'Not applied' — a verdict. Without this branch every rule in
     the TCL-915 not-evaluated bucket would disclose "Not applied on <target>",
     asserting per rule exactly what the bucket around it exists to deny. The
     per-rule help is a diagnostic, and a diagnostic that reports the defect
     class it was written for is the sharpest version of that mistake. */
  const status = item.outcome === 'not_evaluated' ? 'Not evaluated'
    : item.outcome === 'enforced' ? 'Enforced'
      : item.outcome === 'enforced_partial' ? 'Partial'
        : item.outcome === 'refused' ? 'Launch blocked' : 'Not applied';
  const heading = item.outcome === 'not_evaluated'
    ? `${status} on ${targetLabel}. This rule was never judged: the target was refused first.`
    : `${status} on ${targetLabel}.`;
  return {
    text: `${heading}${item.detail ? ` ${item.detail}` : ''}`,
    content: html`<span><strong>${heading}</strong>${item.detail ? ` ${item.detail}` : ''}</span>`,
  };
}

function SandboxOutcomeBucket({
  bucket, open, helpOpen, setHelpOpen, helpPrefix, targetLabel,
}) {
  return html`<details class=${`sbx-rule-bucket sbx-rule-bucket-${bucket.key}`} open=${open}>
    <summary><span>${bucket.label}</span><span class="sbx-rule-count">${bucket.rules.length}</span></summary>
    ${/* Before the rules, not after: the note says what the list below it IS,
          and a disclaimer under a list has already been read past. */ ''}
    ${bucket.note && html`<div class="sbx-bucket-note">${bucket.note}</div>`}
    ${bucket.items.length > 0 && html`<ul>${bucket.items.map((item, index) => {
    const helpID = `${helpPrefix}-${bucket.key}-${index}`;
    const help = sandboxRuleOutcomeHelp(item, targetLabel);
    return html`<li key=${index} class="sbx-rule-row"><span>${item.label}</span>
      <span class="sbx-section-help sbx-rule-help"><${HelpDisclosure}
        id=${helpID} label=${item.label} help=${help.text} content=${help.content}
        open=${helpOpen === helpID} setOpen=${setHelpOpen}/></span>
    </li>`;
  })}</ul>`}
    ${bucket.reasons.map((reason, index) => html`<div key=${index} class="sbx-rule-reason"><strong>${reason.label}:</strong> ${reason.detail}</div>`)}
  </details>`;
}

/* The label the context selector gives an assignment, so a warning about
   another assignment names something the operator can actually locate in the
   dropdown rather than an opaque ordinal.

   It mirrors the selector's option text but does NOT share its discriminator:
   the selector distinguishes the global assignment by `global === draft.name`,
   this by the presence of `explicit`. They agree for every role shape that
   produces more than one context, and this helper only ever runs when there is
   another assignment to name — the one divergent shape (an unsaved draft named
   identically to the existing global profile) arises solely in the
   single-context fallback. Stated rather than claimed "kept in step", so the
   next person to touch the selector knows the two are not literally coupled.

   This is the vocabulary itself, taking the context VALUE rather than a list
   and an index. An assignment past the display cap has no index to be looked
   up by — the daemon sends its context beside the refusal (TCL-913) — so the
   naming has to be reachable without one. Returns null for a missing value so
   the ordinal fallback stays with the index-based caller below, where there is
   an N to show; an omitted assignment has none. */
function sandboxContextLabelFor(value) {
  if (!value) return null;
  if (value.group_name) return `group ${value.group_name}`;
  if (value.explicit) return 'explicit selection';
  return 'global assignment';
}

/* The index-based caller: names a LISTED assignment, and falls back to the
   ordinal when the index has no context behind it. */
function sandboxContextLabel(contexts, index) {
  return sandboxContextLabelFor(contexts?.[index]?.context) ?? `assignment ${index + 1}`;
}

/* TCL-914. The network entries for ONE assignment context — the single
   definition shared by both readers of that value. SandboxPolicyResult decides
   what to RENDER and sandboxPolicyNeedsAttention decides whether to RAISE
   ATTENTION; those are the same question about the same context, so they get one
   predicate. They previously each spelled the expression out, and nothing kept
   the two copies in step.

   The fallback keys on whether the daemon sent the LIST AT ALL, never on whether
   one index holds entries. Those are different questions and only the first has
   a fallback answer:

   - No `context_network_entries` at all. THREE shapes produce this, and the
     fallback is right for all three: an OLD daemon that never computed
     per-context entries; a CURRENT daemon with no effective assignment contexts
     to compute them for (describePredictedDraftSandboxProfile returns nil for
     every per-context slice when len(contexts) == 0); and a target REFUSED
     OUTRIGHT, whose draft-enforcement append carries only Target/ResolvedBy/
     Predicted/Refusal and so omits the network entries as well as the axes. The
     second is the same shape `target.context_axes?.[i] || target.axes` already
     falls back for, one line above. In the third, `network_entries` is nil too,
     so the fallback yields [] — and consumers must branch on the refusal before
     any of this matters, which SandboxPolicyResult does below.
   - The list exists — it is AUTHORITATIVE for every index in it, INCLUDING a
     null one. The daemon writes an explicit null at a refused index to stay
     index-aligned with context_axes, and that null means "this context produced
     no entries". That is a VERDICT, not a gap: filling it from
     `target.network_entries` would attribute the DRAFT-ONLY prediction — a
     different policy, which no launch uses — to this context.

   An index NOT in the list returns [] as well, where the old expression fell
   back. Said plainly because it is a behaviour change the paragraph above does
   not cover: "authoritative for every index IN it" is silent about an index past
   the end. It is unreachable — context_network_entries, context_axes and
   context_refusals are built in one loop and truncated under the same
   len(response.Contexts) bound in the draft-enforcement handler, so they are
   equal-length, and both call sites are gated on
   prediction.contexts[effectiveContext] existing — and the strict answer is the
   right one anyway: an index the daemon never described has no verdict, and the
   draft-only rows are not it.

   Null at an index is EXCLUSIVELY the refusal marker, which is what lets this
   treat it as a verdict rather than as missing data. Verified against the only
   producer, describePredictedDraftSandboxProfile's per-context loop: the success
   path appends `append([]harness.PredictedNetworkEntry{}, ...)`, non-nil even
   when empty, so a context with genuinely zero network rows serializes as `[]`
   and never as null; the sole nil-appending path is the typed-capability
   refusal, which appends a non-nil refusal at the SAME index in the same block.
   A null here therefore always has a refusal beside it — it is never an error
   swallowed (derivation and untyped prediction errors fail the whole request),
   an early return, or a display cap (the cap drops whole trailing indexes and
   never nulls one in range).

   Cited by symbol rather than by line on purpose: an earlier draft of this
   comment carried line numbers that its own follow-up commit invalidated by
   adding lines above the code they pointed at.

   `??` cannot express that, because null is nullish and so takes the fallback in
   exactly the case the fallback is wrong for. Neither can a per-index
   `Array.isArray` check that falls back on a miss: handed an explicit null it
   returns `target.network_entries`, which is what `??` already did. */
function sandboxContextNetworkEntries(target, contextIndex) {
  const perContext = target?.context_network_entries;
  if (!Array.isArray(perContext)) return target?.network_entries ?? [];
  const entries = perContext[contextIndex];
  return Array.isArray(entries) ? entries : [];
}

export function SandboxPolicyResult({ target, context, contextIndex, contexts = [] }) {
  const [ruleHelpOpen, setRuleHelpOpen] = useState('');
  const axes = target.context_axes?.[contextIndex] || target.axes || {};
  const networkEntries = sandboxContextNetworkEntries(target, contextIndex);
  /* TCL-885. A refused target carries NO axes, so it must be read here before
     anything touches `axes` — sandboxRuleBuckets substitutes
     {outcome:'not_enforced', detail:'No enforcement verdict was returned.'} for
     a missing axis, and a refusal that fell into that path would render as
     "unsupported, no verdict returned" with launchRefused FALSE: an operator
     told nothing is wrong while the launch is blocked.

     This branch is a deliberate MINIMUM, not the ticket's row type. The row-type
     design is the operator's open decision; this reuses the existing
     .sbx-launch-blocked element so the preview cannot regress below today's
     honest refusal in the meantime. Whatever the operator picks refines what
     this renders, not whether it exists — so keep it structureless: capability
     text and remedy, no buckets, no grouping, no vocabulary of its own. */
  const refusal = sandboxTargetRefusal(target, contextIndex);
  const buckets = sandboxRuleBuckets(axes, context, networkEntries, refusal);
  /* A refusal in an assignment context the operator is NOT looking at, plus any
     from contexts past the display cap. The pre-existing axis-based check cannot
     find these: a refused context contributes nothing to the aggregate axes by
     design, so there is no verdict for it to compare. Without this the editor
     would render a clean preview for the selected context and say nothing at all
     about an assignment whose launch is blocked. */
  const otherRefusals = sandboxOtherContextRefusals(target, contextIndex);
  const otherWarnings = [
    ...sandboxOtherAssignmentWarnings(target.axes, axes),
    // A distinct axis per entry: it is the list key, and several omitted
    // refusals would otherwise share one.
    ...otherRefusals.map(({ index, refusal, context: assignment }, position) => ({
      axis: index === null ? `refusal-omitted-${position}` : `refusal-${index}`,
      /* An omitted assignment has no index to name it by, so the daemon sends
         its context beside the refusal (TCL-913) and it is named with the SAME
         vocabulary as a listed one. The unnamed wording is the compat path, not
         a default: it is what a response carrying no identity degrades to —
         either from a daemon predating the field, or from one that could not
         resolve the assignment. Both are ABSENT rather than empty, so this
         branch is decidable; an empty identity would be named "global
         assignment" by the helper, which is a confident wrong answer. */
      label: assignment
        ? sandboxContextLabelFor(assignment)
        : index === null
          ? 'An assignment omitted from this selector'
          : sandboxContextLabel(contexts, index),
      outcome: 'refused',
      detail: refusal.message,
    })),
  ];
  const otherLaunchRefused = otherWarnings.some((warning) => warning.outcome === 'refused');
  const partialCount = buckets.partial.rules.length;
  const unsupportedCount = buckets.notApplied.rules.length;
  const a11ySummary = `${partialCount} partially supported ${partialCount === 1 ? 'rule' : 'rules'} and ${unsupportedCount} unsupported ${unsupportedCount === 1 ? 'rule' : 'rules'}.`;
  const targetLabel = sandboxTargetLabel(target);
  const helpPrefix = `sandbox-rule-${contextIndex}-${[
    target.target?.implementation, target.target?.harness, target.target?.platform,
  ].filter(Boolean).join('-').replace(/[^a-zA-Z0-9_-]+/g, '-')}`;
  return html`<div class="sbx-policy-result">
    <strong class="sbx-policy-target">${targetLabel}</strong>
    ${!buckets.launchRefused && html`<div class="sbx-a11y-status" role="status" aria-live="polite" aria-atomic="true">${a11ySummary}</div>`}
    ${otherWarnings.length > 0 && html`<div class=${`sbx-other-assignments${otherLaunchRefused ? ' refused' : ''}`} role=${otherLaunchRefused ? 'alert' : 'status'}>
      <strong>Other assignments need attention.</strong>
      <div>The overall safety check includes every assignment, including any omitted from the selector.</div>
      <ul>${otherWarnings.map((warning) => html`<li key=${warning.axis}><strong>${warning.label}:</strong> ${warning.detail}</li>`)}</ul>
    </div>`}
    ${/* TCL-915. The target-level refusal banner. Same element and same red as
          the minimum that shipped in TCL-885 — this promotes it to a headline
          plus the capability kind plus the evaluator's own text, it does not
          introduce a second red.

          The detail is the wire's `message` VERBATIM. It is a single fused
          sentence whose remedies are its trailing clause, and there is no
          structured remedies field; splitting it client-side would be a guess
          dressed as structure, and a mis-split silently DROPS a remedy — this
          ticket's own failure mode committed in its own rendering. Ruled by the
          operator's lead. Discrete remedy bullets need a wire change and a
          separate ticket. */ ''}
    ${buckets.launchRefused && html`<div class="sbx-launch-blocked" role="alert">${refusal
    ? html`<strong>This target cannot enforce this policy, so the launch is refused.</strong>
      ${/* The kind is a raw machine token. Sighted readers get "this is an
            identifier" from the monospace chip; that cue does not survive into
            speech, where it would run straight on from the sentence above as
            though it were prose. The visible prefix says what it is on BOTH
            channels rather than hiding the answer in an aria-label only one of
            them reads. */ ''}
      <span class="sbx-refusal-kind"><span class="sbx-refusal-kind-label">Capability: </span>${refusal.kind}</span>
      <div class="sbx-refusal-detail">${refusal.message}</div>`
    : 'This target refuses the launch. Unsupported rules are not silently skipped.'}</div>`}
    ${/* Listed, never judged. Ships COLLAPSED: these rules carry no verdict, so
          they are reference material rather than something needing attention —
          the banner above is what needs attention. This is a pure ADDITION; the
          `!refusal` guard below is untouched, so the three verdict buckets stay
          suppressed exactly as before.

          Suppressed entirely when there is nothing to list. The note reads
          "These are the rules this profile would apply", which is FALSE over an
          empty policy, and a bucket headed "Rules not evaluated  0" invites
          reading the zero as a finding. Nothing to list means nothing to say;
          the banner already carries the whole story. */ ''}
    ${refusal && buckets.unjudged.items.length > 0 && html`<${SandboxOutcomeBucket}
      bucket=${buckets.unjudged} open=${false}
      helpOpen=${ruleHelpOpen} setHelpOpen=${setRuleHelpOpen}
      helpPrefix=${helpPrefix} targetLabel=${targetLabel}/>`}
    ${/* The Applied bucket ships closed — a fully supported policy needs no
         attention. A remapped rule is the exception: the editor row shows only a
         glyph, so this line is where the host → sandbox mapping is actually
         legible, and a mapping two collapses deep is not a disclosure. Only
         profiles that use a mount path are affected. */ ''}
    ${!refusal && html`<${SandboxOutcomeBucket} bucket=${buckets.applied} open=${buckets.applied.hasMountPath === true}
      helpOpen=${ruleHelpOpen} setHelpOpen=${setRuleHelpOpen}
      helpPrefix=${helpPrefix} targetLabel=${targetLabel}/>
    <${SandboxOutcomeBucket} bucket=${buckets.partial} open=${true}
      helpOpen=${ruleHelpOpen} setHelpOpen=${setRuleHelpOpen}
      helpPrefix=${helpPrefix} targetLabel=${targetLabel}/>
    <${SandboxOutcomeBucket} bucket=${buckets.notApplied} open=${true}
      helpOpen=${ruleHelpOpen} setHelpOpen=${setRuleHelpOpen}
      helpPrefix=${helpPrefix} targetLabel=${targetLabel}/>`}
    ${(context.resource_limits?.memory || context.resource_limits?.cpu != null) && html`<div class="sbx-resource-evaluation">
      <strong>Resource limits — Linux only</strong>
      ${context.resource_limits.memory && html`<div>Memory: ${context.resource_limits.memory} → ${context.memory_limit_bytes} bytes (<code>memory.max</code>)</div>`}
      ${context.resource_limits.cpu != null && html`<div>CPU: ${context.resource_limits.cpu} cores → <code>cpu.max ${context.cpu_max}</code></div>`}
      <div>${refusal?.kind === 'unsupported_resource_limits' ? 'This target cannot enforce these limits.' : 'This Linux target can enforce the requested cgroup-v2 limits; live controller delegation is checked again before launch.'}</div>
    </div>`}
    ${context.darwin_allow_mach_register && html`<div class="sbx-mach-register-evaluation">
      <strong>Mach service registration — macOS only</strong>
      <div>${target.target?.platform === 'darwin' && target.target?.implementation === 'tclaude-layer'
    ? 'Allowed by the tclaude Seatbelt layer for this target.'
    : 'Stored in this effective policy, but it does not apply to this target; only the macOS tclaude-layer sandbox consumes it.'}</div>
    </div>`}
    <details class="sbx-target-details"><summary>Evaluation details</summary>
      ${target.target.sandbox ? html`<div>Sandbox mode: ${harnessBuiltinModeDetail(target.target.harness, target.target.sandbox)}</div>` : null}
      ${target.resolved_by
    ? html`<div>${RESOLVED_DEFAULTS_LABEL} came from: ${target.resolved_by}</div>`
    : html`<div>Launch target overridden here; ${RESOLVED_DEFAULTS_LABEL.toLowerCase()} were not used.</div>`}
    </details>
  </div>`;
}

export function sandboxPolicyNeedsAttention(target, context, contextIndex) {
  const axes = target.context_axes?.[contextIndex] || target.axes || {};
  const networkEntries = sandboxContextNetworkEntries(target, contextIndex);
  return sandboxRuleBuckets(
    axes, context, networkEntries, sandboxTargetRefusal(target, contextIndex),
  ).launchRefused
    || sandboxOtherAssignmentWarnings(target.axes, axes).length > 0
    || sandboxOtherContextRefusals(target, contextIndex).length > 0;
}

function accessRowShapeError(network, unixSockets) {
  const isRow = (row) => !!row && typeof row === 'object' && !Array.isArray(row);
  const networkIndex = (network.allow || []).findIndex((row) => !isRow(row));
  if (networkIndex >= 0) return `Network row ${networkIndex + 1} must be a JSON object containing a host, domain, CIDR, or loopback selector.`;
  const networkDenyIndex = (network.deny || []).findIndex((row) => !isRow(row));
  if (networkDenyIndex >= 0) return `Network deny row ${networkDenyIndex + 1} must be a JSON object containing a host, domain, CIDR, or loopback selector.`;
  const socketIndex = (unixSockets.allow || []).findIndex((row) => !isRow(row));
  return socketIndex >= 0
    ? `Unix-socket row ${socketIndex + 1} must be a JSON object containing a path or path_glob selector.`
    : '';
}

/* One entry of the common-rule preset menu: a button that inserts the entry's
   audited paths as ordinary deny rows, with its rationale, warning and the
   exact paths it would insert visible before the click. Nothing about the
   entry is stored — after insertion the rows are plain, editable table rows. */
function CommonRuleEntry({ entry, onAdd, variant = 'filesystem' }) {
  const paths = commonRulePaths(entry);
  // The rationale, the warning and the exact paths are what make the button
  // safe to press, so they are announced with it rather than left as nearby
  // text a screen-reader or keyboard operator can tab straight past.
  const base = `sbx-common-rule-${String(entry.id || '').replace(/[^a-zA-Z0-9_-]+/g, '-')}`;
  const descrID = `${base}-descr`; const warnID = `${base}-warn`; const pathsID = `${base}-paths`;
  const describedBy = [descrID, entry.warning ? warnID : '', pathsID].filter(Boolean).join(' ');
  // An entry with no paths on this platform has nothing to insert, but
  // `disabled` would take it out of the tab order and take its description —
  // which is precisely the explanation of WHY it does nothing — with it.
  // aria-disabled keeps both reachable; the handler refuses instead of the DOM.
  const noPaths = variant === 'filesystem' && !paths.length;
  return html`<div class=${variant === 'filesystem' ? 'sbx-common-rule-entry' : 'sbx-access-template-entry'} data-rule=${entry.id}>
    <button type="button" class=${variant === 'filesystem' ? 'sbx-common-rule-add' : 'sbx-access-template-add'} aria-describedby=${describedBy} aria-disabled=${noPaths ? 'true' : null} onClick=${() => { if (!noPaths) onAdd(entry); }}>＋ ${entry.label || entry.id}</button>
    <span class="sbx-common-rule-descr" id=${descrID}>${entry.description || ''}</span>
    ${entry.warning ? html`<span class="sbx-common-rule-warn" id=${warnID}>⚠ ${entry.warning}</span>` : null}
    <code class="sbx-common-rule-paths" id=${pathsID}>${paths.length ? paths.join(' · ') : '(no audited paths on this platform)'}</code>
  </div>`;
}

const ACCESS_MODE_OPTIONS = [
  ['', 'No override'],
  ['open', 'Full access'],
  ['closed', 'No access'],
  ['list', 'Access list'],
];

const NETWORK_BASELINE_OPTIONS = [
  ['deny', 'Deny all'],
  ['allow', 'Allow all'],
  ['inherit', 'No override'],
];

// Engine names HOW a discriminating rule set is enforced, never WHAT it
// authorizes, so it sits beside the baseline rather than inside the rule table.
// "No override" is the fourth state and the default: it inherits the next
// composition layer, and when no layer names one it is today's behavior for the
// platform.
const NETWORK_ENGINE_OPTIONS = [
  ['', 'No override'],
  ['packet', 'Packet filter'],
  ['proxy', 'Proxy filter'],
];

const NETWORK_NAMESPACE_OPTIONS = [
  ['', 'No override'],
  ['host', 'Shared host'],
  ['private', 'Private, routed'],
];

const DEFAULT_NETWORK_PACKS = ['net-local', 'net-anthropic', 'net-openai-codex'];

function networkEntriesMayOverlap(left = {}, right = {}) {
  const leftPorts = new Set(Array.isArray(left.ports) ? left.ports.map(Number) : []);
  const rightPorts = new Set(Array.isArray(right.ports) ? right.ports.map(Number) : []);
  if (leftPorts.size && rightPorts.size &&
      ![...leftPorts].some((port) => rightPorts.has(port))) return false;

  if (left.loopback || right.loopback) {
    if (left.loopback && right.loopback) return true;
    // Valid authored CIDRs never cover loopback. DNS selectors can resolve to
    // loopback, so keep those conservatively possibly overlapping.
    return !(left.cidr || right.cidr);
  }
  if (left.cidr || right.cidr) {
    // CIDR/CIDR and DNS/CIDR overlap needs address resolution or IP parsing.
    // Uncertainty must suppress a "redundant" claim, never invent one.
    return true;
  }

  // Different DNS names can resolve to the same address. Without resolution
  // data they remain possibly overlapping even when neither name covers the
  // other syntactically.
  return true;
}

function NetworkAccessEditor({ draft, setDraft, catalog, newDraft, packVisibilityError, packVisibilityAttention, retryPackCatalog, packCatalogBusy }) {
  const [helpOpen, setHelpOpen] = useState('');
  const [packsOpen, setPacksOpen] = useState(false);
  const manualRowKeys = useRef({ allow: [], deny: [] });
  const nextManualRowKey = useRef(0);
  const hadPackAttention = useRef(false);
  useEffect(() => {
    if (packVisibilityAttention && !hadPackAttention.current) setPacksOpen(true);
    hadPackAttention.current = !!packVisibilityAttention;
  }, [packVisibilityAttention]);
  const defaultsAvailable = useRef(!!newDraft);
  const rules = sandboxNetworkAuthoring(draft);
  const editable = rules.baseline !== 'inherit';
  for (const mode of ['allow', 'deny']) {
    manualRowKeys.current[mode].length = Math.min(manualRowKeys.current[mode].length, rules[mode].length);
    while (manualRowKeys.current[mode].length < rules[mode].length) {
      manualRowKeys.current[mode].push(`network-manual-row-${nextManualRowKey.current++}`);
    }
  }
  const update = (patch) => setDraft((value) => ({ ...value, network: { ...value.network, ...patch } }));
  const rowsFor = (mode) => mode === 'deny' ? rules.deny : rules.allow;
  const updateRows = (mode, rows) => update({ [mode]: rows });
  const updateRow = (mode, index, patch) => updateRows(mode,
    rowsFor(mode).map((row, i) => i === index ? { ...row, ...patch } : row));
  // Prefer the selector that survives wire normalization, then preserve an
  // intentionally empty domain/CIDR key while its value is being authored.
  const selector = (row) => row.domain ? 'domain'
    : row.cidr ? 'cidr'
      : row.loopback === true ? 'loopback'
        : row.host ? 'host'
          : Object.hasOwn(row, 'domain') ? 'domain'
            : Object.hasOwn(row, 'cidr') ? 'cidr'
              : 'host';
  const changeSelector = (mode, index, kind) => {
    const current = rowsFor(mode)[index];
    const next = kind === 'loopback' ? { loopback: true, ports: current.ports || [] } : { [kind]: '', ports: current.ports || [] };
    updateRows(mode, rowsFor(mode).map((row, i) => i === index ? next : row));
  };
  const changeRowMode = (mode, index, nextMode) => {
    if (mode === nextMode) return;
    const row = rowsFor(mode)[index];
    const [rowKey] = manualRowKeys.current[mode].splice(index, 1);
    manualRowKeys.current[nextMode].push(rowKey);
    update({
      [mode]: rowsFor(mode).filter((_, i) => i !== index),
      [nextMode]: [...rowsFor(nextMode), row],
    });
  };
  // Rules are retained session-locally across No override flips so an
  // accidental select change is recoverable; the stored draft still carries
  // no pack or destination rows while that baseline is active.
  const retainedRules = useRef(null);
  if (rules.baseline !== 'inherit') {
    retainedRules.current = {
      packs: rules.packs, deny_packs: rules.deny_packs,
      allow: rules.allow, deny: rules.deny,
    };
  }
  const changeBaseline = (baseline) => {
    setDraft((value) => {
      const current = sandboxNetworkAuthoring(value);
      let next = {
        packs: current.packs, deny_packs: current.deny_packs,
        allow: current.allow, deny: current.deny,
      };
      if (baseline === 'inherit') {
        next = { packs: [], deny_packs: [], allow: [], deny: [] };
      } else if (current.baseline === 'inherit') {
        next = retainedRules.current || { packs: [], deny_packs: [], allow: [], deny: [] };
        if (baseline === 'deny' && defaultsAvailable.current &&
            !next.packs.length && !next.deny_packs.length && !next.allow.length && !next.deny.length) {
          next = { ...next, packs: DEFAULT_NETWORK_PACKS };
          defaultsAvailable.current = false;
        }
      }
      // The engine survives a baseline change: it is not one of the rules the
      // baseline governs, and silently clearing it would change the mechanism
      // as a side effect of an unrelated edit.
      return { ...value, network: {
        baseline, ...next,
        ...(current.engine ? { engine: current.engine } : {}),
        ...(current.namespace ? { namespace: current.namespace } : {}),
      } };
    });
  };
  const packMode = (id) => rules.packs.includes(id) ? 'allow'
    : rules.deny_packs.includes(id) ? 'deny' : 'off';
  const changePackMode = (id, mode) => update({
    packs: mode === 'allow'
      ? [...new Set([...rules.packs, id])]
      : rules.packs.filter((value) => value !== id),
    deny_packs: mode === 'deny'
      ? [...new Set([...rules.deny_packs, id])]
      : rules.deny_packs.filter((value) => value !== id),
  });
  const packRows = (catalog.network_packs || []).flatMap((pack) =>
    packMode(pack.id) !== 'off'
      ? (pack.entries || []).map((row) => ({ row, pack, mode: packMode(pack.id) }))
      : []);
  const manualRows = [
    ...rules.allow.map((row, index) => ({ row, index, mode: 'allow' })),
    ...rules.deny.map((row, index) => ({ row, index, mode: 'deny' })),
  ];
  const hasDenyRules = rules.deny.length > 0 || rules.deny_packs.length > 0;
  const allowEntries = [
    ...rules.allow,
    ...(catalog.network_packs || []).flatMap((pack) =>
      packMode(pack.id) === 'allow' ? pack.entries || [] : []),
  ];
  const knownPackIDs = new Set((catalog.network_packs || []).map((pack) => pack.id));
  const unresolvedAllowPack = rules.packs.some((id) => !knownPackIDs.has(id));
  const redundantLabel = (mode, entries) => {
    if (rules.baseline === 'allow' && mode === 'allow') return 'Redundant under Allow all';
    if (rules.baseline !== 'deny' || mode !== 'deny') return '';
    if (unresolvedAllowPack) return '';
    return (entries || []).every((denyEntry) =>
      !allowEntries.some((allowEntry) => networkEntriesMayOverlap(allowEntry, denyEntry)))
      ? 'Redundant under Deny all' : '';
  };
  return html`<${SandboxSection} id="sandbox-profile-editor-network-section" className="sbx-access-axis"
      label="Network" help=${NETWORK_ACCESS_HELP} helpID="sandbox-profile-editor-network-help"
      attention=${packVisibilityAttention}
      entryCount=${rules.packs.length + rules.deny_packs.length + manualRows.length}>
    <label class="sbx-network-baseline-label">Baseline <${Select} id="sandbox-profile-editor-network-baseline" value=${rules.baseline} onChange=${changeBaseline} options=${NETWORK_BASELINE_OPTIONS}/></label>
    <label class="sbx-network-baseline-label">Filtering engine <${Select} id="sandbox-profile-editor-network-engine" value=${rules.engine || ''} onChange=${(engine) => update({ engine })} options=${NETWORK_ENGINE_OPTIONS}/></label>
    <label class="sbx-network-baseline-label">Network namespace <${Select} id="sandbox-profile-editor-network-namespace" value=${rules.namespace || ''} onChange=${(namespace) => update({ namespace })} options=${NETWORK_NAMESPACE_OPTIONS}/></label>
    ${rules.namespace === 'private' && html`<p class="sbx-inline-note">Linux tclaude-layer only. Internet traffic is routed normally, but host localhost services, IDE bridges, and abstract Unix sockets are not shared.</p>`}
    ${packVisibilityError && html`<div class="sbx-network-pack-visibility-error" role="alert"><span>⚠ ${packVisibilityError}</span>
      <button type="button" onClick=${retryPackCatalog}>${packCatalogBusy ? 'retry loading' : 'retry catalog'}</button></div>`}
    <fieldset class=${`sbx-network-unlocks${editable ? '' : ' sbx-disabled'}`}>
      <legend class="sbx-network-unlocks-legend">Network rules</legend>
      <details class="sbx-network-packs" open=${packsOpen || null}
        onToggle=${(event) => setPacksOpen(event.currentTarget.open)}>
        <summary class="sbx-network-subhead"><strong>Built-in rule packs</strong><${SandboxHelp}><${HelpDisclosure}
          id="sandbox-profile-editor-network-packs-help" label="built-in network rule packs"
          help=${NETWORK_PACKS_HELP} open=${helpOpen === 'sandbox-profile-editor-network-packs-help'}
          setOpen=${setHelpOpen}/></${SandboxHelp}></summary>
        <div class="sbx-network-pack-list">${(catalog.network_packs || []).map((pack) => {
        const packHelp = [pack.note || '', pack.warning ? `⚠ ${pack.warning}` : ''].filter(Boolean).join(' ');
        const packHelpID = `sandbox-profile-editor-network-pack-${String(pack.id || '').replace(/[^a-zA-Z0-9_-]+/g, '-')}-help`;
        return html`<div key=${pack.id} class="sbx-network-pack">
          <${SegmentedControl} className="sbx-network-pack-mode" label=${`${pack.label} network pack mode`}
            disabled=${!editable} value=${packMode(pack.id)} onChange=${(mode) => changePackMode(pack.id, mode)}
            options=${[['off', 'Off'], ['allow', 'Allow'], ['deny', 'Deny']]}/>
          <span class="sbx-network-pack-label">${pack.group ? `${pack.group} · ` : ''}${pack.label}</span>
          ${redundantLabel(packMode(pack.id), pack.entries) && html`<span class="sbx-network-redundant">${redundantLabel(packMode(pack.id), pack.entries)}</span>`}
          ${packHelp && html`<${SandboxHelp}><${HelpDisclosure} id=${packHelpID}
            label=${`${pack.label} network pack`} help=${packHelp}
            open=${helpOpen === packHelpID} setOpen=${setHelpOpen}/></${SandboxHelp}>`}
        </div>`;
      })}</div></details>
      <div class="sbx-network-subhead"><strong>Destinations</strong><${SandboxHelp}><${HelpDisclosure}
        id="sandbox-profile-editor-network-destinations-help" label="network access list"
        help=${NETWORK_DESTINATIONS_HELP} open=${helpOpen === 'sandbox-profile-editor-network-destinations-help'}
        setOpen=${setHelpOpen}/></${SandboxHelp}></div>
      ${hasDenyRules && html`<div class="sbx-network-deny-note" role="note">Deny enforcement depends on the launch target — see Effective policy preview.</div>`}
      <div class="sbx-network-table">
      <div class="sbx-rows sbx-network-rows sbx-network-pack-rows">${packRows.map(({ row, pack, mode }, index) => { const kind = selector(row); const hasModifier = kind === 'domain' && row.include_subdomains; return html`<div key=${`${mode}:${pack.id}:${index}`} class=${`sbx-row sbx-access-row sbx-network-row sbx-network-pack-row${hasModifier ? '' : ' sbx-network-row-no-modifier'}`}>
        <div class="sbx-network-mode-cell"><span class=${`sbx-network-mode-readonly sbx-state-${mode}`}>${mode === 'deny' ? 'Deny' : 'Allow'}</span>
          ${redundantLabel(mode, [row]) && html`<span class="sbx-network-redundant">${redundantLabel(mode, [row])}</span>`}</div>
        <span class="sbx-network-selector sbx-network-value-readonly">${kind}</span>
        <span class="sbx-network-value sbx-network-value-readonly">${kind === 'loopback' ? '—' : row[kind]}</span>
        ${hasModifier && html`<span class="sbx-network-modifier">subdomains</span>`}
        <span class="sbx-network-ports sbx-network-value-readonly">${(row.ports || []).join(', ') || 'all ports'}</span>
        <span class="sbx-network-pack-owner" title="This release-owned row is changed by toggling its pack.">${pack.label}</span>
      </div>`; })}</div>
      <div class="sbx-rows sbx-network-rows sbx-network-manual-rows">${manualRows.map(({ row, index, mode }) => { const kind = selector(row); const hasModifier = kind === 'domain'; return html`<div key=${manualRowKeys.current[mode][index]} class=${`sbx-row sbx-access-row sbx-network-row${hasModifier ? '' : ' sbx-network-row-no-modifier'}`}>
      <div class="sbx-network-mode-cell"><${SegmentedControl} className="sbx-network-rule-mode" label="Network row mode"
        disabled=${!editable} value=${mode} onChange=${(value) => changeRowMode(mode, index, value)}
        options=${[['allow', 'Allow'], ['deny', 'Deny']]}/>
        ${redundantLabel(mode, [row]) && html`<span class="sbx-network-redundant">${redundantLabel(mode, [row])}</span>`}</div>
      <${Select} class="sbx-network-selector" disabled=${!editable} value=${kind} onChange=${(value) => changeSelector(mode, index, value)} options=${[['host', 'host'], ['domain', 'domain'], ['cidr', 'CIDR'], ['loopback', 'loopback']]}/>
      ${kind === 'loopback' ? html`<span class="sbx-network-value sbx-network-value-readonly" aria-hidden="true">—</span>` : html`<input class="sbx-network-value" disabled=${!editable} value=${row[kind] || ''} placeholder=${kind === 'cidr' ? '192.0.2.0/24' : 'example.com'} onInput=${(event) => updateRow(mode, index, { [kind]: event.currentTarget.value })}/>`}
      ${hasModifier && html`<span class="sbx-network-modifier"><label class="sbx-inline-check"><input type="checkbox" disabled=${!editable} checked=${!!row.include_subdomains} onChange=${(event) => updateRow(mode, index, { include_subdomains: event.currentTarget.checked })}/> subdomains</label></span>`}
      <input class="sbx-network-ports" disabled=${!editable} list="sandbox-common-ports" value=${Array.isArray(row.ports) ? row.ports.join(', ') : row.ports || ''} placeholder="ports" title="Comma-separated ports. Common suggestions are 22, 80, and 443; leaving this blank matches all ports for the destination." onInput=${(event) => updateRow(mode, index, { ports: event.currentTarget.value })}/>
      <button type="button" disabled=${!editable} aria-label="Delete network row" onClick=${() => {
        manualRowKeys.current[mode].splice(index, 1);
        updateRows(mode, rowsFor(mode).filter((_, i) => i !== index));
      }}>×</button>
    </div>`; })}</div></div>
    <datalist id="sandbox-common-ports"><option value="443"/><option value="80, 443"/><option value="22"/></datalist>
    <button type="button" class="sbx-add-row" disabled=${!editable} onClick=${() => {
      const mode = rules.baseline === 'allow' ? 'deny' : 'allow';
      manualRowKeys.current[mode].push(`network-manual-row-${nextManualRowKey.current++}`);
      updateRows(mode, [...rowsFor(mode), { host: '', ports: [] }]);
    }}>＋ add ${rules.baseline === 'allow' ? 'deny' : 'allow'} destination</button>
    </fieldset>
    ${(catalog.global_network || []).length > 0 && html`<details class="sbx-inherited-access"><summary>Inherited global network config (${catalog.global_network.length})</summary>${catalog.global_network.map((row, index) => html`<div key=${index} class="sbx-rule-note"><strong>${row.origin?.harness} · ${row.origin?.setting}:</strong> ${JSON.stringify(row.entry || { mode: row.mode })}</div>`)}</details>`}
  </${SandboxSection}>`;
}

function SocketAccessEditor({ draft, setDraft, catalog, notice, setNotice }) {
  const rules = sandboxAccessAxes({ unix_sockets: draft.unix_sockets }).unix_sockets;
  const update = (patch) => setDraft((value) => ({ ...value, unix_sockets: { ...value.unix_sockets, ...patch } }));
  const updateRow = (index, patch) => update({ allow: rules.allow.map((row, i) => i === index ? { ...row, ...patch } : row) });
  const insert = (entry) => {
    const mode = entry.mode || 'list';
    const incoming = clone(entry.entries || []);
    const existing = new Set(rules.allow.map((row) => JSON.stringify(row)));
    const added = incoming.filter((row) => !existing.has(JSON.stringify(row)));
    const removed = mode === 'list' ? 0 : rules.allow.length;
    update({ mode, allow: mode === 'list' ? [...rules.allow, ...added] : [] });
    setNotice({ label: entry.label, added: added.length, skipped: incoming.length - added.length, removed, warning: entry.warning || '' });
  };
  return html`<${SandboxSection} id="sandbox-profile-editor-unix-sockets-section"
      className="sbx-access-axis" label="Unix sockets" help=${UNIX_SOCKETS_HELP}
      entryCount=${rules.allow.length}>
    <${Select} id="sandbox-profile-editor-unix-sockets-mode" value=${rules.mode || ''} onChange=${(mode) => update({ mode, allow: mode === 'list' ? rules.allow : [] })} options=${ACCESS_MODE_OPTIONS}/>
    ${rules.mode === 'list' && html`<div class="sbx-rows sbx-socket-rows">${rules.allow.map((row, index) => { const glob = Object.hasOwn(row, 'path_glob'); return html`<div key=${index} class="sbx-row sbx-access-row sbx-socket-row">
      <${SegmentedControl} className="sbx-socket-selector" label=${`Unix socket row ${index + 1} kind`}
        value=${glob ? 'path_glob' : 'path'} onChange=${(kind) => {
          if (kind === (glob ? 'path_glob' : 'path')) return;
          update({ allow: rules.allow.map((item, i) => i === index ? { [kind]: '' } : item) });
        }}
        options=${[['path', 'Path'], ['path_glob', 'Glob']]}/>
      <input class="sbx-socket-value" value=${glob ? row.path_glob || '' : row.path || ''} placeholder=${glob ? '/tmp/ssh-*/agent.*' : '/run/example.sock'} onInput=${(event) => updateRow(index, glob ? { path_glob: event.currentTarget.value } : { path: event.currentTarget.value })}/>
      <button type="button" aria-label="Delete Unix-socket row" onClick=${() => update({ allow: rules.allow.filter((_, i) => i !== index) })}>×</button>
    </div>`; })}</div>
    <button type="button" class="sbx-add-row" onClick=${() => update({ allow: [...rules.allow, { path: '' }] })}>＋ add socket</button>`}
    <details class="sbx-common-rules"><summary>＋ insert socket template</summary><div class="sbx-common-rule-list">${(catalog.socket_templates || []).map((entry) => html`<${CommonRuleEntry} key=${entry.id} variant="access" entry=${{ ...entry, description: entry.note, paths: (entry.entries || []).map((row) => row.path || row.path_glob) }} onAdd=${() => insert(entry)}/>` )}</div></details>
    ${(catalog.global_unix_sockets || []).length > 0 && html`<details class="sbx-inherited-access"><summary>Inherited global socket config (${catalog.global_unix_sockets.length})</summary>${catalog.global_unix_sockets.map((row, index) => html`<div key=${index} class="sbx-rule-note"><strong>${row.origin?.harness} · ${row.origin?.setting}:</strong> ${JSON.stringify(row.entry || { mode: row.mode })}</div>`)}</details>`}
    ${notice && html`<div class="sbx-common-rule-notice" role="status">Inserted “${notice.label}”: ${notice.added} added, ${notice.skipped} already present.${notice.removed ? ` ${notice.removed} incompatible existing row${notice.removed === 1 ? '' : 's'} removed.` : ''}${notice.warning ? ` ⚠ ${notice.warning}` : ''}</div>`}
  </${SandboxSection}>`;
}

function commonRulePaths(entry) {
  return [...new Set((entry?.paths || []).map((path) => String(path || '').trim()).filter(Boolean))];
}

function globalFilesystemAccessLabel(access) {
  return ({ read: 'read', write: 'write', deny: 'deny', 'deny-read': 'deny read', 'deny-write': 'deny write' })[access] || access;
}

function globalFilesystemHarnessLabel(harnesses) {
  const set = new Set(harnesses || []);
  if (set.has('claude') && set.has('codex')) return 'Claude + Codex';
  if (set.has('claude')) return 'Claude';
  if (set.has('codex')) return 'Codex';
  return 'global';
}

function globalFilesystemRuleTooltip(rule) {
  const access = globalFilesystemAccessLabel(rule.access);
  const origins = (rule.origins || []).map((origin) => {
    const harness = origin.harness === 'claude' ? 'Claude Code' : origin.harness === 'codex' ? 'Codex' : origin.harness;
    return `${harness}: ${origin.source} → ${origin.setting}.${origin.note ? ` ${origin.note}` : ''}`;
  });
  return [`Inherited ${access} rule for ${rule.path}. This row is read-only because it belongs to global harness config, not this profile.`, ...origins].join('\n');
}

function globalFilesystemForHarness(rows, filter) {
  if (filter === 'both') return rows || [];
  if (filter === 'none') return [];
  return (rows || []).flatMap((row) => {
    const origins = (row.origins || []).filter((origin) => origin.harness === filter);
    if (origins.length === 0 && !(row.harnesses || []).includes(filter)) return [];
    const originAccess = origins.map((origin) => origin.access).filter(Boolean);
    const access = originAccess.includes('write') ? 'write' : originAccess.includes('read') ? 'read' : originAccess[0] || row.access;
    return [{ ...row, access, harnesses: [filter], origins }];
  });
}

/* Comparison-only path identity, mirroring the daemon's own `filepath.Clean`:
   trailing separators, duplicated separators, `.` segments and `..` segments
   all name the same location there, so treating them as distinct lets a preset
   append a deny for a path the operator already authored as `write` — the
   daemon canonicalizes and folds deny over write, silently overriding the
   authored row while the notice claims it was left as authored. `..` is folded
   lexically rather than skipped because that is exactly what the daemon does:
   sandboxpolicy's canonicalization Cleans before it calls EvalSymlinks, so no
   `..` segment ever survives to be resolved against a symlink. Symlinks
   themselves stay unresolved — they need the filesystem — so two names for one
   inode remain distinct here, as they must.

   A leading `~` or `~/` expands against the daemon home shipped with the
   catalog before cleaning, in the same order as the daemon. `~otheruser/...`
   stays literal because the daemon does not guess another account's home.
   When talking to an older daemon that does not ship its home, `~` also stays
   literal so the comparison remains conservative.

   The inserted row always keeps the catalog's own spelling; only the
   comparison normalizes. */
function pathIdentity(path, home = '') {
  let raw = String(path || '').trim();
  if (!raw) return '';
  const daemonHome = String(home || '').trim();
  if (daemonHome && (raw === '~' || raw.startsWith('~/'))) {
    raw = raw === '~' ? daemonHome : `${daemonHome}/${raw.slice(2)}`;
  }
  const rooted = raw.startsWith('/');
  const out = [];
  for (const segment of raw.split('/')) {
    if (!segment || segment === '.') continue;
    if (segment !== '..') { out.push(segment); continue; }
    // `..` past the root is the root, as filepath.Clean has it; on a relative
    // path a leading `..` has nothing to pop and stays.
    if (out.length && out[out.length - 1] !== '..') out.pop();
    else if (!rooted) out.push('..');
  }
  if (rooted) return `/${out.join('/')}`;
  return out.length ? out.join('/') : '.';
}

function RequestList({ request, label, retry, children }) {
  if ((request.phase === 'idle' || request.phase === 'loading') && !request.data?.length) return html`<div class="template-empty">Loading ${label}…</div>`;
  if (request.phase === 'error' && !request.data?.length) return html`<div class="template-empty" role="alert">Could not load ${label}: ${request.error} <button onClick=${retry}>retry</button></div>`;
  return html`${request.phase === 'error' && html`<div class="island-error" role="alert">Refresh failed: ${request.error} <button onClick=${retry}>retry</button></div>`}${children}`;
}

function Manager({ kind, current, state, actions, confirmDiscard }) {
  const profiles = kind === 'profiles'; const roles = kind === 'roles';
  const all = profiles ? current.profiles : roles ? current.roles : current.sandboxProfiles;
  const filter = profiles ? current.profileFilter : roles ? current.roleFilter : current.sandboxFilter;
  const setFilter = profiles ? state.profileFilter : roles ? state.roleFilter : state.sandboxFilter;
  const request = current.requests[kind === 'sandbox' ? 'sandbox' : kind];
  const domKind = kind === 'sandbox' ? 'sandbox-profiles' : kind;
  const q = filter.trim().toLowerCase();
  const list = all.filter((item) => !q || [item.name, ...(item.aliases || []), item.disabled_reason, item.descr, item.role, item.model, item.harness, item.agent_name].some((value) => String(value || '').toLowerCase().includes(q)));
  const title = profiles ? html`<span class="profiles-word-regular">Spawn profiles</span><span class="profiles-word-wizard">Familiar patterns</span>` : roles ? html`<span class="roles-word-regular">Role library</span><span class="roles-word-wizard">Class library</span>` : html`<span class="sandbox-word-regular">Sandbox profiles</span><span class="sandbox-word-wizard">Wards</span>`;
  // Each panel remembers its own dragged size (dashPrefs, so it survives
  // reopen, daemon restart and tab). fitContent:false because these are LIST
  // panels, like the templates manager: content-tracking would pin the shrink
  // floor at the max-height cap, and auto-grow would re-expand a deliberately
  // shortened box whenever the live listing refreshes.
  return html`<${Overlay} id=${`${domKind}-manage-modal`} manage labelledby=${`${domKind}-manage-title`} onClose=${state.closeManager} confirmDiscard=${confirmDiscard} resizeKey=${`tclaude.dash.modalSize.${domKind}-manage`} fitContent=${false}>
    <h3 id=${`${domKind}-manage-title`}>${title}</h3>
    <p class="manage-intro">${profiles ? "Reusable bundles of the spawn dialog's launch and identity fields." : roles ? 'Named reusable behavior, guidance, and access presets.' : 'Filesystem and environment policy applied when an agent launches.'}</p>
    <div class="filter-bar"><input id=${`filter-${kind}`} value=${filter} onInput=${(event) => { setFilter.value = event.currentTarget.value; }} placeholder="Filter" autocomplete="off" spellcheck="false" autofocus /><span class="filter-count" id=${`filter-${kind}-count`}>${q ? `${list.length} / ${all.length}` : all.length}</span><button class="clear-filter" onClick=${() => { setFilter.value = ''; }}>×</button><span class="spacer"></span>
      ${profiles && html`<button id="profile-export-open" class="tool" onClick=${() => state.openDialog({ kind: 'profile-export' })}>⇪ export</button><button id="profile-import-open" class="tool" onClick=${() => state.openDialog({ kind: 'profile-import' })}>⤒ import</button>`}
      ${kind === 'sandbox' && html`<button id="sandbox-profile-export-open" class="tool" onClick=${() => state.openDialog({ kind: 'sandbox-export' })}>⇪ export</button><button id="sandbox-profile-import-open" class="tool" onClick=${() => state.openDialog({ kind: 'sandbox-import' })}>⤒ import</button><button id="sandbox-profile-scribe-open" class="tool" onClick=${() => actions.configureSandboxWithAgent({ name: '', filesystem: [], environment: [], network_access: '' })}>🤖 configure with agent</button>`}
      <button id=${profiles ? 'profile-create-open' : roles ? 'role-create-open' : 'sandbox-profile-create-open'} class="primary" onClick=${() => profiles ? actions.openProfileEditor() : roles ? actions.openRoleEditor() : actions.openSandboxEditor()}>${profiles ? html`<span class="profiles-word-regular">+ new profile</span><span class="profiles-word-wizard">+ new pattern</span>` : roles ? html`<span class="roles-word-regular">+ new role</span><span class="roles-word-wizard">+ new class</span>` : html`<span class="sandbox-word-regular">+ new sandbox profile</span><span class="sandbox-word-wizard">+ new ward</span>`}</button>
    </div>
    <div id=${profiles ? 'profiles-list' : roles ? 'roles-list' : 'sandbox-profiles-list'}><${RequestList} request=${request} label=${kind} retry=${() => actions.load(kind)}>${list.length ? list.map((item) => html`<div key=${item.name} class=${`template-card ${profiles ? 'profile' : roles ? 'role' : 'sandbox-profile'}-card${profiles && item.disabled ? ' profile-card-disabled' : ''}`} data-key=${item.name}><div class="tc-head"><span class="tc-name">${item.name}</span>${profiles && item.disabled ? html`<span class="tc-disabled" aria-label="Disabled profile">🚫 Disabled</span>` : null}${profiles && !item.disabled && item.operator_only ? html`<span class="tc-operator-only" aria-label="Operator-only profile">👤 Operator only</span>` : null}${profiles && item.aliases?.length ? html`<span class="tc-aliases">${profileAliasesLabel(item)}</span>` : null}<span class="tc-descr">${profiles ? profileSummary(item, { status: false }) : roles ? roleSummary(item) : sandboxProfileSummary(item)}</span><span class="tc-actions"><button class="tool" onClick=${() => profiles ? actions.openProfileEditor(item) : roles ? actions.openRoleEditor(item) : actions.openSandboxEditor(item)}>edit</button>${profiles && html`<button class="tool profile-clone" onClick=${() => actions.openProfileClone(item)}>clone</button>`}${kind === 'sandbox' && html`<button class="tool sandbox-profile-clone" onClick=${() => actions.openSandboxClone(item)}>clone</button>`}<button class="tool" onClick=${() => profiles ? actions.removeProfile(item.name) : roles ? actions.removeRole(item.name) : actions.removeSandbox(item.name)}>delete</button></span></div>${profiles && item.disabled && html`<div class="tc-sub tc-disabled-reason">${item.disabled_reason}</div>`}${roles && item.descr && html`<div class="tc-sub">${item.descr}</div>`}${kind === 'sandbox' && html`<div class="sbx-caps">${(item.filesystem || []).map((entry) => html`<div key=${`${entry.access}:${entry.path}`} class="sbx-cap"><span class=${`sbx-cap-tag sbx-cap-${entry.access}`}>${entry.access}</span><span class="sbx-cap-val" title=${entry.path}>${entry.path}</span></div>`)}${(item.includes || []).map((name) => html`<div key=${`inc:${name}`} class="sbx-cap"><span class="sbx-cap-tag sbx-cap-inc">include</span><span class="sbx-cap-val" title=${name}>${name}</span></div>`)}${(item.environment || []).map((entry) => { const binding = `${entry.name} → ${entry.value}`; return html`<div key=${`env:${entry.name}`} class="sbx-cap"><span class="sbx-cap-tag sbx-cap-env">env</span><span class="sbx-cap-val" title=${binding}>${binding}</span></div>`; })}${(item.agent_directories || []).map((name) => html`<div key=${`own:${name}`} class="sbx-cap"><span class="sbx-cap-tag sbx-cap-own">own</span><span class="sbx-cap-val" title=${`${name} — isolated per agent`}>${name}</span></div>`)}</div>`}</div>`) : html`<div class="template-empty">${all.length ? wizWord('No items match the filter.', 'No items match the filter.') : profiles ? wizWord('No spawn profiles yet', 'No familiar patterns yet') : roles ? wizWord('No roles yet', 'No classes yet') : wizWord('No sandbox profiles yet', 'No wards yet')}</div>`}</${RequestList}></div>
    <div class="modal-buttons"><span class="spacer"></span><button onClick=${state.closeManager}>Close</button></div>
  </${Overlay}>`;
}

function Select({ value, onChange, options, ...props }) { return html`<select ...${props} value=${value} onChange=${(event) => onChange(event.currentTarget.value)}>${options.map(([key, label]) => html`<option key=${key} value=${key}>${label}</option>`)}</select>`; }
function Row({ label, hidden = false, title = '', children }) { return html`<label class="cron-create-row" hidden=${hidden} title=${title}><span class="cron-create-label">${label}</span>${children}</label>`; }

function HarnessFields({ draft, setDraft, catalog, actions, profile = false, sandboxImpl = {} }) {
  const hEntry = harnessByName(catalog, draft.harness);
  const models = hEntry?.models || [];
  const hasModelList = models.length > 0;
  const [customModel, setCustomModel] = useState(() => hasModelList && !!draft.model && !models.includes(draft.model));

  // Preview warning and informational messages for the effective boundary. The
  // daemon decides — an explicit `off` is unsafe on any machine, while
  // `inherit` depends on host settings the browser cannot know, and OpenCode's
  // split server boundary needs a non-warning disclosure. The profile probe has
  // no dir, so the verdict reflects the portable, machine-global tiers.
  const [autonomyWarnings, setAutonomyWarnings] = useState([]);
  const [sandboxInfo, setSandboxInfo] = useState([]);
  const autonomyRequest = useRef(0);
  useEffect(() => {
    if (typeof actions?.loadUnsandboxedAutonomy !== 'function') return undefined;
    const request = ++autonomyRequest.current;
    // Selects fire on change, not per keystroke, so a short debounce only
    // collapses a rapid harness→mode retap; it is imperceptible otherwise.
    const timer = setTimeout(() => {
      Promise.resolve(actions.loadUnsandboxedAutonomy({
        harness: draft.harness, sandbox: draft.sandbox,
        sandboxImplementation: profile ? draft.sandbox_implementation : '',
        approval: draft.approval,
      })).then((result) => {
        if (request !== autonomyRequest.current) return;
        setSandboxInfo(result?.info || []);
        setAutonomyWarnings(result?.warnings || []);
      });
    }, 200);
    return () => clearTimeout(timer);
  }, [draft.harness, draft.sandbox, draft.sandbox_implementation, draft.approval, profile]);
  const updateHarness = (harness) => {
    const h = harnessByName(catalog, harness);
    setCustomModel(false);
    setDraft((current) => {
      const defaults = profile
        ? profileHarnessDefaults(h, current.sandbox_implementation)
        : harnessDefaults(h);
      return {
        ...current, harness, model: '', effort: '', ...defaults,
        trust_dir: '', remote_control: '', auto_memory: '', copilot_api: '', codex_app_server: '', fast_mode: '',
        ssh_workaround: !!h?.can_ssh_workaround,
        // Keep every explicit implementation visible across harness switches.
        // An incapable selection gets an inline refusal warning and the server
        // remains the apply authority.
        sandbox_implementation: current.sandbox_implementation || defaults.sandbox_implementation || '',
        sandbox_implementation_cleared: null,
      };
    });
  };
  const [helpOpen, setHelpOpen] = useState('');
  const modelID = 'profile-editor-model';
  const approvalID = 'profile-editor-approval';
  const sandboxID = 'profile-editor-sandbox';
  const toolsID = 'profile-editor-tools';
  const approvalLabel = draft.harness === 'codex' ? 'Approval policy' : 'Permission mode';
  const recommendedApproval = profile
    ? hEntry?.profile_recommended_approval || hEntry?.default_approval
    : hEntry?.default_approval;
  const approvalHelp = hEntry?.approval_mode_help?.[draft.approval] || '';
  const sandboxHelp = harnessBuiltinModeHelpForImplementation(
    hEntry?.sandbox_mode_help?.[draft.sandbox],
    draft.sandbox_implementation || '',
    draft.harness,
  );
  const toolsHelp = hEntry?.tools_mode_help?.[draft.tools] || '';
  const askTimeoutHelp = hEntry?.ask_timeout_mode_help?.[draft.ask_user_question_timeout] || '';
  const autoCompactWindowHint = autoCompactWindowHintFor(
    { autoCompactWindow: draft.auto_compact_window },
    {
      autoCompactWindowMin: Number(hEntry?.auto_compact_window_min) || 0,
      autoCompactWindowMax: Number(hEntry?.auto_compact_window_max) || 0,
    },
  );
  const contextWindowMaxHint = contextWindowMaxHintFor(
    { contextWindowMax: draft.context_window_max },
    {
      contextWindowMaxMin: Number(hEntry?.context_window_max_min) || 0,
      contextWindowMaxMax: Number(hEntry?.context_window_max_max) || 0,
    },
  );
  const harnessLabel = hEntry?.display_name || hEntry?.name || '';
  const sandboxImplOptions = sandboxImplOptionsFor(
    sandboxImpl?.options, harnessLabel, hEntry?.can_builtin_os_sandbox !== false,
  );
  const showHarnessBuiltinMode = !profile
    || (hEntry?.can_sandbox && draft.sandbox_implementation === 'harness-builtin');
  const sandboxImplCleared = sandboxImplClearedNoticeFor(
    { sandboxImplCleared: draft.sandbox_implementation_cleared },
  );
  const sandboxImplHint = sandboxImplHintFor(
    {
      sandboxImpl: draft.sandbox_implementation,
      harness: draft.harness,
      sandbox: draft.sandbox,
    },
    {
      showSandboxImpl: !!hEntry,
      sandboxImplDefault: sandboxImpl?.default || 'harness-builtin',
      sandboxImplCanBuiltin: hEntry?.can_builtin_os_sandbox !== false,
      sandboxImplHarness: harnessLabel,
      sandboxImplHarnessName: hEntry?.name || '',
      sandboxImplCanStacked: !!hEntry?.can_stacked,
      sandboxImplStackedAvailability: sandboxImpl?.stacked?.[hEntry?.name] || {},
      // A profile may legitimately pin stacked for a DIFFERENT machine — that
      // is the whole reason this editor discloses instead of refusing — so the
      // AppArmor answer is passed as what it is: a fact about the host running
      // this dashboard. The hint copy says "on this host" for the same reason.
      sandboxImplStackedAppArmorLikely: !!sandboxImpl?.stacked_apparmor_nested_bwrap_likely,
      sandboxImplHostAvailable: hEntry?.tclaude_layer_server_boundary
        ? sandboxImpl?.server_host_available !== false
        : sandboxImpl?.host_available !== false,
      sandboxImplHostReason: hEntry?.tclaude_layer_server_boundary
        ? sandboxImpl?.server_host_unavailable_reason || ''
        : sandboxImpl?.host_unavailable_reason || '',
    },
  );
  // Same disclosure contract as the spawn dialog: the caveat for the selected
  // implementation is read through the row's [!], never printed under it.
  const sandboxImplCaveat = sandboxImplCaveatFor(
    { sandboxImpl: draft.sandbox_implementation },
    {
      showSandboxImpl: !!hEntry,
      sandboxImplHarnessName: hEntry?.name || '',
      sandboxImplCanBuiltin: hEntry?.can_builtin_os_sandbox !== false,
    },
  );
  const sandboxImplHelp = sandboxImplCaveat
    ? `${sandboxImplCaveat} ${SANDBOX_IMPL_TITLE}` : SANDBOX_IMPL_TITLE;
  const showApprovalControls = !profile || approvalControlsVisibleFor({
    harness: draft.harness,
    sandbox: draft.sandbox,
    sandbox_implementation: draft.sandbox_implementation,
  }, sandboxImpl?.default);
  const reviewerHelp = approvalReviewerHelp(draft.approval_reviewer, draft.approval);
  const modelControl = hasModelList ? html`<div class="cron-create-target"><${Select} id=${modelID} value=${customModel ? '__custom__' : draft.model} onChange=${(value) => { if (value === '__custom__') { setCustomModel(true); change(setDraft, 'model', ''); } else { setCustomModel(false); change(setDraft, 'model', value); } }} options=${[['', 'Default (unset)'], ...models.map((model) => [model, model]), ['__custom__', 'Custom model id…']]} />${customModel && html`<input id=${`${modelID}-custom`} type="text" aria-label="Custom model id" value=${draft.model} onInput=${(event) => change(setDraft, 'model', event.currentTarget.value)} placeholder="model id or alias" autocomplete="off" spellcheck="false" autofocus />`}</div>` : html`<input id=${modelID} type="text" aria-label="Model id" value=${draft.model} onInput=${(event) => change(setDraft, 'model', event.currentTarget.value)} placeholder="blank = unset; model id or alias" autocomplete="off" spellcheck="false"/>`;
  return html`
    <${Row} label="Harness"><${Select} id="profile-editor-harness" value=${draft.harness} onChange=${updateHarness} options=${catalog.map((entry) => [entry.name, entry.display_name || entry.name])} /></${Row}>
    <${Row} label="Model" title="Model suggested by the selected harness. Blank leaves it unset; Custom model id accepts an out-of-catalog model supported by that harness.">${modelControl}</${Row}>
    <${Row} label="Effort"><${Select} value=${draft.effort} onChange=${(value) => change(setDraft, 'effort', value)} options=${[['', "Default (harness's own)"], ...(hEntry?.effort_levels || ['low', 'medium', 'high', 'xhigh', 'max']).map((value) => [value, value])]} /></${Row}>
    ${profile && hEntry && html`<${Row} label="Sandbox"
      title=${SANDBOX_IMPL_TITLE}>
      ${/* Row wraps its children in a <label>, whose activation behaviour
           forwards clicks from any NON-interactive descendant to the select.
           The popover body is a <span> — tabindex does not make it interactive
           — so reading or selecting text inside the open caveat would focus the
           select and pop its dropdown open over the words being read. The
           label still forwards clicks on its own "Sandbox" text, which is
           outside this column. */ ''}
      <div class="cron-create-target spawn-field-help-column"
        onClick=${(event) => event.stopPropagation()}>
        <div class="spawn-field-with-help">
          <${Select} id="profile-editor-sandbox-impl" value=${draft.sandbox_implementation}
            aria-describedby="profile-editor-sandbox-impl-help"
            onChange=${(value) => setDraft((current) => ({
    ...current, sandbox_implementation: value, sandbox_implementation_cleared: null,
  }))}
            options=${[['', `Unset (${RESOLVED_DEFAULTS_LABEL.toLowerCase()} at spawn)`],
    ...sandboxImplOptions.map((option) => [option.value,
      option.label + (option.value === hEntry?.profile_recommended_sandbox_implementation
        ? ' (recommended)' : '')])]} />
          <${HelpDisclosure} id="profile-editor-sandbox-impl"
            descriptionID="profile-editor-sandbox-impl-help" label="Sandbox"
            help=${sandboxImplHelp} warn=${!!sandboxImplCaveat}
            open=${helpOpen === 'profile-editor-sandbox-impl'} setOpen=${setHelpOpen} />
        </div>
        <${SandboxImplHint} hint=${sandboxImplHint} />
      </div>
    </${Row}>`}
    ${profile && sandboxImplCleared && html`<div class="cron-create-row"
      id="profile-editor-sandbox-impl-cleared-row" role="alert">
      <span class="cron-create-label"></span>
      <div class="cron-create-target">
        <div class="spawn-field-hint warn">${sandboxImplCleared.text}</div>
      </div>
    </div>`}
    <${HelpField} id=${sandboxID}
      label=${profile ? harnessBuiltinModeControlLabel(hEntry) : 'Sandbox'}
      title=${profile
        ? "Harness-native sandbox mode. Available only when the harness's built-in sandbox is selected above."
        : 'Launch containment for the agent. The modes are per-harness.'}
      value=${draft.sandbox}
      options=${(profile ? harnessBuiltinModeOptionsForImplementation({
    modes: hEntry?.sandbox_modes || [],
  }, draft.harness, draft.sandbox).modes : (hEntry?.sandbox_modes || [])).map((value) => ({
    value, label: harnessBuiltinModeOptionLabel(draft.harness, value, hEntry.default_sandbox),
  }))}
      onChange=${(event) => change(setDraft, 'sandbox', event.currentTarget.value)}
      help=${sandboxHelp} open=${helpOpen === sandboxID} setOpen=${setHelpOpen}
      disabled=${!showHarnessBuiltinMode} />
    <${HelpField} id=${approvalID} label=${approvalLabel} title="Controls when the harness requests approval; it does not change the sandbox."
      value=${draft.approval}
      options=${(hEntry?.approval_modes || []).map((value) => ({ value, label: approvalPolicyLabel(draft.harness, value, recommendedApproval) }))}
      onChange=${(event) => change(setDraft, 'approval', event.currentTarget.value)}
      help=${approvalHelp} open=${helpOpen === approvalID} setOpen=${setHelpOpen}
      disabled=${!hEntry?.can_approval || !showApprovalControls} />
    <${HelpField} id=${toolsID} label="Tool governance" title="Uniform action for OpenCode's bash, glob, grep, lsp, task, and skill tools."
      value=${draft.tools}
      options=${(hEntry?.tools_modes || []).map((value) => ({ value, label: value + (value === hEntry.default_tools ? ' (recommended)' : '') }))}
      onChange=${(event) => change(setDraft, 'tools', event.currentTarget.value)}
      help=${toolsHelp} open=${helpOpen === toolsID} setOpen=${setHelpOpen}
      disabled=${!hEntry?.can_tools} />
    <div class=${`cron-create-row${sandboxInfo.length === 0 ? ' sandbox-info-pending' : ''}`}
      id=${`${profile ? 'profile' : 'role'}-editor-sandbox-info`}>
      <span class="cron-create-label"></span>
      <div class="cron-create-target" role="status">
        ${sandboxInfo.map((message) => html`<div class="spawn-field-hint info" key=${message}>ℹ ${message}</div>`)}
      </div>
    </div>
    ${autonomyWarnings.length > 0 && html`<div class="cron-create-row" id=${`${profile ? 'profile' : 'role'}-editor-autonomy-warning`}>
      <span class="cron-create-label"></span>
      <div class="cron-create-target" role="alert">
        ${autonomyWarnings.map((warning) => html`<div class="spawn-field-hint warn" key=${warning}>${warning}</div>`)}
      </div>
    </div>`}
    ${profile && html`<${HelpField} id="profile-editor-approval-reviewer" label="Approval reviewer" title="Controls who decides eligible approval requests; it does not change the approval policy or sandbox."
      value=${draft.approval_reviewer} options=${approvalReviewerOptions(true)}
      onChange=${(event) => change(setDraft, 'approval_reviewer', event.currentTarget.value)}
      help=${reviewerHelp} open=${helpOpen === 'profile-editor-approval-reviewer'} setOpen=${setHelpOpen}
      disabled=${!hEntry?.can_auto_review || !showApprovalControls} />`}
    ${profile && html`<${HelpField} id="profile-editor-ask-timeout" label="Question timeout" title="AskUserQuestion idle-timeout for the agent."
      value=${draft.ask_user_question_timeout}
      options=${(hEntry?.ask_timeout_modes || []).map((value) => ({ value, label: value + (value === hEntry.default_ask_timeout ? ' (recommended)' : '') }))}
      onChange=${(event) => change(setDraft, 'ask_user_question_timeout', event.currentTarget.value)}
      help=${askTimeoutHelp} open=${helpOpen === 'profile-editor-ask-timeout'} setOpen=${setHelpOpen}
      disabled=${!hEntry?.can_ask_timeout} />`}
    ${profile && hEntry?.can_auto_compact_window && html`<${Row} label="Compact at"
      title=${AUTO_COMPACT_WINDOW_TITLE}>
      <div class="cron-create-target">
        <input id="profile-editor-auto-compact-window" type="text" aria-label="Auto-compact window (tokens)"
          value=${draft.auto_compact_window}
          onInput=${(event) => change(setDraft, 'auto_compact_window', event.currentTarget.value)}
          placeholder="blank = model default; e.g. 450k" autocomplete="off" spellcheck="false" inputmode="numeric" />
        ${autoCompactWindowHint && html`<div
          class=${`spawn-field-hint${autoCompactWindowHint.warn ? ' warn' : ''}`}>${autoCompactWindowHint.text}</div>`}
      </div>
    </${Row}>`}
    ${profile && hEntry?.can_context_window_max && html`<${Row} label="Context max"
      title=${CONTEXT_WINDOW_MAX_TITLE}>
      <div class="cron-create-target">
        <input id="profile-editor-context-window-max" type="text" aria-label="Configured Copilot context max (tokens)"
          value=${draft.context_window_max}
          onInput=${(event) => change(setDraft, 'context_window_max', event.currentTarget.value)}
          placeholder="blank = assumed; e.g. 272000" autocomplete="off" spellcheck="false" inputmode="numeric" />
        ${contextWindowMaxHint && html`<div class=${`spawn-field-hint${contextWindowMaxHint.warn ? ' warn' : ''}`}>${contextWindowMaxHint.text}</div>`}
      </div>
    </${Row}>`}
  `;
}

function ProfileEditor({ descriptor, roles = [], state, actions, confirmDiscard, openProfilePermissions, openProfileContextFeatures }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const { seed, options = {}, catalog = [] } = descriptor;
  const baseline = useMemo(() => profileDraft(seed, options, catalog), [descriptor]);
  const [draft, setDraft] = useState(() => clone(baseline));
  const [inspectedRole, setInspectedRole] = useState('');
  const dirty = dirtyDraft(draft, baseline); const local = !!options.local;
  const hEntry = harnessByName(catalog, draft.harness);
  const submit = async () => {
    state.error.value = '';
    if (!local && !draft.name.trim()) { state.error.value = 'profile name is required'; return; }
    if (!local && draft.disabled && !draft.disabled_reason.trim()) { state.error.value = 'a reason is required when disabling a profile'; return; }
    if (hEntry?.can_context_window_max) {
      const hint = contextWindowMaxHintFor(
        { contextWindowMax: draft.context_window_max },
        {
          contextWindowMaxMin: Number(hEntry.context_window_max_min) || 0,
          contextWindowMaxMax: Number(hEntry.context_window_max_max) || 0,
        },
      );
      if (hint?.warn) { state.error.value = hint.text; return; }
    }
    await actions.saveProfile({ draft, original: options.editExisting === false ? null : seed, options, payload: profilePayload(draft, seed, catalog, { local }) });
  };
  const saving = state.busy.value === 'profile-save';
  const sshWorkaroundAvailable = !!hEntry?.can_ssh_workaround
    && draft.sandbox === 'tclaude-agent';
  const selectedRoles = (draft.role_refs || []).map((name) => roles.find((role) => role.name === name) || { name, missing: true });
  const inspected = selectedRoles.find((role) => role.name === inspectedRole);
  const roleGrantSlug = (grant) => typeof grant === 'string' ? grant : String(grant?.slug || '');
  const roleGrantScope = (grant) => {
    if (typeof grant?.scope === 'string') return grant.scope;
    return Object.entries(grant?.scope || {})
      .sort(([left], [right]) => left.localeCompare(right))
      .map(([dimension, values]) => `${dimension}=${(values || []).join(', ')}`)
      .join('; ');
  };
  const roleTitle = (role) => [role.descr, role.brief,
    (role.permissions || []).length ? `Grants: ${role.permissions.map((grant) => roleGrantSlug(grant)).join(', ')}` : 'Grants: none']
    .filter(Boolean).join('\n\n');
  return html`<${Overlay} id="profile-editor-modal" labelledby="profile-editor-title" onClose=${state.closeDialog} onSubmitHotkey=${saving ? null : submit} dirty=${dirty} blocked=${saving} confirmDiscard=${confirmDiscard} registerClose=${registerClose}><h3 id="profile-editor-title">${local ? wizWord('Custom launch — this agent only', 'Bespoke summons — this familiar only') : options.cloneSourceName ? wizWord(`Clone profile: ${options.cloneSourceName}`, `Mirror pattern: ${options.cloneSourceName}`) : seed && options.editExisting !== false ? wizWord(`Edit profile: ${seed.name}`, `Edit pattern: ${seed.name}`) : wizWord('New spawn profile', 'New familiar pattern')}</h3>
    <${Row} label="Name" hidden=${local}><input id="profile-editor-name" value=${draft.name} onInput=${(event) => change(setDraft, 'name', event.currentTarget.value)} placeholder="profile name — kebab-or-snake-case label" autofocus autocomplete="off" spellcheck="false" /></${Row}>
    <${Row} label="Aliases" hidden=${local} title="Alternate handles for this same profile. Separate multiple aliases with commas."><input id="profile-editor-aliases" value=${draft.aliases_text} onInput=${(event) => change(setDraft, 'aliases_text', event.currentTarget.value)} placeholder="e.g. codex-reviewer, cold-reviewer" autocomplete="off" spellcheck="false" /></${Row}>
    <${Row} label="Disabled" hidden=${local} title="Keep this profile visible and editable, but block every spawn that would use it."><input id="profile-editor-disabled" type="checkbox" checked=${draft.disabled} onChange=${(event) => change(setDraft, 'disabled', event.currentTarget.checked)} /></${Row}>
    <${Row} label="Disable reason" hidden=${local || !draft.disabled} title="Required while disabled. Retained when enabled so it can be reviewed or reused later."><textarea id="profile-editor-disabled-reason" value=${draft.disabled_reason} onInput=${(event) => change(setDraft, 'disabled_reason', event.currentTarget.value)} rows="2" placeholder="required when disabled — retained after re-enabling" spellcheck="true" /></${Row}>
    <${Row} label="Operator only" hidden=${local} title="Allow humans to spawn with this profile, but reject agent-originated spawns."><input id="profile-editor-operator-only" type="checkbox" checked=${draft.operator_only} onChange=${(event) => change(setDraft, 'operator_only', event.currentTarget.checked)} /></${Row}>
    <${HarnessFields} draft=${draft} setDraft=${setDraft} catalog=${catalog} actions=${actions}
      sandboxImpl=${descriptor.sandboxImpl} profile />
    <${Row} label="Trust dir" hidden=${hEntry && !hEntry.can_dir_trust} title=${`Pre-trust the launch directory so the agent doesn't freeze on the harness's trust-folder dialog${hEntry?.dir_trust_store ? ` (edits ${hEntry.dir_trust_store})` : ''}.`}><${Select} id="profile-editor-trust-dir" value=${draft.trust_dir} onChange=${(value) => change(setDraft, 'trust_dir', value)} options=${TRI_OPTIONS}/></${Row}>
    <${Row} label="Remote control" hidden=${hEntry && !hEntry.can_remote_control}><${Select} id="profile-editor-remote-control" value=${draft.remote_control} onChange=${(value) => change(setDraft, 'remote_control', value)} options=${TRI_OPTIONS}/></${Row}>
    <${Row} label="Auto memory" hidden=${hEntry && !hEntry.can_auto_memory} title="Claude Code's built-in auto memory. tclaude disables it by default: agents sharing a repo all read one per-project memory store and cross-pollute each other's notes. Does not affect CLAUDE.md."><${Select} id="profile-editor-auto-memory" value=${draft.auto_memory} onChange=${(value) => change(setDraft, 'auto_memory', value)} options=${AUTO_MEMORY_TRI_OPTIONS}/></${Row}>
    <${Row} label="Copilot drive" hidden=${hEntry && !hEntry.can_copilot_api} title="EXPERIMENTAL: drive the agent over Copilot's embedded JSON-RPC API (copilot --ui-server) instead of typing into its pane with tmux send-keys. Messages, rename and compaction become typed calls, context is read live, and an agent blocked on a permission prompt becomes visible; soft exit still uses keystrokes. A spawn is refused unless the launch directory is already trusted — set the Trust dir row above, or pre-trust it by hand — and unless the pane shares host loopback. The endpoint is unauthenticated and loopback-bound. Unset means send-keys — the two drives run side by side and agents move over one at a time."><${Select} id="profile-editor-copilot-api" value=${draft.copilot_api} onChange=${(value) => change(setDraft, 'copilot_api', value)} options=${COPILOT_API_TRI_OPTIONS}/></${Row}>
    <${Row} label="Codex drive" hidden=${hEntry && !hEntry.can_codex_app_server} title="EXPERIMENTAL: launch the normal Codex TUI against a private per-agent app-server. The TUI is the sole birth writer and agentd binds the created thread without replay. Requires Codex 0.147.x and fails closed if unavailable. Unset means send-keys for deliberate A/B testing."><${Select} id="profile-editor-codex-app-server" value=${draft.codex_app_server} onChange=${(value) => change(setDraft, 'codex_app_server', value)} options=${CODEX_APP_SERVER_TRI_OPTIONS}/></${Row}>
    <${Row} label="Fast mode" hidden=${hEntry && !hEntry.can_fast_mode} title="Codex fast mode uses a higher-cost, lower-latency service tier. Harness default leaves the global Codex config in charge."><${Select} id="profile-editor-fast-mode" value=${draft.fast_mode} onChange=${(value) => change(setDraft, 'fast_mode', value)} options=${FAST_MODE_TRI_OPTIONS}/></${Row}>
    <${Row} label="SSH workaround" hidden=${!hEntry?.can_ssh_workaround} title=${sshWorkaroundAvailable ? "Use an agent-owned copy of the host SSH client config to avoid Codex sandbox ownership errors. This overrides Git core.sshCommand; uncheck it if the workaround conflicts with your setup." : "Available only for the Codex tclaude-agent managed sandbox."}><input id="profile-editor-ssh-workaround" type="checkbox" checked=${sshWorkaroundAvailable && draft.ssh_workaround} disabled=${!sshWorkaroundAvailable} onChange=${(event) => change(setDraft, 'ssh_workaround', event.currentTarget.checked)} /></${Row}>
    <${Row} label="Roles" hidden=${local} title="Behavioral guidance and default access from the role library. Launch settings remain owned by this spawn profile."><div class="agent-spawn-role-picker">
      <div class="agent-spawn-role-chips" id="profile-editor-role-refs">${selectedRoles.map((role) => html`<span key=${role.name} class=${`agent-spawn-role-chip${role.missing ? ' missing' : ''}`}>
        <button type="button" class="role-chip-inspect" title=${role.missing ? 'This role is missing from the library' : roleTitle(role)} onClick=${() => setInspectedRole(inspectedRole === role.name ? '' : role.name)}>${role.name}</button>
        <button type="button" class="role-chip-remove" aria-label=${`Remove role ${role.name}`} onClick=${() => { setInspectedRole(inspectedRole === role.name ? '' : inspectedRole); change(setDraft, 'role_refs', draft.role_refs.filter((name) => name !== role.name)); }}>×</button>
      </span>`)}<select id="profile-editor-role-add" value="" aria-label="Add role" onChange=${(event) => { const value = event.currentTarget.value; if (value) change(setDraft, 'role_refs', [...draft.role_refs, value]); event.currentTarget.value = ''; }}><option value="">＋ Add role…</option>${roles.filter((role) => !draft.role_refs.includes(role.name)).map((role) => html`<option value=${role.name}>${role.name}${role.descr ? ` — ${role.descr}` : ''}</option>`)}</select></div>
      <div class="spawn-field-hint">Hover or click a role to inspect its brief and grants.</div>
      ${inspected && html`<div class=${`agent-spawn-role-inspect${inspected.missing ? ' missing' : ''}`}>${inspected.missing ? '⚠ This role is no longer in the library.' : html`<div>${inspected.descr || ''}</div><div><b>Brief</b> ${inspected.brief || 'none'}</div><div><b>Grants</b> ${(inspected.permissions || []).length ? html`<span class="role-inspect-grants">${inspected.permissions.map((grant) => html`<code>${roleGrantSlug(grant)}${roleGrantScope(grant) ? ` · ${roleGrantScope(grant)}` : ''}</code>`)}</span>` : 'none'}</div>`}</div>`}
    </div></${Row}>
    ${[['Agent name', 'agent_name', 'optional — names the spawned agent'], ['Role label', 'role', 'optional — routing/display label; defaults independently of the role preset'], ['Descr', 'descr', 'optional — short one-line description']].map(([label, key, placeholder]) => html`<${Row} key=${key} label=${label} hidden=${local}><input value=${draft[key]} onInput=${(event) => change(setDraft, key, event.currentTarget.value)} placeholder=${placeholder} autocomplete="off" spellcheck="false"/></${Row}>`)}
    <${Row} label="Initial msg" hidden=${local}><textarea value=${draft.initial_message} onInput=${(event) => change(setDraft, 'initial_message', event.currentTarget.value)} rows="3" placeholder="optional — task brief pre-filled into the spawn dialog" spellcheck="false" /></${Row}>
    <${Row} label="Profile context" title="Extra startup guidance delivered to every agent that resolves this profile. It is kept separate from the per-spawn task brief and group context."><textarea id="profile-editor-startup-context" value=${draft.startup_context} onInput=${(event) => change(setDraft, 'startup_context', event.currentTarget.value)} rows="5" placeholder="optional — model/profile-specific guidance injected into every spawn" spellcheck="false" /></${Row}>
    ${[['Sync worktree', 'sync_worktree'], ['Auto focus', 'auto_focus'], ['Group context', 'include_group_default_context'], ['Group owner', 'is_owner']].map(([label, key]) => html`<${Row} key=${key} label=${label} hidden=${local && key !== 'is_owner'}><${Select} id=${key === 'is_owner' ? 'profile-editor-owner' : `profile-editor-${key.replaceAll('_', '-')}`} value=${draft[key]} onChange=${(value) => change(setDraft, key, value)} options=${TRI_OPTIONS}/></${Row}>`)}
    <div class="cron-create-row"><span class="cron-create-label">Permissions</span><button id="profile-editor-perms" class="tool" type="button" onClick=${() => openProfilePermissions({ overrides: draft.permission_overrides, ownsGroup: readTri(draft.is_owner) === true, label: draft.agent_name.trim(), roleGrants: selectedRoles.filter((role) => !role.missing).flatMap((role) => (role.permissions || []).map((grant) => ({ slug: roleGrantSlug(grant), scope: roleGrantScope(grant), role: role.name }))), onSave: (kept) => change(setDraft, 'permission_overrides', kept) })}>Permissions…</button><span>${Object.keys(draft.permission_overrides).length || ''}</span></div>
    ${(!hEntry || hEntry.can_context_features) && html`<div class="cron-create-row" title="How much of Claude Code's startup context agents from this profile load. Trimming bundled skills, unused tool schemas and system-prompt blocks leaves more of the window for the actual task."><span class="cron-create-label">Startup context</span><button id="profile-editor-context-features" class="tool" type="button" onClick=${() => openProfileContextFeatures({ catalog: hEntry?.context_features || [], selection: draft.context_features, label: draft.agent_name.trim(), onSave: (kept) => change(setDraft, 'context_features', kept) })}>Context…</button><span>${contextFeatureSummary(draft.context_features)}</span></div>`}
    <div class="cron-create-error" role="alert">${state.error.value}</div><div class="modal-buttons"><button disabled=${saving} onClick=${() => { void requestClose(); }}>Cancel</button><span class="spacer"></span><button id="profile-editor-submit" class="primary" disabled=${saving} onClick=${submit}>${saving ? 'Saving…' : local ? 'Apply' : 'Save profile'}</button></div>
  </${Overlay}>`;
}

function RoleEditor({ descriptor, state, actions, confirmDiscard, openProfilePermissions }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const { seed } = descriptor;
  const baseline = useMemo(() => roleDraft(seed), [descriptor]);
  const [draft, setDraft] = useState(() => clone(baseline)); const dirty = dirtyDraft(draft, baseline);
  const saving = state.busy.value === 'role-save';
  const submit = async () => { state.error.value = ''; if (!draft.name.trim()) { state.error.value = 'role name is required'; return; } await actions.saveRole({ draft, original: seed, payload: rolePayload(draft) }); };
  return html`<${Overlay} id="role-editor-modal" labelledby="role-editor-title" onClose=${state.closeDialog} dirty=${dirty} blocked=${saving} confirmDiscard=${confirmDiscard} registerClose=${registerClose}><h3 id="role-editor-title">${seed ? `Edit role: ${seed.name}` : 'New role'}</h3>
    <${Row} label="Name"><input id="role-editor-name" value=${draft.name} onInput=${(event) => change(setDraft, 'name', event.currentTarget.value)} placeholder="role name — kebab-or-snake-case label (e.g. reviewer)" autofocus autocomplete="off" spellcheck="false" /></${Row}><${Row} label="Descr"><input id="role-editor-descr" value=${draft.descr} onInput=${(event) => change(setDraft, 'descr', event.currentTarget.value)} placeholder="optional — short one-line description" autocomplete="off" spellcheck="false" /></${Row}><${Row} label="Brief"><textarea id="role-editor-brief" rows="5" value=${draft.brief} onInput=${(event) => change(setDraft, 'brief', event.currentTarget.value)} placeholder="canonical role-brief — prepended to a referencing agent's startup context (newlines OK)" spellcheck="false" /></${Row}>
    <div class="cron-create-row"><span class="cron-create-label">Permissions</span><button id="role-editor-perms" class="tool" type="button" onClick=${() => openProfilePermissions({
      overrides: grantListToOverrides(draft.permissions), grantOnly: true, subject: 'role',
      label: draft.name.trim(),
      onSave: (kept) => change(setDraft, 'permissions', grantOverridesToList(kept)),
    })}>Permissions…</button><span>${draft.permissions.length || ''}</span></div>
    <div class="cron-create-error" role="alert">${state.error.value}</div><div class="modal-buttons"><button disabled=${saving} onClick=${() => { void requestClose(); }}>Cancel</button><span class="spacer"></span><button id="role-editor-submit" class="primary" disabled=${saving} onClick=${submit}>${saving ? 'Saving…' : 'Save role'}</button></div>
  </${Overlay}>`;
}

function sandboxFilesystemEditorRows(filesystem, spellings) {
  const retained = new Map((spellings?.rules || []).map((rule) => [rule.resolved_path, rule.spellings || []]));
  return (filesystem || []).map((row) => {
    const aliases = retained.get(row.path) || [];
    if (!aliases.length) return clone(row);
    return {
      ...clone(row),
      path: aliases[0],
      _resolved_path: row.path,
      _spellings: clone(aliases),
    };
  });
}

// sandboxMountPathWire keeps an authored mount_path (TCL-866) on the wire.
// Omitting the key entirely when there is none keeps the request byte-identical
// for every profile that has no projection, and dropping the field for rows the
// editor cannot yet author would silently delete authority the operator (or the
// profile scribe) put there.
function sandboxMountPathWire(row) {
  const mountPath = (row.mount_path || '').trim();
  return mountPath ? { mount_path: mountPath } : {};
}

function sandboxFilesystemWire(draft, baseline) {
  // Deliberately keyed on the HOST path alone. Retained spellings belong to the
  // host directory, so changing where that directory is mounted inside the
  // sandbox does not invalidate them — and treating it as a re-authoring would
  // silently replace the canonical path with the operator's alias spelling and
  // drop the sidecar, from an edit that changed nothing about which directory
  // is granted (TCL-866).
  const pathKey = (rows) => JSON.stringify((rows || []).map((row) => row.path));
  const pathsUnchanged = pathKey(draft.filesystem) === pathKey(baseline.filesystem);
  const retained = draft.filesystem_spellings;
  if (pathsUnchanged && retained?.version === 1) {
    return {
      filesystem: (draft.filesystem || []).map((row) => ({
        path: row._resolved_path || row.path,
        access: row.access,
        ...sandboxMountPathWire(row),
      })),
      filesystem_spellings: clone(retained),
    };
  }
  // A path add/remove/edit is an explicit re-authoring operation. The daemon
  // canonicalizes the visible spellings and writes a fresh sidecar; it never
  // infers new launch authority from an old retained spelling.
  return {
    filesystem: (draft.filesystem || []).map((row) => ({
      path: row.path, access: row.access, ...sandboxMountPathWire(row),
    })),
    filesystem_spellings: null,
  };
}

function SandboxPreLaunchEditor({ blocks, setDraft, validation }) {
  const rows = blocks || [];
  const update = (index, patch) => setDraft((draft) => ({
    ...draft,
    pre_launch: (draft.pre_launch || []).map((block, i) =>
      i === index ? { ...block, ...patch } : block),
  }));
  const remove = (index) => setDraft((draft) => ({
    ...draft,
    pre_launch: (draft.pre_launch || []).filter((_, i) => i !== index),
  }));
  const move = (index, offset) => setDraft((draft) => {
    const pre_launch = [...(draft.pre_launch || [])];
    const target = index + offset;
    if (target < 0 || target >= pre_launch.length) return draft;
    [pre_launch[index], pre_launch[target]] = [pre_launch[target], pre_launch[index]];
    return { ...draft, pre_launch };
  });
  return html`<div class="sbx-prelaunch-intro"><strong>Execution order:</strong> blocks run top to bottom in the launching shell.</div>
    ${validation.profile.map((error, index) => html`<div key=${index} class="sbx-prelaunch-error" role="alert">${error}</div>`)}
    <div class="sbx-prelaunch-list">${rows.map((block, index) => {
    const errors = validation.blocks[index] || { name: [], script: [], exports: [] };
    const nameErrorID = `sandbox-profile-editor-pre-launch-name-error-${index}`;
    const scriptErrorID = `sandbox-profile-editor-pre-launch-script-error-${index}`;
    const exportsErrorID = `sandbox-profile-editor-pre-launch-exports-error-${index}`;
    return html`<div key=${block._editor_id || index} role="group"
      aria-label=${`Block ${index + 1}: ${(block.name || '').trim() || 'unnamed'}`}
      class=${`sbx-prelaunch-card${[...errors.name, ...errors.script, ...errors.exports].length ? ' is-invalid' : ''}`}>
      <div class="sbx-prelaunch-head">
        <span class="sbx-prelaunch-order" aria-label=${`Execution position ${index + 1}`}>${index + 1}</span>
        <label class="sbx-prelaunch-name">Block name
          <input value=${block.name || ''} placeholder="setup-tools" autocomplete="off" spellcheck="false"
            aria-invalid=${errors.name.length ? 'true' : null}
            aria-describedby=${errors.name.length ? nameErrorID : null}
            onInput=${(event) => update(index, { name: event.currentTarget.value })}/>
        </label>
        <div class="sbx-prelaunch-actions" aria-label=${`Reorder or remove block ${index + 1}`}>
          <button type="button" disabled=${index === 0} aria-label=${`Move block ${index + 1} up`}
            title="Move earlier" onClick=${() => move(index, -1)}>↑</button>
          <button type="button" disabled=${index === rows.length - 1} aria-label=${`Move block ${index + 1} down`}
            title="Move later" onClick=${() => move(index, 1)}>↓</button>
          <button type="button" class="sbx-prelaunch-remove" aria-label=${`Remove block ${index + 1}`}
            onClick=${() => remove(index)}>Remove</button>
        </div>
      </div>
      ${errors.name.length > 0 && html`<div id=${nameErrorID} class="sbx-prelaunch-error" role="alert">${errors.name.join(' ')}</div>`}
      <label class="sbx-prelaunch-script">Script
        <textarea rows="8" value=${block.script || ''} placeholder=${'#!/usr/bin/env bash\nexport TOOL_HOME=/tmp/tool'} spellcheck="false"
          aria-invalid=${errors.script.length ? 'true' : null}
          aria-describedby=${errors.script.length ? scriptErrorID : null}
          onInput=${(event) => update(index, { script: event.currentTarget.value })}/>
      </label>
      ${errors.script.length > 0 && html`<div id=${scriptErrorID} class="sbx-prelaunch-error" role="alert">${errors.script.join(' ')}</div>`}
      <label class="sbx-prelaunch-exports">Exports <span>comma or space separated</span>
        <input value=${block._exports_text || ''} placeholder="PATH, TOOL_HOME" autocomplete="off" spellcheck="false"
          aria-invalid=${errors.exports.length ? 'true' : null}
          aria-describedby=${errors.exports.length ? exportsErrorID : null}
          onInput=${(event) => update(index, { _exports_text: event.currentTarget.value })}/>
      </label>
      ${errors.exports.length > 0 && html`<div id=${exportsErrorID} class="sbx-prelaunch-error" role="alert">${errors.exports.join(' ')}</div>`}
    </div>`;
  })}</div>
    <button type="button" class="sbx-add-row sbx-prelaunch-add" disabled=${rows.length >= 32}
      onClick=${() => setDraft((draft) => ({ ...draft, pre_launch: [
        ...(draft.pre_launch || []), sandboxPreLaunchNewEditorRow(),
      ] }))}>＋ add block</button>`;
}

function SandboxEditor({ descriptor, sandboxProfiles, state, actions, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const seed = descriptor.seed || null; const options = descriptor.options || {};
  const evaluationHarnesses = sandboxEvaluationHarnesses(descriptor.catalog);
  const baseline = useMemo(() => {
    const axes = sandboxAccessAxes(seed || {});
    const network = sandboxNetworkAuthoring(seed || {});
    const filesystem_spellings = clone(seed?.filesystem_spellings ?? null);
    return { id: seed?.id || 0, name: seed?.name || '', filesystem: sandboxFilesystemEditorRows(seed?.filesystem || [], filesystem_spellings), filesystem_spellings, filesystem_root: seed?.filesystem_root || '', environment: clone(seed?.environment || []), includes: clone(seed?.includes || []), agent_directories: clone(seed?.agent_directories || []), network_access: '', network, unix_sockets: axes.unix_sockets, resource_limits: { memory: seed?.resource_limits?.memory || '', cpu: seed?.resource_limits?.cpu == null ? '' : String(seed.resource_limits.cpu) }, darwin_allow_mach_register: !!seed?.darwin_allow_mach_register, ...(seed?.pre_launch ? { pre_launch: sandboxPreLaunchEditorRows(seed.pre_launch) } : {}) };
  }, [descriptor]);
  const initialFilesystemWire = sandboxFilesystemWire(baseline, baseline);
  const [draft, setDraft] = useState(() => clone(baseline)); const [advanced, setAdvanced] = useState(false); const [rawFS, setRawFS] = useState(() => JSON.stringify(initialFilesystemWire.filesystem, null, 2)); const [rawSpellings, setRawSpellings] = useState(() => JSON.stringify(initialFilesystemWire.filesystem_spellings, null, 2)); const [rawEnv, setRawEnv] = useState(() => JSON.stringify(baseline.environment, null, 2)); const [rawIncludes, setRawIncludes] = useState(() => JSON.stringify(baseline.includes, null, 2)); const [rawAgentDirs, setRawAgentDirs] = useState(() => JSON.stringify(baseline.agent_directories, null, 2)); const [rawNetwork, setRawNetwork] = useState(() => JSON.stringify(baseline.network, null, 2)); const [rawSockets, setRawSockets] = useState(() => JSON.stringify(baseline.unix_sockets, null, 2)); const [rawResources, setRawResources] = useState(() => JSON.stringify(sandboxResourceLimitsForWire(baseline.resource_limits), null, 2)); const [rawPreLaunch, setRawPreLaunch] = useState(() => JSON.stringify(sandboxPreLaunchForWire(baseline.pre_launch || []), null, 2));
  // The audited common-rule presets. They are pure row inserters: nothing from
  // the catalog is persisted, so a profile never depends on it being loaded.
  const [commonRules, setCommonRules] = useState({ version: 0, categories: [], informational: [], global_filesystem: [], global_network: [], global_unix_sockets: [], network_packs: [], network_templates: [], socket_templates: [], global_config_warnings: [] });
  // Global harness rows are context, not draft state. Keep the potentially
  // long ambient list folded until the operator asks to inspect it.
  const [showGlobalFilesystem, setShowGlobalFilesystem] = useState(false);
  const [globalHarnessFilter, setGlobalHarnessFilter] = useState('both');
  // The menu is long and most profiles never touch it, so it ships folded.
  const [commonRulesOpen, setCommonRulesOpen] = useState(false);
  // What the last insertion did, including the entry's warning — the operator
  // must see the consequence of the rows that just appeared in the table.
  const [commonRuleNotice, setCommonRuleNotice] = useState(null);
  const [socketTemplateNotice, setSocketTemplateNotice] = useState(null);
  const [evaluateHarness, setEvaluateHarness] = useState('');
  const [evaluateImplementation, setEvaluateImplementation] = useState('harness-builtin');
  const [evaluatePlatform, setEvaluatePlatform] = useState(() =>
    ['linux', 'darwin'].includes(descriptor.sandboxImpl?.platform)
      ? descriptor.sandboxImpl.platform : 'linux');
  const [prediction, setPrediction] = useState(null);
  const [predictionError, setPredictionError] = useState('');
  const [predictionBusy, setPredictionBusy] = useState(false);
  const [effectiveContext, setEffectiveContext] = useState(0);
  // Index of the filesystem row whose "mount at" popover is open, or -1. One
  // at a time: the panel renders beneath its row, so two open panels would push
  // the rows apart and make the table hard to read.
  const [mountPathRow, setMountPathRow] = useState(-1);
  // Only one mount panel is open at a time, so one disclosure key covers every
  // row's [?]. The two paragraphs this replaced doubled the panel's height for
  // copy an operator reads once and then knows.
  //
  // Every path that closes a panel clears this too. A remembered open key would
  // reopen the popover the next time that row's panel mounts — on top of the
  // mount-path input the panel has just autofocused, so the operator would be
  // typing under a popover they did not open.
  const [mountHelpOpen, setMountHelpOpen] = useState('');
  // The feed is optional and its failures are the menu's own business. They
  // must never reach `state.error`, which carries save and validation
  // refusals: a late rejection would replace the reason a save was refused
  // with an explanation of a convenience the operator did not ask for.
  const [commonRuleFeedError, setCommonRuleFeedError] = useState('');
  const [commonRuleFeedBusy, setCommonRuleFeedBusy] = useState(false);
  const [commonRuleFeedSettled, setCommonRuleFeedSettled] = useState(
    typeof actions.loadCommonRuleCatalog !== 'function',
  );
  const commonRuleGeneration = useRef(0);
  // Retry stays live even while a load is in flight: a request that never
  // settles would otherwise leave the only way back permanently disabled. A
  // second attempt simply supersedes the first by generation.
  const loadCommonRules = () => {
    if (typeof actions.loadCommonRuleCatalog !== 'function') return;
    const generation = ++commonRuleGeneration.current;
    setCommonRuleFeedBusy(true); setCommonRuleFeedSettled(false);
    // Resolve.then rather than a bare call: a feed that throws synchronously
    // must land in the catch like any other failure, or the busy flag sticks.
    Promise.resolve().then(() => actions.loadCommonRuleCatalog()).then((value) => {
      if (generation !== commonRuleGeneration.current) return;
      setCommonRules(value || { version: 0, categories: [], informational: [], global_filesystem: [], global_network: [], global_unix_sockets: [], network_packs: [], network_templates: [], socket_templates: [], global_config_warnings: [] });
      setCommonRuleFeedError(''); setCommonRuleFeedBusy(false); setCommonRuleFeedSettled(true);
    }).catch((error) => {
      if (generation !== commonRuleGeneration.current) return;
      setCommonRuleFeedError(message(error)); setCommonRuleFeedBusy(false); setCommonRuleFeedSettled(true);
    });
  };
  // Unmount bumps the generation so an in-flight load resolves into nothing.
  useEffect(() => { loadCommonRules(); return () => { commonRuleGeneration.current++; }; }, []);
  useEffect(() => {
    if (commonRuleFeedError) setCommonRulesOpen(true);
  }, [commonRuleFeedError]);
  const [directoryStatus, setDirectoryStatus] = useState({ missing: [], creatable: [] }); const [directoryBusy, setDirectoryBusy] = useState(false);
  const directoryGeneration = useRef(0); const submitRef = useRef(null); const wasSaving = useRef(false); const filesystemSignature = JSON.stringify(draft.filesystem); const latestFilesystem = useRef(filesystemSignature); latestFilesystem.current = filesystemSignature;
  const dirty = dirtyDraft(draft, baseline);
  const saving = state.busy.value === 'sandbox-save';
  const setFS = (index, patch) => setDraft((value) => ({ ...value, filesystem: value.filesystem.map((row, i) => {
    if (i !== index) return row;
    const next = { ...row, ...patch };
    // A deny hides a host path rather than projecting one, so mount_path is a
    // validation error on it. Drop the value when the operator switches the row
    // to deny rather than letting the save fail later with a server-side
    // refusal. The change is visible: the row's mount glyph greys out.
    if (next.access === 'deny') delete next.mount_path;
    return next;
  }) }));
  const setEnv = (index, patch) => setDraft((value) => ({ ...value, environment: value.environment.map((row, i) => i === index ? { ...row, ...patch } : row) }));
  const parseRaw = () => { const filesystem = JSON.parse(rawFS || '[]'); const filesystem_spellings = JSON.parse(rawSpellings || 'null'); const environment = JSON.parse(rawEnv || '[]'); const includes = JSON.parse(rawIncludes || '[]'); const agent_directories = JSON.parse(rawAgentDirs || '[]'); const network = JSON.parse(rawNetwork || '{}'); const unix_sockets = JSON.parse(rawSockets || '{}'); const resource_limits = JSON.parse(rawResources || '{}'); if (![filesystem, environment, includes, agent_directories].every(Array.isArray)) throw new Error('filesystem, environment, includes and agent dirs must be arrays'); if (filesystem_spellings !== null && (!filesystem_spellings || Array.isArray(filesystem_spellings))) throw new Error('filesystem spellings must be a JSON object or null'); if (!network || typeof network !== 'object' || Array.isArray(network) || !unix_sockets || typeof unix_sockets !== 'object' || Array.isArray(unix_sockets) || !resource_limits || typeof resource_limits !== 'object' || Array.isArray(resource_limits)) throw new Error('network and unix sockets must be JSON objects; resource limits must be a JSON object'); if (network.allow != null && !Array.isArray(network.allow) || network.deny != null && !Array.isArray(network.deny) || unix_sockets.allow != null && !Array.isArray(unix_sockets.allow)) throw new Error('network allow/deny and Unix-socket allow fields must be arrays'); if (network.packs != null && !Array.isArray(network.packs) || network.deny_packs != null && !Array.isArray(network.deny_packs)) throw new Error('network packs and deny_packs must be arrays'); const resourceErrors = sandboxResourceLimitErrors(resource_limits); if (resourceErrors.length) throw new Error(resourceErrors[0]); const rowError = accessRowShapeError(network, unix_sockets); if (rowError) throw new Error(rowError); const pre_launch = JSON.parse(rawPreLaunch || '[]'); const preLaunchErrors = sandboxPreLaunchValidation(pre_launch).errors; if (preLaunchErrors.length) throw new Error(preLaunchErrors[0]); const axes = sandboxAccessAxes({ network, unix_sockets }); /* Omitting the key on a block-less profile keeps the draft byte-identical to the baseline, so merely
     opening and closing this panel does not raise a spurious discard prompt. Clearing an existing profile's
     blocks still emits the explicit empty array the daemon needs to distinguish "clear" from "leave alone". */
  return { filesystem, filesystem_spellings, environment, includes, agent_directories, ...(baseline.pre_launch === undefined && draft.pre_launch === undefined && pre_launch.length === 0 ? {} : { pre_launch }), network: axes.network, unix_sockets: axes.unix_sockets, resource_limits: sandboxResourceLimitsForWire(resource_limits) }; };
  const rawDraftForWire = (parsed) => { const next = { ...draft, ...parsed }; if (parsed.pre_launch === undefined) delete next.pre_launch; else next.pre_launch = sandboxPreLaunchForWire(parsed.pre_launch); return next; };
  const applyRaw = () => { try { const parsed = parseRaw(); setDraft((value) => { const next = { ...value, ...parsed, filesystem: sandboxFilesystemEditorRows(parsed.filesystem, parsed.filesystem_spellings) }; if (parsed.pre_launch === undefined) delete next.pre_launch; else next.pre_launch = sandboxPreLaunchEditorRows(parsed.pre_launch, value.pre_launch); return next; }); state.error.value = ''; return true; } catch (error) { state.error.value = error.message || String(error); return false; } };
  const toggleAdvanced = () => { if (advanced && !applyRaw()) return; if (!advanced) { const wire = sandboxFilesystemWire(draft, baseline); setRawFS(JSON.stringify(wire.filesystem, null, 2)); setRawSpellings(JSON.stringify(wire.filesystem_spellings, null, 2)); setRawEnv(JSON.stringify(draft.environment, null, 2)); setRawIncludes(JSON.stringify(draft.includes, null, 2)); setRawAgentDirs(JSON.stringify(draft.agent_directories, null, 2)); setRawNetwork(JSON.stringify(draft.network, null, 2)); setRawSockets(JSON.stringify(draft.unix_sockets, null, 2)); setRawResources(JSON.stringify(sandboxResourceLimitsForWire(draft.resource_limits), null, 2)); setRawPreLaunch(JSON.stringify(sandboxPreLaunchForWire(draft.pre_launch || []), null, 2)); } setAdvanced(!advanced); };
  const submit = async () => {
    let value = { ...draft, ...sandboxFilesystemWire(draft, baseline), resource_limits: sandboxResourceLimitsForWire(draft.resource_limits), ...(draft.pre_launch === undefined ? {} : { pre_launch: sandboxPreLaunchForWire(draft.pre_launch) }) };
    if (advanced) { try { value = rawDraftForWire(parseRaw()); } catch (error) { state.error.value = error.message || String(error); return; } }
    await actions.saveSandbox({ draft: value, original: seed, options });
  };
  useEffect(() => {
    if (wasSaving.current && !saving) queueMicrotask(() => {
      const button = submitRef.current;
      if (button?.isConnected && !button.disabled && !button.closest('[inert]')) button.focus();
    });
    wasSaving.current = saving;
  }, [saving]);
  useEffect(() => { if (advanced) return undefined; let active = true; const generation = ++directoryGeneration.current; const filesystem = clone(draft.filesystem); const timer = setTimeout(async () => { try { const result = await actions.inspectDirectories(filesystem); if (active && generation === directoryGeneration.current) setDirectoryStatus({ missing: result?.missing || [], creatable: result?.creatable || [] }); } catch (_) { if (active && generation === directoryGeneration.current) setDirectoryStatus({ missing: [], creatable: [] }); } }, 300); return () => { active = false; clearTimeout(timer); }; }, [advanced, filesystemSignature]);
  let predictionDraft = { ...draft, ...sandboxFilesystemWire(draft, baseline), resource_limits: sandboxResourceLimitsForWire(draft.resource_limits), ...(draft.pre_launch === undefined ? {} : { pre_launch: sandboxPreLaunchForWire(draft.pre_launch) }) };
  let predictionDraftError = '';
  if (advanced) {
    try { predictionDraft = rawDraftForWire(parseRaw()); }
    catch (error) { predictionDraftError = message(error); }
  }
  const authoredNetwork = sandboxNetworkAuthoring(
    advanced && !predictionDraftError ? predictionDraft : draft,
  );
  const knownNetworkPacks = new Set((commonRules.network_packs || []).map((pack) => pack.id));
  const hiddenNetworkPacks = authoredNetwork.baseline === 'inherit' ? [] : [
    ...new Set([...authoredNetwork.packs, ...authoredNetwork.deny_packs]
      .filter((id) => !knownNetworkPacks.has(id))),
  ];
  const networkPackVisibilityError = hiddenNetworkPacks.length
    ? `Saving is paused because release-owned network intent cannot be displayed for: ${hiddenNetworkPacks.join(', ')}.${commonRuleFeedBusy ? ' The pack catalog is still loading.' : commonRuleFeedError ? ` Catalog error: ${commonRuleFeedError}` : ' Retry the common-rule catalog.'}`
    : '';
  const accessErrors = [...sandboxAccessDraftErrors(draft), ...sandboxResourceLimitErrors(draft.resource_limits)];
  const preLaunchValidation = sandboxPreLaunchValidation(sandboxPreLaunchForWire(draft.pre_launch || []));
  // Raw JSON is authoritative while Advanced is open, so a repaired raw axis
  // can resume preview even if the hidden structured draft remains invalid.
  const predictionAccessErrors = predictionDraftError ? [] : advanced
    ? [
      ...sandboxAccessDraftErrors(predictionDraft), ...sandboxResourceLimitErrors(predictionDraft.resource_limits),
      ...sandboxPreLaunchValidation(predictionDraft.pre_launch || []).errors,
      ...(networkPackVisibilityError ? [networkPackVisibilityError] : []),
    ]
    : [
      ...accessErrors, ...preLaunchValidation.errors,
      ...(networkPackVisibilityError ? [networkPackVisibilityError] : []),
    ];
  const predictionPauseReason = predictionDraftError || predictionAccessErrors[0] || '';
  const predictionPaused = !!predictionDraft.name.trim() && !!predictionPauseReason;
  const evaluationImplementations = sandboxEvaluationImplementations(
    evaluateHarness, evaluatePlatform, descriptor.catalog,
  );
  const evaluationTarget = sandboxEvaluationTarget(
    evaluateHarness, evaluateImplementation, evaluatePlatform,
  );
  const predictionSignature = JSON.stringify([
    predictionDraftError ? null : predictionDraft, evaluationTarget, options.group || '',
    predictionPauseReason,
  ]);
  useEffect(() => {
    if (typeof actions.predictSandbox !== 'function') return undefined;
    if (predictionDraftError || !predictionDraft.name.trim() || predictionAccessErrors.length > 0) {
      setPrediction(null); setPredictionError(''); setPredictionBusy(false);
      return undefined;
    }
    let active = true;
    setPredictionBusy(true);
    const targets = evaluationTarget ? [evaluationTarget] : [];
    const timer = setTimeout(() => {
      actions.predictSandbox(predictionDraft, targets, { group: options.group || '' }).then((value) => {
        if (!active) return;
        setPrediction(value); setPredictionError(''); setPredictionBusy(false); setEffectiveContext((index) => Math.min(index, Math.max(0, (value.contexts || []).length - 1)));
      }).catch((error) => {
        if (!active) return;
        setPrediction(null); setPredictionError(message(error)); setPredictionBusy(false);
      });
    }, 300);
    return () => { active = false; clearTimeout(timer); };
  }, [predictionSignature]);
  const createMissing = async () => { const filesystem = clone(draft.filesystem); const signature = JSON.stringify(filesystem); const generation = ++directoryGeneration.current; setDirectoryBusy(true); state.error.value = ''; try { const result = await actions.createDirectories(filesystem); const refreshed = await actions.inspectDirectories(filesystem); if (generation === directoryGeneration.current && signature === latestFilesystem.current) { const created = result?.created || []; state.error.value = `Created ${created.length} sandbox director${created.length === 1 ? 'y' : 'ies'}.`; setDirectoryStatus({ missing: refreshed?.missing || [], creatable: refreshed?.creatable || [] }); } } catch (error) { if (generation === directoryGeneration.current) state.error.value = error.message || String(error); } finally { setDirectoryBusy(false); } };
  const configureWithAgent = () => {
    let value = { ...draft, ...sandboxFilesystemWire(draft, baseline), resource_limits: sandboxResourceLimitsForWire(draft.resource_limits), ...(draft.pre_launch === undefined ? {} : { pre_launch: sandboxPreLaunchForWire(draft.pre_launch) }) };
    if (advanced) {
      try { value = rawDraftForWire(parseRaw()); }
      catch (error) { state.error.value = error.message || String(error); return; }
    }
    const handoffErrors = [
      ...sandboxAccessDraftErrors(value),
      ...sandboxResourceLimitErrors(value.resource_limits),
      ...sandboxPreLaunchValidation(value.pre_launch || []).errors,
      ...(networkPackVisibilityError ? [networkPackVisibilityError] : []),
    ];
    if (handoffErrors.length) { state.error.value = handoffErrors[0]; return; }
    const editExisting = options.editExisting ?? !!seed;
    const targetName = editExisting ? options.targetName || seed?.name || '' : '';
    state.closeDialog();
    void actions.configureSandboxWithAgent(value, { targetName, editExisting, cloneSourceName: options.cloneSourceName, onCreate: options.onCreate });
  };
  const structuredFilesystemWire = sandboxFilesystemWire(draft, baseline);
  const rawDirty = advanced && [rawFS !== JSON.stringify(structuredFilesystemWire.filesystem, null, 2), rawSpellings !== JSON.stringify(structuredFilesystemWire.filesystem_spellings, null, 2), rawEnv !== JSON.stringify(draft.environment, null, 2), rawIncludes !== JSON.stringify(draft.includes, null, 2), rawAgentDirs !== JSON.stringify(draft.agent_directories, null, 2), rawNetwork !== JSON.stringify(draft.network, null, 2), rawSockets !== JSON.stringify(draft.unix_sockets, null, 2), rawResources !== JSON.stringify(sandboxResourceLimitsForWire(draft.resource_limits), null, 2), rawPreLaunch !== JSON.stringify(sandboxPreLaunchForWire(draft.pre_launch || []), null, 2)].some(Boolean);
  // A preset inserts ordinary deny rows and then forgets it ever existed: no
  // stored ID, no hidden state. Paths already present in the table are left
  // exactly as authored rather than silently re-denied, and the notice says so.
  // The running set also absorbs an entry whose own paths alias each other,
  // which no audited entry does today — if one ever did, the notice's skip
  // count would need to distinguish that from "already in the table".
  const addCommonRule = (entry) => {
    const paths = commonRulePaths(entry);
    const existing = new Set(draft.filesystem.map((row) => pathIdentity(row.path, commonRules.home)).filter(Boolean));
    const added = [];
    for (const path of paths) {
      const identity = pathIdentity(path, commonRules.home);
      if (!identity || existing.has(identity)) continue;
      existing.add(identity);
      added.push(path);
    }
    if (added.length) setDraft((value) => ({ ...value, filesystem: [...value.filesystem, ...added.map((path) => ({ path, access: 'deny' }))] }));
    setCommonRuleNotice({ label: entry.label || entry.id, added, skipped: paths.length - added.length, warning: entry.warning || '' });
  };
  const globalFilesystem = commonRules.global_filesystem || [];
  const visibleGlobalFilesystem = globalFilesystemForHarness(globalFilesystem, globalHarnessFilter);
  const globalConfigWarnings = commonRules.global_config_warnings || [];
  // Same guard as the Save button, so the hotkey can never reach a save the
  // mouse path refuses.
  const selectedEffective = prediction?.contexts?.[effectiveContext] || null;
  const constructedRootWarning = sandboxConstructedRootWarning(prediction, effectiveContext);
  const effectivePolicyAttention = !!selectedEffective
    && (prediction?.targets || []).some((target) =>
      sandboxPolicyNeedsAttention(target, selectedEffective, effectiveContext));
  const submitBlocked = saving || directoryBusy || !!networkPackVisibilityError
    || (!advanced && (accessErrors.length > 0 || preLaunchValidation.errors.length > 0));
  return html`<${Overlay} id="sandbox-profile-editor-modal" labelledby="sandbox-profile-editor-title" onClose=${state.closeDialog} onSubmitHotkey=${submitBlocked ? null : submit} dirty=${dirty || rawDirty} blocked=${saving || directoryBusy} confirmDiscard=${confirmDiscard} registerClose=${registerClose} resizeKey="tclaude.dash.modalSize.sandbox-profile-editor"><h3 id="sandbox-profile-editor-title">${options.cloneSourceName ? wizWord(`Clone sandbox profile: ${options.cloneSourceName}`, `Mirror ward: ${options.cloneSourceName}`) : seed ? wizWord(`Edit sandbox profile: ${seed.name}`, `Edit ward: ${seed.name}`) : wizWord('New sandbox profile', 'New ward')}</h3><${Row} label="Name"><input value=${draft.name} onInput=${(event) => change(setDraft, 'name', event.currentTarget.value)} placeholder="e.g. shared-build-caches" autofocus autocomplete="off" spellcheck="false"/></${Row}>
    ${!advanced && html`<${NetworkAccessEditor} draft=${draft} setDraft=${setDraft} catalog=${commonRules} newDraft=${!seed}
      packVisibilityError=${networkPackVisibilityError}
      packVisibilityAttention=${!!networkPackVisibilityError && commonRuleFeedSettled}
      retryPackCatalog=${loadCommonRules}
      packCatalogBusy=${commonRuleFeedBusy}/><${SocketAccessEditor} draft=${draft} setDraft=${setDraft} catalog=${commonRules} notice=${socketTemplateNotice} setNotice=${setSocketTemplateNotice}/>`}
    ${constructedRootWarning && html`<div class="sbx-constructed-root-warning" role="alert">
      <strong>⚠ Separate filesystem root</strong>
      <div>${constructedRootWarning.reasons.length
        ? `${constructedRootWarning.reasons.join(' and ')} ${constructedRootWarning.reasons.length === 1 ? 'requires' : 'require'}`
        : 'This resolved sandbox policy requires'} a separate, minimal filesystem root on ${constructedRootWarning.targets.join('; ')}.</div>
      <div>Only the fixed read-only OS/runtime surface and filesystem paths explicitly mounted below remain visible. Home-installed tools elsewhere may disappear unless you grant their paths.</div>
    </div>`}
    <${SandboxSection} id="sandbox-profile-editor-filesystem-section" label="Filesystem"
      help=${FILESYSTEM_HELP} hidden=${advanced}
      attention=${globalConfigWarnings.length > 0 || !!commonRuleFeedError}
      entryCount=${draft.filesystem.length}>
      <div class="sbx-resource-fields"><label>Filesystem root <select id="sandbox-profile-editor-filesystem-root" value=${draft.filesystem_root || ''} onChange=${(event) => setDraft((value) => ({ ...value, filesystem_root: event.currentTarget.value }))}>
        <option value="">Automatic (from other rules)</option>
        <option value="inherit">Inherit host filesystem root</option>
        <option value="separate">Separate/minimal filesystem root</option>
      </select></label></div>
      <div class="sbx-resource-intro">A separate root exposes only the fixed read-only OS/runtime surface and the directory mounts in this policy. Network or Unix-socket restrictions can still require it even when “inherit” is selected. Explicit separation currently requires Linux tclaude-layer.</div>
      ${(globalFilesystem.length > 0 || globalConfigWarnings.length > 0) && html`<div class="sbx-global-filesystem">
        <div class="sbx-global-controls"><label class="sbx-global-toggle" title="These read-only rows come from Claude Code and Codex global sandbox config. They are launch context, not part of the named profile."><input id="sandbox-profile-editor-show-global-filesystem" type="checkbox" checked=${showGlobalFilesystem} onChange=${(event) => setShowGlobalFilesystem(event.currentTarget.checked)}/> Show inherited global config rules${globalFilesystem.length ? ` (${globalFilesystem.length})` : ''}</label>
          ${showGlobalFilesystem && globalFilesystem.length > 0 && html`<label class="sbx-global-filter" for="sandbox-profile-editor-global-harness-filter">Builtins <select id="sandbox-profile-editor-global-harness-filter" value=${globalHarnessFilter} onChange=${(event) => setGlobalHarnessFilter(event.currentTarget.value)}><option value="both">Claude + Codex</option><option value="claude">Claude only</option><option value="codex">Codex only</option><option value="none">None</option></select></label>`}
        </div>
        ${showGlobalFilesystem && visibleGlobalFilesystem.length > 0 && html`<div id="sandbox-profile-editor-global-filesystem" class="sbx-rows sbx-global-rows">
          ${visibleGlobalFilesystem.map((row, index) => { const tooltip = globalFilesystemRuleTooltip(row); return html`<div key=${`${row.path}:${row.access}:${index}`} class="sbx-row sbx-global-row" role="group" title=${tooltip} aria-label=${tooltip}><span class=${`sbx-access sbx-global-access sbx-global-access-${row.access}`}>${globalFilesystemAccessLabel(row.access)}</span><input class="sbx-path" value=${row.path || ''} readonly aria-readonly="true" tabindex="-1"/><span class="sbx-global-harness">${globalFilesystemHarnessLabel(row.harnesses)}</span></div>`; })}
        </div>`}
        ${globalConfigWarnings.map((warning, index) => html`<div key=${index} class="sbx-global-warning" role="status">⚠ ${warning}</div>`)}
      </div>`}
      <div class="sbx-rows">${draft.filesystem.map((row, index) => { const denied = (row.access || 'read') === 'deny'; const mountPath = (row.mount_path || '').trim(); const mountOpen = mountPathRow === index; const mountInvalid = denied && !!mountPath; const hostPath = row._resolved_path || row.path || 'the host directory'; const mountHelpID = `sandbox-profile-editor-mount-help-${index}`; const mountHelp = `The agent sees this path instead of the host path; ${hostPath} is not visible inside the sandbox at all. Leave it empty to expose the directory at its own location. Linux tclaude-layer or stacked only: mounting a directory somewhere else needs a mount namespace. macOS Seatbelt and harness-builtin sandboxes cannot do it and refuse the launch — they never fall back to exposing the host path instead.`; const mountHelpContent = html`<span>The agent sees this path instead of the host path; <strong>${hostPath} is not visible inside the sandbox at all</strong>. Leave it empty to expose the directory at its own location.</span><br/><span><strong>Linux tclaude-layer or stacked only.</strong> Mounting a directory somewhere else needs a mount namespace. macOS Seatbelt and harness-builtin sandboxes cannot do it and refuse the launch — they never fall back to exposing the host path instead.</span>`; const mountLabel = mountInvalid ? `A deny rule must not carry a mount path; this profile will be refused until ${mountPath} is removed` : denied ? 'A deny always applies to the host path' : mountPath ? `Mounts inside the sandbox at ${mountPath}` : 'Mount at a different sandbox path'; return html`<div key=${index} class="sbx-row sbx-filesystem-row"><${SegmentedControl} className="sbx-access sbx-filesystem-access" label=${`Filesystem row ${index + 1} access`} value=${row.access || 'read'} onChange=${(access) => setFS(index, { access })} options=${[['read', 'Read'], ['write', 'Write'], ['deny', 'Deny']]}/><span class="sbx-path-binding"><input class="sbx-path" value=${row.path || ''} onInput=${(event) => setFS(index, { path: event.currentTarget.value })}/>${row._resolved_path && html`<span class="sbx-binding-target">binds → ${row._resolved_path}${row._spellings?.length > 1 ? ` · also retained: ${row._spellings.slice(1).join(', ')}` : ''}</span>`}</span><button type="button" onClick=${async () => { const result = await pickDirectory({ startDir: row.path || '', title: 'Select a sandbox directory' }); if (result.path) setFS(index, { path: result.path }); else if (result.error) state.error.value = result.error; }}>Browse…</button>${/* The SET state is deliberately loud. The value itself lives in a popover, so this glyph is the only signal a reviewer scrolling the table gets that the row projects its directory somewhere else; a quiet toggle would let a remapped row read as an ordinary same-path grant. */ ''}<button type="button" class=${`sbx-mount-btn${mountPath && !denied ? ' is-set' : ''}${mountInvalid ? ' is-invalid' : ''}`} id=${`sandbox-profile-editor-mount-toggle-${index}`} disabled=${denied} aria-expanded=${denied ? undefined : mountOpen} aria-controls=${mountOpen && !denied ? `sandbox-profile-editor-mount-panel-${index}` : undefined} aria-label=${`Filesystem row ${index + 1}: ${mountLabel}`} title=${mountLabel} onClick=${() => { setMountHelpOpen(''); setMountPathRow(mountOpen ? -1 : index); }}>⤳</button><button type="button" onClick=${() => { setMountHelpOpen(''); setMountPathRow(-1); setDraft((value) => ({ ...value, filesystem: value.filesystem.filter((_, i) => i !== index) })); }}>×</button></div>${mountOpen && !denied && html`<div key=${`mount-${index}`} class="sbx-mount-panel" id=${`sandbox-profile-editor-mount-panel-${index}`} onKeyDown=${(event) => { if (event.key !== 'Escape') return; /* Close just this panel. Without it Escape reaches the Overlay and starts the whole modal's discard-confirm flow, which is a wildly disproportionate answer to dismissing one popover. */ event.stopPropagation(); event.preventDefault(); setMountHelpOpen(''); setMountPathRow(-1); document.getElementById(`sandbox-profile-editor-mount-toggle-${index}`)?.focus(); }}><div class="sbx-mount-title">Mount inside the sandbox at<${SandboxHelp}><${HelpDisclosure} id=${mountHelpID} label="Mount path" help=${mountHelp} content=${mountHelpContent} open=${mountHelpOpen === mountHelpID} setOpen=${setMountHelpOpen}/></${SandboxHelp}></div><div class="sbx-mount-row"><input class="sbx-path" value=${row.mount_path || ''} placeholder="/data" ref=${(node) => { /* Preact does not implement autofocus, and the document's autofocus-processed flag is already set by the modal's name field, so the browser will not honor it either. Focus the field explicitly when the panel first mounts. */ if (node && !node.dataset.focused) { node.dataset.focused = '1'; node.focus(); } }} onInput=${(event) => setFS(index, { mount_path: event.currentTarget.value })}/><button type="button" disabled=${!mountPath} onClick=${() => setFS(index, { mount_path: '' })}>clear</button></div></div>`}`; })}</div><button type="button" class="sbx-add-row" onClick=${() => setDraft((value) => ({ ...value, filesystem: [...value.filesystem, { path: '', access: 'read' }] }))}>＋ add directory</button>
      ${/* `|| null` rather than a bare boolean: where `open` is not a settable
           DOM property, Preact falls back to setAttribute, and setting it to
           `false` still leaves the attribute present (i.e. open). null removes
           it on both paths. */ ''}
      <details class="sbx-common-rules" id="sandbox-profile-editor-common-rules" open=${commonRulesOpen || null}
        onToggle=${(event) => setCommonRulesOpen(event.currentTarget.open)}>
        ${/* The menu ships folded, and nothing inside a closed <details> is in
             the accessibility tree — so a feed failure has to be legible on the
             summary itself or an operator never learns the presets are gone. */ ''}
        <summary class="sbx-common-rule-summary">＋ add common rule${commonRuleFeedError ? ' — unavailable' : ''}</summary>
        <div class="sbx-common-rule-intro">Audited presets for locations most profiles want denied. Each one inserts ordinary deny rows into the table above — visible, editable, and yours to adjust or remove afterwards. Nothing else is stored.</div>
        ${commonRuleFeedError && html`<div id="sandbox-profile-editor-common-rule-feed-error" class="sbx-common-rule-feed-error" role="alert">Could not load the common-rule catalog: ${commonRuleFeedError} <button type="button" onClick=${loadCommonRules}>${commonRuleFeedBusy ? 'retrying…' : 'retry'}</button></div>`}
        <div class="sbx-common-rule-list">${(commonRules.categories || []).map((entry) => html`<${CommonRuleEntry} key=${entry.id} entry=${entry} onAdd=${addCommonRule}/>`)}</div>
        ${(commonRules.informational || []).length > 0 && html`<details class="sbx-common-rule-informational"><summary>Required, non-removable access</summary>${(commonRules.informational || []).map((entry) => html`<div key=${entry.id} class="sbx-rule-note"><strong>${entry.label}:</strong> ${entry.description}</div>`)}</details>`}
      </details>
      ${commonRuleNotice && html`<div id="sandbox-profile-editor-common-rule-notice" class="sbx-common-rule-notice" role="status">
        <span>${commonRuleNotice.added.length ? `Added ${commonRuleNotice.added.length} deny row${commonRuleNotice.added.length === 1 ? '' : 's'} from “${commonRuleNotice.label}”: ${commonRuleNotice.added.join(' · ')}.` : `“${commonRuleNotice.label}” added no rows.`}${commonRuleNotice.skipped ? ` ${commonRuleNotice.skipped} path${commonRuleNotice.skipped === 1 ? ' was' : 's were'} already in the table and left as authored.` : ''}</span>
        ${commonRuleNotice.warning ? html`<span class="sbx-common-rule-warn">⚠ ${commonRuleNotice.warning}</span>` : null}
        <button type="button" class="sbx-common-rule-dismiss" aria-label="Dismiss common-rule notice" onClick=${() => setCommonRuleNotice(null)}>×</button>
      </div>`}
    </${SandboxSection}>
    <${SandboxSection} id="sandbox-profile-editor-resource-limits-section" label="Resource limits — Linux only"
      help="Optional hard cgroup-v2 ceilings for the aggregate managed agent workload. Child test and build processes share the same budget. Linux harness-builtin, tclaude-layer, and stacked launches can enforce them; macOS and sandbox implementation off cannot. Blank fields preserve the existing launch path and do not probe cgroups."
      hidden=${advanced} entryCount=${Number(!!draft.resource_limits.memory) + Number(String(draft.resource_limits.cpu ?? '').trim() !== '')}>
      <div class="sbx-resource-intro"><strong>Linux only.</strong> Limits cover the harness and all descendant test/build workers. Generic host-memory views such as <code>/proc/meminfo</code> may still show total host RAM.</div>
      <div class="sbx-resource-fields">
        <label>Memory <input id="sandbox-profile-editor-memory-limit" value=${draft.resource_limits.memory} placeholder="e.g. 4GiB or 512MB" autocomplete="off" spellcheck="false" onInput=${(event) => setDraft((value) => ({ ...value, resource_limits: { ...value.resource_limits, memory: event.currentTarget.value } }))}/></label>
        <label>CPU cores <input id="sandbox-profile-editor-cpu-limit" value=${draft.resource_limits.cpu} placeholder="e.g. 0.5 or 2" inputmode="decimal" autocomplete="off" spellcheck="false" onInput=${(event) => setDraft((value) => ({ ...value, resource_limits: { ...value.resource_limits, cpu: event.currentTarget.value } }))}/></label>
      </div>
    </${SandboxSection}>
    ${descriptor.sandboxImpl?.platform === 'darwin' && html`<${SandboxSection} id="sandbox-profile-editor-compatibility-section" label="Compatibility — macOS only"
      help="Optional exceptions for software that cannot start under the default tclaude Seatbelt profile. These settings apply only to the tclaude-layer sandbox; Claude Code and Codex own their built-in Seatbelt profiles."
      hidden=${advanced} entryCount=${Number(draft.darwin_allow_mach_register)}>
      <label class="sbx-global-toggle"><input id="sandbox-profile-editor-allow-mach-register" type="checkbox" checked=${draft.darwin_allow_mach_register} onChange=${(event) => setDraft((value) => ({ ...value, darwin_allow_mach_register: event.currentTarget.checked }))}/> Allow Mach service registration</label>
      <div class="sbx-resource-intro">Needed by multi-process browser rendering such as headless Chrome/Chromium and Playwright WebKit on macOS. This adds <code>(allow mach-register)</code> to tclaude's Seatbelt profile and broadens the processes' ability to register Mach services.</div>
    </${SandboxSection}>`}
    <${SandboxSection} id="sandbox-profile-editor-environment-section" label="Environment"
      help=${ENVIRONMENT_HELP} hidden=${advanced} entryCount=${draft.environment.length}><div class="sbx-rows">${draft.environment.map((row, index) => html`<div key=${index} class="sbx-row sbx-environment-row"><input class="sbx-env-name" value=${row.name || ''} placeholder="NAME" onInput=${(event) => setEnv(index, { name: event.currentTarget.value })}/><input class="sbx-env-value" value=${row.value || ''} placeholder="value" onInput=${(event) => setEnv(index, { value: event.currentTarget.value })}/><button type="button" onClick=${() => setDraft((value) => ({ ...value, environment: value.environment.filter((_, i) => i !== index) }))}>×</button></div>`)}</div><button type="button" class="sbx-add-row" onClick=${() => setDraft((value) => ({ ...value, environment: [...value.environment, { name: '', value: '' }] }))}>＋ add variable</button></${SandboxSection}>
    <${SandboxSection} id="sandbox-profile-editor-pre-launch-section" label="Pre-launch scripts"
      help=${PRE_LAUNCH_HELP} hidden=${advanced} entryCount=${(draft.pre_launch || []).length}>
      <${SandboxPreLaunchEditor} blocks=${draft.pre_launch || []} setDraft=${setDraft}
        validation=${preLaunchValidation}/>
    </${SandboxSection}>
    <${SandboxSection} id="sandbox-profile-editor-includes-section" label="Includes"
      help=${INCLUDES_HELP} hidden=${advanced} entryCount=${draft.includes.length}><div class="sbx-rows">${draft.includes.map((name, index) => {
    const missing = !!name && !sandboxProfiles.some((item) => item.name === name);
    const warningID = `sandbox-profile-editor-include-warning-${index}`;
    const options = [
      ...(missing ? [[name, '— missing —']] : []),
      ['', '— choose profile —'],
      ...sandboxProfiles.filter((item) => item.name !== seed?.name || item.name === name).map((item) => [item.name, item.name]),
    ];
    return html`<div key=${index} class="sbx-row sbx-include-row"><${Select} class="sbx-inc-name" value=${name} aria-invalid=${missing || null} aria-describedby=${missing ? warningID : null} onChange=${(value) => setDraft((old) => ({ ...old, includes: old.includes.map((item, i) => i === index ? value : item) }))} options=${options} />${missing && html`<span id=${warningID} class="sbx-global-warning sbx-include-warning" role="alert">⚠ "${name}" not found in registry</span>`}<button type="button" onClick=${() => setDraft((old) => ({ ...old, includes: old.includes.filter((_, i) => i !== index) }))}>×</button></div>`;
  })}</div><button type="button" class="sbx-add-row sbx-include-add" onClick=${() => setDraft((old) => ({ ...old, includes: [...old.includes, ''] }))}>＋ include profile</button></${SandboxSection}>
    <${SandboxSection} id="sandbox-profile-editor-agent-directories-section" label="Agent-owned directories"
      help=${AGENT_DIRECTORIES_HELP} hidden=${advanced} entryCount=${draft.agent_directories.length}><div class="sbx-rows">${draft.agent_directories.map((name, index) => html`<div key=${index} class="sbx-row"><input class="sbx-agent-name" value=${name} placeholder="GOCACHE" onInput=${(event) => setDraft((old) => ({ ...old, agent_directories: old.agent_directories.map((item, i) => i === index ? event.currentTarget.value : item) }))}/><button type="button" onClick=${() => setDraft((old) => ({ ...old, agent_directories: old.agent_directories.filter((_, i) => i !== index) }))}>×</button></div>`)}</div><button type="button" class="sbx-add-row sbx-agent-add" onClick=${() => setDraft((old) => ({ ...old, agent_directories: [...old.agent_directories, ''] }))}>＋ add agent-owned directory</button></${SandboxSection}>
    <${SandboxSection} id="sandbox-profile-editor-effective-policy-section"
      className="sbx-effective-preview" label="Effective policy preview"
      help=${EFFECTIVE_POLICY_HELP} hidden=${advanced}
      attention=${!!predictionError || (selectedEffective?.notices || []).length > 0
        || effectivePolicyAttention}>
      <div class="sbx-evaluation-target-controls">
        <label title=${EVALUATION_TARGET_TITLE}>Agent harness <select id="sandbox-profile-editor-evaluate-harness" value=${evaluateHarness} onChange=${(event) => {
          const nextHarness = event.currentTarget.value;
          const implementations = sandboxEvaluationImplementations(
            nextHarness, evaluatePlatform, descriptor.catalog,
          );
          setEvaluateHarness(nextHarness);
          if (!implementations.some(([value]) => value === evaluateImplementation)) {
            setEvaluateImplementation(implementations[0]?.[0] || 'harness-builtin');
          }
        }}>
          <option value="">${RESOLVED_DEFAULTS_LABEL}</option>
          ${evaluationHarnesses.map((entry) => html`
            <option value=${entry.name}>${entry.display_name || entry.name}</option>
          `)}
        </select></label>
        <label title=${EVALUATION_TARGET_TITLE}>Sandbox implementation <select id="sandbox-profile-editor-evaluate-implementation" value=${evaluateImplementation} disabled=${!evaluateHarness} onChange=${(event) => setEvaluateImplementation(event.currentTarget.value)}>
          ${evaluateHarness
    ? evaluationImplementations.map(([value, label]) => html`<option value=${value}>${label}</option>`)
    : html`<option value="harness-builtin">${RESOLVED_DEFAULTS_LABEL}</option>`}
        </select></label>
        <label title=${EVALUATION_TARGET_TITLE}>Operating system <select id="sandbox-profile-editor-evaluate-platform" value=${evaluatePlatform} disabled=${!evaluateHarness} onChange=${(event) => {
          const nextPlatform = event.currentTarget.value;
          const implementations = sandboxEvaluationImplementations(
            evaluateHarness, nextPlatform, descriptor.catalog,
          );
          setEvaluatePlatform(nextPlatform);
          if (!implementations.some(([value]) => value === evaluateImplementation)) {
            setEvaluateImplementation(implementations[0]?.[0] || 'harness-builtin');
          }
        }}>
          ${evaluateHarness ? html`
            <option value="linux">Linux</option>
            <option value="darwin">macOS</option>
          ` : html`<option value="linux">${RESOLVED_DEFAULTS_LABEL} (this host)</option>`}
        </select></label>
      </div>
      ${predictionPaused && html`<div class="sbx-preview-status">Effective policy preview paused: ${predictionPauseReason}</div>`}
      ${predictionBusy && html`<div class="sbx-preview-status">Evaluating draft…</div>`}
      ${predictionError && html`<div class="sbx-preview-error" role="alert">Could not evaluate draft: ${predictionError}</div>`}
      ${(prediction?.contexts?.length || 0) > 1 && html`<label>Rules for <select id="sandbox-profile-editor-effective-context" value=${effectiveContext} onChange=${(event) => setEffectiveContext(Number(event.currentTarget.value))}>${prediction.contexts.map((context, index) => html`<option value=${index}>${context.context.group_name ? `group ${context.context.group_name}` : context.context.global === draft.name ? 'global assignment' : 'explicit selection'}</option>`)}</select></label>`}
      ${/* The composed layers stay on screen rather than folding into the
           details below: which sandbox profile occupies which scope is the one
           thing the rule buckets never restate, and it is what separates
           "composed sandbox policy" from the launch-parameter defaults the
           target controls above resolve. */ ''}
      ${/* role="status" because the "Rules for" selector above swaps this row's
           contents in place; without it the one statement of which profiles
           compose the shown rules changes silently for a screen reader. */ ''}
      ${selectedEffective && html`<div class="sbx-policy-layers" id="sandbox-profile-editor-policy-layers"
        role="status" aria-live="polite" aria-atomic="true">
        <strong>${SANDBOX_PROFILE_LAYERS_LABEL}:</strong> ${sandboxProfileLayersText(selectedEffective.context, 'this draft alone — no global or group sandbox profile applies')}
      </div>`}
      ${selectedEffective && prediction?.targets?.map((target, index) => html`<${SandboxPolicyResult} key=${index} target=${target} context=${selectedEffective} contextIndex=${effectiveContext} contexts=${prediction.contexts}/>`)}
      ${(selectedEffective?.notices || []).length > 0 && html`<div class="sbx-a11y-status" role="status" aria-live="polite" aria-atomic="true">Policy composition warning: ${selectedEffective.notices.map((notice) => notice.detail).join('. ')}</div>`}
      ${selectedEffective && html`<details class="sbx-composition-details"><summary>How these rules were combined</summary>
        ${/* The layer list itself is the always-visible row above; this
             disclosure carries only the rule that explains it. */ ''}
        <div>${SANDBOX_PROFILE_COMPOSITION}</div>
        ${prediction?.remaining_contexts ? html`<div class="sbx-preview-status">Showing 10 assignments; ${prediction.remaining_contexts} more are omitted from this selector but still included in the overall safety check.</div>` : null}
      </details>`}
      ${(selectedEffective?.notices || []).map((notice, index) => html`<div key=${index} class="sbx-composition-warning">⚠ ${notice.detail}</div>`)}
    </${SandboxSection}>
    ${!advanced && accessErrors.map((error, index) => html`<div key=${index} class="sbx-access-validation" role="alert">⚠ ${error}</div>`)}
    ${!advanced && directoryStatus.missing.length > 0 && html`<div class="sbx-missing"><span>${directoryStatus.missing.length} director${directoryStatus.missing.length === 1 ? 'y does' : 'ies do'} not exist. Saving is allowed; read/write rules activate on a later launch, while deny targets must exist before launch.</span>${directoryStatus.creatable.length > 0 && html`<button type="button" disabled=${directoryBusy || saving} onClick=${createMissing}>${directoryBusy ? 'Creating…' : `Create ${directoryStatus.creatable.length} missing director${directoryStatus.creatable.length === 1 ? 'y' : 'ies'}`}</button>`}</div>`}
    <button type="button" class="sbx-advanced-toggle" aria-expanded=${advanced} onClick=${toggleAdvanced}>${advanced ? '▾' : '▸'} Advanced — edit raw JSON</button>${advanced && html`<div class="sbx-advanced-body"><${Row} label="Filesystem JSON"><textarea id="sandbox-profile-editor-filesystem" rows="6" value=${rawFS} onInput=${(event) => setRawFS(event.currentTarget.value)}/></${Row}><${Row} label="Filesystem spellings JSON"><textarea id="sandbox-profile-editor-filesystem-spellings" rows="6" value=${rawSpellings} onInput=${(event) => setRawSpellings(event.currentTarget.value)}/></${Row}><${Row} label="Environment JSON"><textarea id="sandbox-profile-editor-environment" rows="6" value=${rawEnv} onInput=${(event) => setRawEnv(event.currentTarget.value)}/></${Row}><${Row} label="Network JSON"><textarea id="sandbox-profile-editor-network" rows="6" value=${rawNetwork} onInput=${(event) => setRawNetwork(event.currentTarget.value)}/></${Row}><${Row} label="Unix sockets JSON"><textarea id="sandbox-profile-editor-unix-sockets" rows="6" value=${rawSockets} onInput=${(event) => setRawSockets(event.currentTarget.value)}/></${Row}><${Row} label="Resource limits JSON"><textarea id="sandbox-profile-editor-resource-limits" rows="3" value=${rawResources} onInput=${(event) => setRawResources(event.currentTarget.value)}/></${Row}><${Row} label="Includes JSON"><textarea id="sandbox-profile-editor-includes" rows="3" value=${rawIncludes} onInput=${(event) => setRawIncludes(event.currentTarget.value)}/></${Row}><${Row} label="Agent dirs JSON"><textarea id="sandbox-profile-editor-agent-directories" rows="3" value=${rawAgentDirs} onInput=${(event) => setRawAgentDirs(event.currentTarget.value)}/></${Row}><${Row} label="Pre-launch scripts JSON"><textarea id="sandbox-profile-editor-pre-launch" rows="8" placeholder='[{"name": "setup", "script": "export FOO=bar\\n", "exports": ["FOO"]}]' value=${rawPreLaunch} onInput=${(event) => setRawPreLaunch(event.currentTarget.value)}/></${Row}></div>`}
    <div role="alert" class="cron-create-error">${state.error.value}</div><div class="modal-buttons"><button disabled=${saving || directoryBusy} onClick=${() => { void requestClose(); }}>Cancel</button><button id="sandbox-profile-editor-scribe" disabled=${saving || directoryBusy} onClick=${configureWithAgent}>🤖 configure with agent</button><span class="spacer"></span><button ref=${submitRef} id="sandbox-profile-editor-submit" class="primary" disabled=${submitBlocked} onClick=${submit}>${saving ? 'Saving…' : 'Save sandbox profile'}</button></div></${Overlay}>`;
}

function ProfileExport({ current, state, actions, confirmDiscard }) {
  const [selected, setSelected] = useState(() => new Set(current.profiles.map((item) => item.name))); const [error, setError] = useState(''); const [busy, setBusy] = useState(false);
  const toggle = (name) => setSelected((old) => { const next = new Set(old); next.has(name) ? next.delete(name) : next.add(name); return next; });
  const submit = async () => { if (!selected.size) { setError('select at least one profile'); return; } setBusy(true); try { await actions.exportProfileBundle([...selected]); state.closeDialog(); } catch (e) { setError(message(e)); } finally { setBusy(false); } };
  return html`<${Overlay} id="profile-export-modal" labelledby="profile-export-title" onClose=${state.closeDialog} confirmDiscard=${confirmDiscard}><h3 id="profile-export-title">Export spawn profiles</h3><div id="profile-export-list" class="profile-transfer-list">${current.profiles.map((item) => html`<label key=${item.name} class="profile-transfer-row"><input type="checkbox" checked=${selected.has(item.name)} onChange=${() => toggle(item.name)}/><span>${item.name} ${profileAliasesLabel(item)} ${profileSummary(item)}</span></label>`)}</div><div role="alert" class="cron-create-error">${error}</div><div class="modal-buttons"><button onClick=${state.closeDialog}>Cancel</button><span class="spacer"></span><button class="primary" disabled=${busy} onClick=${submit}>${busy ? 'Exporting…' : 'Export'}</button></div></${Overlay}>`;
}

function ProfileImportRow({ row, decision, update }) {
  const renameLabel = row.aliases?.length ? 'Rename copy (aliases omitted)' : 'Rename';
  return html`<div class="profile-transfer-row"><input type="checkbox" disabled=${!row.valid} checked=${decision?.include} onChange=${(event) => update({ include: event.currentTarget.checked })}/><span>${row.name}${row.error && ` — ${row.error}`}</span>${row.exists && row.valid && html`<span class="profile-import-conflict"><${Select} value=${decision?.action} onChange=${(value) => update({ action: value })} options=${[['rename', renameLabel], ['overwrite', 'Overwrite']]} />${decision?.action === 'rename' && html`<input value=${decision?.as} onInput=${(event) => update({ as: event.currentTarget.value })}/>`}</span>`}</div>`;
}

function ProfileImport({ state, actions, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const [raw, setRaw] = useState(''); const [envelope, setEnvelope] = useState(null); const [preview, setPreview] = useState(null); const [decisions, setDecisions] = useState({}); const [error, setError] = useState(''); const [busy, setBusy] = useState('');
  const inspect = async () => { setError(''); setBusy('inspect'); try { const parsed = JSON.parse(raw); const found = await actions.inspectProfiles(parsed); setEnvelope(parsed); setPreview(found); const initial = {}; for (const row of found.profiles || []) initial[row.name] = { include: !!row.valid, action: row.exists ? 'rename' : 'create', as: row.default_name || `${row.name}-copy` }; setDecisions(initial); } catch (e) { setError(message(e)); } finally { setBusy(''); } };
  const update = (name, patch) => setDecisions((value) => ({ ...value, [name]: { ...value[name], ...patch } }));
  const submit = async () => { if (!preview) { setError('preview the import first'); return; } setBusy('import'); try { const rows = Object.entries(decisions).map(([name, value]) => ({ name, ...value })); await actions.importProfileBundle(envelope, rows); state.closeDialog(); } catch (e) { setError(message(e)); } finally { setBusy(''); } };
  const dirty = !!raw;
  return html`<${Overlay} id="profile-import-modal" labelledby="profile-import-title" onClose=${state.closeDialog} dirty=${dirty} blocked=${!!busy} confirmDiscard=${confirmDiscard} registerClose=${registerClose}><h3 id="profile-import-title">Import spawn profiles</h3><${Row} label="File"><input type="file" accept=".json,application/json" onChange=${async (event) => { const file = event.currentTarget.files?.[0]; if (file) { setRaw(await file.text()); setPreview(null); } }}/></${Row}><${Row} label="or paste"><textarea rows="6" value=${raw} onInput=${(event) => { setRaw(event.currentTarget.value); setPreview(null); }} /></${Row}><button type="button" class="tool profile-transfer-preview-button" disabled=${busy} onClick=${inspect}>Preview</button>
    ${preview && html`<div id="profile-import-preview" class="profile-transfer-list">${(preview.profiles || []).map((row) => html`<${ProfileImportRow} key=${row.name} row=${row} decision=${decisions[row.name]} update=${(patch) => update(row.name, patch)} />`)}</div>`}
    <div role="alert" class="cron-create-error">${error}</div><div class="modal-buttons"><button disabled=${!!busy} onClick=${() => { void requestClose(); }}>Cancel</button><span class="spacer"></span><button class="primary" disabled=${busy || !preview} onClick=${submit}>${busy === 'import' ? 'Importing…' : 'Import selected'}</button></div></${Overlay}>`;
}

function SandboxExport({ current, state, actions, confirmDiscard }) {
  const [selected, setSelected] = useState(() => new Set(current.sandboxProfiles.map((item) => item.name))); const [error, setError] = useState(''); const [busy, setBusy] = useState(false);
  const toggle = (name) => setSelected((old) => { const next = new Set(old); next.has(name) ? next.delete(name) : next.add(name); return next; });
  const submit = async () => { if (!selected.size) { setError('select at least one sandbox profile'); return; } setBusy(true); try { await actions.exportSandboxBundle([...selected]); state.closeDialog(); } catch (e) { setError(message(e)); } finally { setBusy(false); } };
  return html`<${Overlay} id="sandbox-profile-export-modal" labelledby="sandbox-profile-export-title" onClose=${state.closeDialog} confirmDiscard=${confirmDiscard}><h3 id="sandbox-profile-export-title"><span class="sandbox-word-regular">Export sandbox profiles</span><span class="sandbox-word-wizard">📜 Inscribe wards</span></h3><div class="profile-transfer-list">${current.sandboxProfiles.map((item) => html`<label key=${item.name} class="profile-transfer-row"><input type="checkbox" checked=${selected.has(item.name)} onChange=${() => toggle(item.name)}/><span>${item.name} ${sandboxProfileSummary(item)}</span></label>`)}</div><div role="alert" class="cron-create-error">${error}</div><div class="modal-buttons"><button onClick=${state.closeDialog}>Cancel</button><span class="spacer"></span><button class="primary" disabled=${busy} onClick=${submit}>${busy ? 'Exporting…' : 'Export'}</button></div></${Overlay}>`;
}

function sandboxImportNetworkRows(profile) {
  const network = sandboxNetworkAuthoring(profile);
  const rows = [];
  if (network.baseline && (network.baseline !== 'inherit' || profile.network)) rows.push(`baseline ${network.baseline}${network.baseline === 'inherit' ? '' : ' all'}${network.engine ? ` · ${network.engine} filter` : ''}`);
  for (const pack of network.packs || []) rows.push(`allow pack ${pack}`);
  for (const pack of network.deny_packs || []) rows.push(`deny pack ${pack}`);
  const endpoint = (entry) => {
    const target = entry.host || entry.domain || entry.cidr || (entry.loopback ? 'loopback' : 'unknown destination');
    const suffix = entry.include_subdomains ? ' + subdomains' : '';
    const ports = entry.ports?.length ? `:${entry.ports.join(',')}` : '';
    return `${target}${suffix}${ports}`;
  };
  for (const entry of network.allow || []) rows.push(`allow ${endpoint(entry)}`);
  for (const entry of network.deny || []) rows.push(`deny ${endpoint(entry)}`);
  return rows;
}

function sandboxImportPolicyRows(profile) {
  const rows = [];
  if (profile.filesystem_root) rows.push({ kind: 'root', value: profile.filesystem_root === 'separate' ? 'separate/minimal filesystem root' : 'inherit host filesystem root' });
  for (const entry of profile.filesystem || []) rows.push({ kind: entry.access, value: entry.mount_path ? `${entry.path} → ${entry.mount_path}` : entry.path });
  for (const rule of profile.filesystem_spellings?.rules || []) {
    for (const spelling of rule.spellings || []) rows.push({ kind: 'alias', value: `${spelling} → ${rule.resolved_path}` });
  }
  for (const name of profile.includes || []) rows.push({ kind: 'include', value: name });
  for (const entry of profile.environment || []) rows.push({ kind: 'env', value: `${entry.name} → ${entry.value}` });
  for (const name of profile.agent_directories || []) rows.push({ kind: 'own', value: `${name} — isolated per agent` });
  for (const block of profile.pre_launch || []) rows.push({ kind: 'pre-launch', value: `${block.name}${block.exports?.length ? ` → exports ${block.exports.join(', ')}` : ''}`, script: block.script });
  for (const value of sandboxImportNetworkRows(profile)) rows.push({ kind: 'network', value });
  const sockets = sandboxAccessAxes(profile).unix_sockets;
  if (sockets.mode) rows.push({ kind: 'sockets', value: sockets.mode });
  for (const entry of sockets.allow || []) rows.push({ kind: 'sockets', value: `allow ${entry.path || entry.path_glob}` });
  const limits = profile.resource_limits || {};
  if (limits.memory) rows.push({ kind: 'limit', value: `memory ${limits.memory}` });
  if (limits.cpu != null) rows.push({ kind: 'limit', value: `CPU ${limits.cpu}` });
  if (profile.darwin_allow_mach_register) rows.push({ kind: 'mach', value: 'allow Mach service registration on macOS' });
  return rows;
}

function SandboxImportProfile({ profile, exists, warnings, expanded }) {
  const rows = sandboxImportPolicyRows(profile);
  return html`<details class="sandbox-import-profile" open=${expanded}>
    <summary>
      <span class="sandbox-import-profile-name">${profile.name}</span>
      <span class="sandbox-import-profile-summary">${rows.length} policy ${rows.length === 1 ? 'entry' : 'entries'} · ${sandboxProfileSummary(profile)}</span>
      <span class=${`sandbox-import-status ${exists ? 'is-conflict' : 'is-new'}`}>${exists ? 'Already exists' : 'New'}</span>
    </summary>
    <div class="sandbox-import-policy">
      ${rows.length ? rows.map((row, index) => row.script == null
        ? html`<div key=${`${row.kind}:${row.value}:${index}`} class="sandbox-import-rule"><span class=${`sbx-cap-tag sbx-cap-${row.kind}`}>${row.kind}</span><span class="sbx-cap-val" title=${row.value}>${row.value}</span></div>`
        : html`<details key=${`${row.kind}:${row.value}:${index}`} class="sandbox-import-script"><summary><span class=${`sbx-cap-tag sbx-cap-${row.kind}`}>${row.kind}</span><span>${row.value}</span></summary><pre>${row.script}</pre></details>`
      ) : html`<div class="sbx-caps-empty">No sandbox rules</div>`}
      ${warnings.map((warning, index) => html`<div key=${`${warning.path}:${index}`} class="sandbox-import-warning" role="note">⚠ <span>${warning.path}: ${warning.message}</span></div>`)}
    </div>
  </details>`;
}

function SandboxImport({ current, state, actions, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const [raw, setRaw] = useState(''); const [envelope, setEnvelope] = useState(null); const [preview, setPreview] = useState(null); const [conflict, setConflict] = useState('skip'); const [error, setError] = useState(''); const [busy, setBusy] = useState('');
  const inspect = async () => {
    setError(''); setBusy('inspect');
    try {
      const parsed = JSON.parse(raw);
      if (parsed?.format !== 'tclaude-sandbox-profiles' || ![1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14].includes(parsed?.format_version)) throw new Error('not a tclaude sandbox-profile export');
      const found = await actions.inspectSandboxBundle(parsed);
      setEnvelope(parsed); setPreview(found);
    } catch (e) {
      setError(message(e));
    } finally { setBusy(''); }
  };
  const existing = new Set(current.sandboxProfiles.map((item) => item.name)); const incoming = preview?.profiles || envelope?.profiles || [];
  const conflicts = incoming.filter((item) => existing.has(item.name)).length;
  const imported = conflict === 'skip' ? incoming.length - conflicts : incoming.length;
  const effect = conflict === 'skip'
    ? `${conflicts ? `${conflicts} existing profile${conflicts === 1 ? '' : 's'} will stay unchanged; ` : ''}${imported} new profile${imported === 1 ? '' : 's'} will be imported.`
    : conflict === 'overwrite'
      ? `${conflicts} existing profile${conflicts === 1 ? '' : 's'} will be replaced; ${incoming.length - conflicts} new profile${incoming.length - conflicts === 1 ? '' : 's'} will be imported.`
      : `Import will stop because ${conflicts} profile name${conflicts === 1 ? '' : 's'} already exist${conflicts === 1 ? 's' : ''} locally.`;
  const importLabel = conflict === 'error' && conflicts > 0
    ? 'Resolve name conflict'
    : `Import ${imported} profile${imported === 1 ? '' : 's'}`;
  // The inspect reports include-graph errors PER conflict policy
  // ("skip" keeps a clashing local profile's own includes, so only one policy
  // may be invalid). Importing under "error" only succeeds when no names
  // clash — every incoming profile lands — so it shares the overwrite graph.
  const includeError = preview?.include_errors?.[conflict === 'skip' ? 'skip' : 'overwrite'] || '';
  const submit = async () => {
    if (!preview) { setError('preview the import first'); return; }
    if (includeError) { setError(includeError); return; }
    setBusy('import');
    try { await actions.importSandboxBundle(envelope, conflict); state.closeDialog(); }
    catch (e) { setError(message(e)); }
    finally { setBusy(''); }
  };
  return html`<${Overlay} id="sandbox-profile-import-modal" labelledby="sandbox-profile-import-title" onClose=${state.closeDialog} dirty=${!!raw} blocked=${!!busy} confirmDiscard=${confirmDiscard} registerClose=${registerClose}><h3 id="sandbox-profile-import-title"><span class="sandbox-word-regular">Import sandbox profiles</span><span class="sandbox-word-wizard">📜 Read wards</span></h3><${Row} label="File"><input type="file" accept=".json,application/json" onChange=${async (event) => { const file = event.currentTarget.files?.[0]; if (file) { setRaw(await file.text()); setPreview(null); } }}/></${Row}><${Row} label="or paste"><textarea rows="6" value=${raw} onInput=${(event) => { setRaw(event.currentTarget.value); setPreview(null); }}/></${Row}><button type="button" class="tool profile-transfer-preview-button" disabled=${busy} onClick=${inspect}>${busy === 'inspect' ? 'Previewing…' : 'Preview'}</button>${preview && html`
      <div class="sandbox-import-overview"><span>${incoming.length} profile${incoming.length === 1 ? '' : 's'} in this bundle</span>${conflicts ? html`<span class="sandbox-import-conflict-count">${conflicts} name conflict${conflicts === 1 ? '' : 's'}</span>` : null}</div>
      <div id="sandbox-profile-import-preview" class="sandbox-import-profile-list">${incoming.map((item, index) => html`<${SandboxImportProfile} key=${item.name} profile=${item} exists=${existing.has(item.name)} warnings=${(preview.warnings || []).filter((warning) => warning.profile === item.name)} expanded=${index === 0}/>` )}</div>
      ${conflicts ? html`<${Row} label="Name conflicts"><${Select} id="sandbox-profile-import-conflict" value=${conflict} onChange=${(value) => setConflict(value)} options=${[['skip', 'Skip existing'], ['overwrite', 'Overwrite existing'], ['error', 'Stop with an error']]}/></${Row}><div id="sandbox-profile-import-effect" class=${`sandbox-import-effect${conflict === 'error' ? ' is-error' : ''}`} role="status">${effect}</div>` : null}
      ${includeError && html`<div id="sandbox-profile-import-include-error" role="alert" class="cron-create-error">Include graph invalid under this conflict policy: ${includeError}</div>`}`}
    <div role="alert" class="cron-create-error">${error}</div><div class="modal-buttons"><button disabled=${!!busy} onClick=${() => { void requestClose(); }}>Cancel</button><span class="spacer"></span><button class="primary" disabled=${busy || !preview || !imported || !!includeError || (conflict === 'error' && conflicts > 0)} onClick=${submit}>${busy === 'import' ? 'Importing…' : preview ? importLabel : 'Import'}</button></div></${Overlay}>`;
}

function SandboxDiffModal({ model, close }) {
  const confirmRef = useRef(null);
  const { dialogRef } = useDialogFocus({ open: !!model, initialFocusRef: confirmRef, onEscape: () => close(false) });
  useEffect(() => {
    if (!model) return undefined;
    const editor = document.querySelector('#sandbox-profile-editor-modal');
    const editorDialog = editor?.querySelector('[role="dialog"]');
    if (!editor) return undefined;
    editor.inert = true;
    editor.setAttribute('aria-hidden', 'true');
    editorDialog?.setAttribute('aria-modal', 'false');
    return () => {
      editor.inert = false;
      editor.removeAttribute('aria-hidden');
      editorDialog?.setAttribute('aria-modal', 'true');
    };
  }, [model]);
  if (!model) return null;
  const beforeRaw = model.before ? JSON.stringify(model.before, null, 2) : '';
  const afterRaw = JSON.stringify(model.after, null, 2);
  const diff = model.before ? lineDiff(beforeRaw, afterRaw) : afterRaw.split('\n').map((s) => ({ t: 'add', s }));
  const adds = diff.filter((line) => line.t === 'add').length;
  const dels = diff.filter((line) => line.t === 'del').length;
  const sign = { add: '+', del: '\u2212', ctx: ' ' };
  const cancelOutside = (event) => { if (event.target === event.currentTarget) close(false); };
  return html`<div ref=${dialogRef} id="sandbox-profile-diff-modal" class="modal-overlay show" role="dialog" aria-modal="true" aria-labelledby="sandbox-profile-diff-title" onClick=${cancelOutside}>
    <div class="config-diff-modal">
      <h3 id="sandbox-profile-diff-title">Confirm sandbox profile changes</h3>
      <p id="sandbox-profile-diff-sub" class="cfg-diff-sub">${model.before ? `${adds} line(s) added, ${dels} removed — server-normalized preview` : `${adds} line(s) added — new server-normalized profile`}</p>
      <div id="sandbox-profile-diff-body" class="config-diff">${diff.map((line, index) => html`<span key=${index} class=${`dl ${line.t}`}>${sign[line.t]} ${line.s}</span>`)}</div>
      ${(model.notices || []).length > 0 && html`<section id="sandbox-profile-diff-evaluation" class="sbx-diff-evaluation" aria-labelledby="sandbox-profile-diff-evaluation-title">
        <h4 id="sandbox-profile-diff-evaluation-title">Evaluation warnings</h4>
        ${(model.notices || []).map((notice, index) => html`<div key=${index} class="sbx-composition-warning" role="alert">⚠ ${notice.detail}</div>`)}
      </section>`}
      <div class="modal-buttons"><button id="sandbox-profile-diff-cancel" type="button" onClick=${() => close(false)}>Cancel</button><span class="spacer"></span><button ref=${confirmRef} id="sandbox-profile-diff-confirm" class="primary" type="button" onClick=${() => close(true)}>Save sandbox profile</button></div>
    </div>
  </div>`;
}

// Keep signal subscriptions at the overlay that consumes them. The dashboard
// snapshot poll republishes template/group arrays every two seconds; a single
// aggregate subscription here would make that unrelated change reconcile all
// open management controls (and close native select popups in Chromium).
function TemplateManagerSlot({ state, actions, confirmDiscard }) {
  if (!state.templateManager.value) return null;
  const current = {
    templates: state.templates.value || [],
    templateGroups: state.templateGroups.value || [],
    profiles: state.profiles.value || [],
    templateFilter: state.templateFilter.value,
  };
  return html`<${TemplateManager} current=${current} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
}

function TemplateEditorSlot({ state, actions, confirmDiscard, confirm }) {
  const descriptor = state.templateDialog.value;
  if (descriptor?.kind !== 'template-editor') return null;
  const current = {
    busy: state.busy.value,
    error: state.error.value,
    roles: state.roles.value || [],
    profiles: state.profiles.value || [],
  };
  return html`<${TemplateEditor} descriptor=${descriptor} current=${current} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} confirm=${confirm}/>`;
}

function ManagerSlot({ state, actions, confirmDiscard }) {
  const kind = state.manager.value;
  if (!kind) return null;
  const current = kind === 'profiles'
    ? { profiles: state.profiles.value || [], profileFilter: state.profileFilter.value, requests: { profiles: state.profilesRequest.request.value } }
    : kind === 'roles'
      ? { roles: state.roles.value || [], roleFilter: state.roleFilter.value, requests: { roles: state.rolesRequest.request.value } }
      : { sandboxProfiles: state.sandboxProfiles.value || [], sandboxFilter: state.sandboxFilter.value, requests: { sandbox: state.sandboxRequest.request.value } };
  return html`<${Manager} kind=${kind} current=${current} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
}

// contextFeatureSummary renders the profile editor's inline badge for a trim map.
function contextFeatureSummary(features) {
  const states = Object.values(features || {});
  const trimmed = states.filter((state) => state === 'off').length;
  const kept = states.filter((state) => state === 'on').length;
  return [trimmed ? `${trimmed} trimmed` : '', kept ? `${kept} kept` : ''].filter(Boolean).join(' · ');
}

function DialogSlot({ state, actions, confirmDiscard, openProfilePermissions, openProfileContextFeatures }) {
  const descriptor = state.dialog.value;
  switch (descriptor?.kind) {
    case 'profile-editor':
      return html`<${ProfileEditor} descriptor=${descriptor} roles=${state.roles.value || []} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} openProfilePermissions=${openProfilePermissions} openProfileContextFeatures=${openProfileContextFeatures}/>`;
    case 'role-editor':
      return html`<${RoleEditor} descriptor=${descriptor} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} openProfilePermissions=${openProfilePermissions}/>`;
    case 'profile-export':
      return html`<${ProfileExport} current=${{ profiles: state.profiles.value || [] }} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    case 'profile-import':
      return html`<${ProfileImport} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    case 'sandbox-editor':
      return html`<${SandboxEditor} descriptor=${descriptor} sandboxProfiles=${state.sandboxProfiles.value || []} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    case 'sandbox-export':
      return html`<${SandboxExport} current=${{ sandboxProfiles: state.sandboxProfiles.value || [] }} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    case 'sandbox-import':
      return html`<${SandboxImport} current=${{ sandboxProfiles: state.sandboxProfiles.value || [] }} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    case 'template-duplicate':
      return html`<${TemplateDuplicateDialog} descriptor=${descriptor} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    case 'template-import':
      return html`<${TemplateImportDialog} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    case 'template-from-group':
      return html`<${TemplateFromGroupDialog} descriptor=${descriptor} current=${{ templates: state.templates.value || [] }} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    case 'template-starters':
      return html`<${TemplateStartersDialog} descriptor=${descriptor} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    case 'group-import':
      return html`<${GroupImportDialog} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    case 'group-context':
      return html`<${GroupContextDialog} descriptor=${descriptor} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    case 'group-clone':
      return html`<${GroupCloneDialog} descriptor=${descriptor} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    case 'template-deploy': {
      const current = { templates: state.templates.value || [], templateGroups: state.templateGroups.value || [], profiles: state.profiles.value || [] };
      return html`<${TemplateDeployDialog} descriptor=${descriptor} current=${current} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    }
    default:
      return null;
  }
}

function SandboxDiffSlot({ state }) {
  const model = state.sandboxDiff.value;
  return html`<${SandboxDiffModal} model=${model} close=${state.cancelSandboxDiff} />`;
}

function ManagementApp({ state, actions, confirm, confirmDiscard, openProfilePermissions, openProfileContextFeatures }) {
  return html`<${TemplateManagerSlot} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>
    <${TemplateEditorSlot} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} confirm=${confirm}/>
    <${ManagerSlot} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>
    <${DialogSlot} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} openProfilePermissions=${openProfilePermissions} openProfileContextFeatures=${openProfileContextFeatures}/>
    <${SandboxDiffSlot} state=${state}/>`;
}

export function mountManagementIsland({ host, state, actions, confirm, confirmDiscard, openProfilePermissions, openProfileContextFeatures, registerCleanup }) {
  const controller = {
    openProfilesManageModal: () => actions.openManager('profiles'), openProfileEditor: actions.openProfileEditor, removeProfile: actions.removeProfile,
    openRolesManageModal: () => actions.openManager('roles'), openRoleEditor: actions.openRoleEditor, removeRole: actions.removeRole,
    openSandboxProfilesManageModal: () => actions.openManager('sandbox'), openSandboxProfileEditor: actions.openSandboxEditor, removeSandboxProfile: actions.removeSandbox,
    openTemplatesManageModal: actions.openTemplateManager, openTemplateEditor: actions.openTemplateEditor,
    updateTemplates: actions.updateTemplates, removeTemplate: actions.removeTemplate,
    exportTemplate: actions.exportTemplate,
    openTemplateDuplicate: actions.openTemplateDuplicate, openTemplateFromGroup: actions.openTemplateFromGroup,
    openTemplateImport: actions.openTemplateImport, openTemplateStarters: actions.openTemplateStarters,
    openTemplateDeploy: actions.openTemplateDeploy,
    openGroupImport: actions.openGroupImport, openGroupContext: actions.openGroupContext, openGroupClone: actions.openGroupClone,
  };
  const unregister = registerManagementController(controller);
  render(html`<${ManagementApp} state=${state} actions=${actions} confirm=${confirm} confirmDiscard=${confirmDiscard} openProfilePermissions=${openProfilePermissions} openProfileContextFeatures=${openProfileContextFeatures}/>` , host);
  registerCleanup(() => { state.cancelSandboxDiff(false); unregister(); render(null, host); });
}
