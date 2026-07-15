function text(value) {
  return String(value || '');
}

export function combineGroupAndTemplateContext(groupContext, templateContext) {
  const group = text(groupContext).trim();
  const template = text(templateContext).trim();
  if (group && template) {
    return `## Mirrored group context\n\n${group}\n\n## Template context\n\n${template}`;
  }
  return group || template;
}

export function findGroupCreateTemplate(templates, name) {
  if (!name) return null;
  return (templates || []).find((template) => template?.name === name) || null;
}

export function findGroupCreateSource(groups, name) {
  if (!name) return null;
  return (groups || []).find((group) => group?.name === name) || null;
}

function sourcePrefill(template, source) {
  if (!source) {
    return {
      descr: text(template?.descr),
      context: text(template?.default_context),
      cwd: '',
      cwdOrigin: '',
    };
  }
  return {
    descr: text(source.descr),
    context: combineGroupAndTemplateContext(
      source.default_context,
      template?.default_context,
    ),
    cwd: text(source.default_cwd),
    cwdOrigin: 'source',
  };
}

function parentPrefill(template, parent) {
  return {
    descr: text(parent?.descr),
    context: combineGroupAndTemplateContext(
      parent?.default_context,
      template?.default_context,
    ),
    cwd: text(parent?.default_cwd),
    cwdOrigin: parent ? 'parent' : '',
  };
}

export function createGroupCreateDraft({
  templates = [], groups = [], presetTemplate = '', parentGroup = '',
} = {}) {
  const template = findGroupCreateTemplate(templates, presetTemplate);
  const parent = findGroupCreateSource(groups, parentGroup);
  const prefill = parent
    ? parentPrefill(template, parent)
    : sourcePrefill(template, null);
  return {
    template: template?.name || '',
    name: '',
    source: '',
    nested: false,
    descr: prefill.descr,
    cwd: prefill.cwd,
    cwdOrigin: prefill.cwdOrigin,
    context: prefill.context,
    task: '',
    maxMembers: '',
  };
}

// Selecting a new template re-derives only fields owned by that selection.
// User-entered name/cwd/cap and the per-instantiation task survive compatible
// template-to-template switches. A source- or parent-derived cwd is replaced
// with the new source's value instead of leaking into an incompatible mode.
export function selectGroupCreateTemplate(draft, templateName, {
  templates = [], groups = [], parentGroup = '',
} = {}) {
  const template = findGroupCreateTemplate(templates, templateName);
  const parent = findGroupCreateSource(groups, parentGroup);
  if (!template) {
    const prefill = parent ? parentPrefill(null, parent) : {
      descr: '', context: '',
      cwd: draft.cwdOrigin === 'source' || draft.cwdOrigin === 'parent' ? '' : draft.cwd,
      cwdOrigin: draft.cwdOrigin === 'source' || draft.cwdOrigin === 'parent' ? '' : draft.cwdOrigin,
    };
    return {
      ...draft,
      template: '', source: '', nested: false, task: '',
      ...prefill,
    };
  }
  const source = parent
    ? null
    : findGroupCreateSource(groups, draft.source);
  const prefill = parent
    ? parentPrefill(template, parent)
    : source
      ? sourcePrefill(template, source)
      : {
          descr: text(template.descr),
          context: text(template.default_context),
          cwd: draft.cwdOrigin === 'source' || draft.cwdOrigin === 'parent' ? '' : draft.cwd,
          cwdOrigin: draft.cwdOrigin === 'source' || draft.cwdOrigin === 'parent' ? '' : draft.cwdOrigin,
        };
  return {
    ...draft,
    template: template.name,
    source: parent ? '' : source?.name || '',
    nested: parent ? false : !!source && draft.nested,
    ...prefill,
  };
}

export function selectGroupCreateSource(draft, sourceName, {
  templates = [], groups = [], parentGroup = '',
} = {}) {
  const template = findGroupCreateTemplate(templates, draft.template);
  if (!template || parentGroup) return { ...draft, source: '', nested: false };
  const source = findGroupCreateSource(groups, sourceName);
  const prefill = source
    ? sourcePrefill(template, source)
    : {
        descr: text(template.descr),
        context: text(template.default_context),
        cwd: draft.cwdOrigin === 'source' ? '' : draft.cwd,
        cwdOrigin: draft.cwdOrigin === 'source' ? '' : draft.cwdOrigin,
      };
  return {
    ...draft,
    source: source?.name || '',
    nested: false,
    ...prefill,
  };
}

export function reconcileGroupCreateTemplates(draft, options = {}) {
  if (!draft.template || findGroupCreateTemplate(options.templates, draft.template)) {
    return draft;
  }
  return selectGroupCreateTemplate(draft, '', options);
}

export function groupCreateDraftIsDirty(draft, baseline) {
  const keys = [
    'template', 'name', 'source', 'nested', 'descr', 'cwd',
    'context', 'task', 'maxMembers',
  ];
  return keys.some((key) => draft[key] !== baseline[key]);
}

export function validateGroupCreateDraft(draft, { templateMode = false } = {}) {
  if (!text(draft.name).trim()) return 'name is required';
  if (templateMode) return '';
  const raw = text(draft.maxMembers).trim();
  if (raw && !/^\d+$/.test(raw)) {
    return 'max members must be a non-negative integer (0 = unlimited)';
  }
  return '';
}

export function groupCreateRequest(draft, template, parentGroup = '') {
  const name = text(draft.name).trim();
  if (!template) {
    return {
      kind: 'blank', name,
      url: '/api/groups',
      body: {
        name,
        parent: text(parentGroup),
        descr: text(draft.descr).trim(),
        default_cwd: text(draft.cwd).trim(),
        default_context: text(draft.context).trim(),
        max_members: Number.parseInt(text(draft.maxMembers).trim(), 10) || 0,
      },
    };
  }
  const body = {
    group_name: name,
    task: text(draft.task),
    cwd: text(draft.cwd).trim(),
    descr_override: text(draft.descr).trim(),
    context_override: text(draft.context),
  };
  if (parentGroup) body.parent = parentGroup;
  else if (draft.source && draft.nested) body.parent = draft.source;
  return {
    kind: 'template', name,
    url: `/api/templates/${encodeURIComponent(template.name)}/instantiate`,
    body,
  };
}
