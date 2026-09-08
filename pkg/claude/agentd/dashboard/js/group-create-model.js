function text(value) {
  return String(value || '');
}

function environment(value) {
  return (value || []).map((entry) => ({
    name: text(entry?.name), value: text(entry?.value),
  }));
}

function requestEnvironment(value) {
  return environment(value)
    .map((entry) => ({ name: entry.name.trim(), value: entry.value }))
    .filter((entry) => entry.name);
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

export function nextGroupCloneName(groups, groupName) {
  const match = /^(.*?)-(?:c|clone)-\d+$/.exec(text(groupName));
  const prefix = `${match ? match[1] : text(groupName)}-c-`;
  const used = new Set((groups || [])
    .filter((item) => item?.name?.startsWith(prefix))
    .map((item) => Number.parseInt(item.name.slice(prefix.length), 10))
    .filter(Number.isInteger));
  let suffix = 1;
  while (used.has(suffix)) suffix += 1;
  return `${prefix}${suffix}`;
}

function sourcePrefill(template, source) {
  if (!source) {
    return {
      descr: text(template?.descr),
      context: text(template?.default_context),
      cwd: '',
      cwdOrigin: '',
      attachmentURL: '',
      attachmentLabel: '',
      environment: [],
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
    attachmentURL: text(source.attachment_url),
    attachmentLabel: text(source.attachment_label_override),
    environment: environment(source.environment),
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
    attachmentURL: text(parent?.attachment_url),
    attachmentLabel: text(parent?.attachment_label_override),
    environment: environment(parent?.environment),
  };
}

export function createGroupCreateDraft({
  templates = [], groups = [], presetTemplate = '', parentGroup = '',
  cloneGroup = '', defaultName = '', placement = null,
  cloneTransport = 'ssh',
} = {}) {
  const template = findGroupCreateTemplate(templates, presetTemplate);
  const parent = findGroupCreateSource(groups, parentGroup);
  const cloneSource = findGroupCreateSource(groups, cloneGroup);
  const prefill = parent
    ? parentPrefill(template, parent)
    : sourcePrefill(template, cloneSource);
  const origin = cloneGroup ? 'group' : template ? 'template' : parentGroup ? 'group' : 'blank';
  return {
    origin,
    template: template?.name || '',
    name: cloneGroup ? text(defaultName) : '',
    source: text(cloneGroup || parentGroup),
    nested: false,
    cloneGroup: text(cloneGroup),
    clonePreset: !!cloneGroup,
    cloneDefaultName: text(defaultName),
    clonePlacement: placement,
    parent: text(placement?.parent ?? parentGroup),
    withAgents: false,
    copyOwners: false,
    descr: prefill.descr,
    cwd: prefill.cwd,
    cwdOrigin: prefill.cwdOrigin,
    workspaceMode: 'existing',
    repository: '',
    cloneTransport: cloneTransport === 'https' ? 'https' : 'ssh',
    cloneDestination: '',
    attachRepository: true,
    context: prefill.context,
    attachmentURL: prefill.attachmentURL,
    attachmentLabel: prefill.attachmentLabel,
    environment: prefill.environment,
    task: '',
    maxMembers: cloneSource ? String(cloneSource.max_members || 0) : '',
  };
}

// Selecting a new template re-derives only fields owned by that selection.
// User-entered name/cwd/cap and the per-instantiation task survive compatible
// template-to-template switches. A source- or parent-derived cwd is replaced
// with the new source's value instead of leaking into an incompatible mode.
export function selectGroupCreateTemplate(draft, templateName, {
  templates = [], groups = [],
} = {}) {
  const template = findGroupCreateTemplate(templates, templateName);
  if (!template) {
    const prefill = {
      descr: '', context: '',
      cwd: draft.cwdOrigin === 'source' || draft.cwdOrigin === 'parent' ? '' : draft.cwd,
      cwdOrigin: draft.cwdOrigin === 'source' || draft.cwdOrigin === 'parent' ? '' : draft.cwdOrigin,
      attachmentURL: '', attachmentLabel: '',
      environment: [],
    };
    return {
      ...draft,
      origin: 'blank', template: '', source: '', nested: false, task: '',
      cloneGroup: '', clonePreset: false,
      ...prefill,
    };
  }
  const source = findGroupCreateSource(groups, draft.source);
  const prefill = source
      ? sourcePrefill(template, source)
      : {
          descr: text(template.descr),
          context: text(template.default_context),
          cwd: draft.cwdOrigin === 'source' || draft.cwdOrigin === 'parent' ? '' : draft.cwd,
          cwdOrigin: draft.cwdOrigin === 'source' || draft.cwdOrigin === 'parent' ? '' : draft.cwdOrigin,
          attachmentURL: '', attachmentLabel: '',
          environment: [],
        };
  return {
    ...draft,
    origin: 'template',
    template: template.name,
    source: source?.name || '',
    cloneGroup: '', clonePreset: false,
    nested: !!source && draft.nested,
    ...prefill,
  };
}

export function selectGroupCreateSource(draft, sourceName, {
  templates = [], groups = [],
} = {}) {
  const template = findGroupCreateTemplate(templates, draft.template);
  const source = findGroupCreateSource(groups, sourceName);
  if (draft.origin === 'group') {
    const exactClone = draft.clonePreset;
    const cloneDefaultName = exactClone && source
      ? nextGroupCloneName(groups, source.name) : draft.cloneDefaultName;
    return {
      ...draft,
      source: source?.name || '',
      cloneGroup: exactClone ? source?.name || '' : '',
      cloneDefaultName,
      clonePlacement: exactClone ? {
        ...(draft.clonePlacement || {}), anchor: source?.name || '',
      } : draft.clonePlacement,
      name: exactClone && source ? cloneDefaultName : draft.name,
      nested: false,
      ...(exactClone ? { maxMembers: String(source?.max_members || 0) } : {}),
      ...sourcePrefill(null, source),
    };
  }
  if (!template) return { ...draft, source: '', nested: false };
  const prefill = source
    ? sourcePrefill(template, source)
    : {
        descr: text(template.descr),
        context: text(template.default_context),
        cwd: draft.cwdOrigin === 'source' ? '' : draft.cwd,
        cwdOrigin: draft.cwdOrigin === 'source' ? '' : draft.cwdOrigin,
        attachmentURL: '', attachmentLabel: '',
        environment: [],
      };
  return {
    ...draft,
    source: source?.name || '',
    nested: false,
    ...prefill,
  };
}

export function selectGroupCreateOrigin(draft, origin, {
  templates = [], groups = [],
} = {}) {
  if (origin === 'template') {
    const templateName = draft.template || templates[0]?.name || '';
    if (!templateName) {
      return { ...draft, origin: 'template', cloneGroup: '', clonePreset: false };
    }
    return selectGroupCreateTemplate({ ...draft, cloneGroup: '' }, templateName, {
      templates, groups,
    });
  }
  if (origin === 'group') {
    const sourceName = draft.source || draft.parent || groups[0]?.name || '';
    return selectGroupCreateSource({
      ...draft, origin: 'group', template: '', task: '',
    }, sourceName, { templates, groups });
  }
  const prefill = {
    descr: '', context: '',
    cwd: draft.cwdOrigin === 'source' || draft.cwdOrigin === 'parent' ? '' : draft.cwd,
    cwdOrigin: draft.cwdOrigin === 'source' || draft.cwdOrigin === 'parent' ? '' : draft.cwdOrigin,
    attachmentURL: '', attachmentLabel: '',
    environment: [],
  };
  return {
    ...draft,
    origin: 'blank', template: '', source: '', nested: false, task: '',
    cloneGroup: '', clonePreset: false, ...prefill,
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
    'origin', 'template', 'name', 'source', 'nested', 'parent', 'descr', 'cwd',
    'withAgents', 'copyOwners',
    'workspaceMode', 'repository', 'cloneTransport', 'cloneDestination',
    'attachRepository',
    'attachmentURL', 'attachmentLabel',
    'context', 'task', 'maxMembers', 'environment',
  ];
  return keys.some((key) => key === 'environment'
    ? JSON.stringify(draft[key]) !== JSON.stringify(baseline[key])
    : draft[key] !== baseline[key]);
}

export function validateGroupCreateDraft(draft, { templateMode = false } = {}) {
  if (!text(draft.name).trim()) return 'name is required';
  if (draft.origin === 'group' && !text(draft.source).trim()) return 'source group is required';
  if (draft.origin === 'template' && !text(draft.template).trim()) return 'group template is required';
  if (draft.workspaceMode === 'clone') {
    if (!text(draft.repository).trim()) return 'repository is required';
    if (!text(draft.cloneDestination).trim()) return 'clone destination is required';
    if (!['ssh', 'https'].includes(draft.cloneTransport)) return 'clone transport must be SSH or HTTPS';
  }
  if (templateMode) return '';
  const raw = text(draft.maxMembers).trim();
  if (raw && !/^\d+$/.test(raw)) {
    return 'max members must be a non-negative integer (0 = unlimited)';
  }
  return '';
}

export function groupCreateRequest(draft, template, parentGroup = '') {
  const name = text(draft.name).trim();
  const parent = Object.prototype.hasOwnProperty.call(draft, 'parent')
    ? text(draft.parent) : text(parentGroup);
  if (draft.cloneGroup) {
    const body = {
      no_clone_members: !draft.withAgents,
      copy_owners: !!draft.copyOwners,
      descr: text(draft.descr).trim(),
      default_cwd: text(draft.workspaceMode === 'clone' ? draft.cloneDestination : draft.cwd).trim(),
      default_context: text(draft.context),
      environment: requestEnvironment(draft.environment),
      attachment_url: text(draft.attachmentURL).trim(),
      attachment_label: text(draft.attachmentLabel).trim(),
      max_members: Number.parseInt(text(draft.maxMembers), 10) || 0,
      ...(draft.workspaceMode === 'clone' ? { repository_clone: {
        repository: text(draft.repository).trim(), transport: draft.cloneTransport,
        destination: text(draft.cloneDestination).trim(), attach: !!draft.attachRepository,
      } } : {}),
    };
    if (name !== text(draft.cloneDefaultName)) body.new_name = name;
    body.parent = parent;
    return {
      kind: 'clone', name,
      source: text(draft.cloneGroup),
      placement: draft.clonePlacement ? {
        ...draft.clonePlacement, parent,
      } : null,
      withAgents: !!draft.withAgents,
      copyOwners: !!draft.copyOwners,
      url: `/api/groups/${encodeURIComponent(draft.cloneGroup)}/clone`,
      body,
    };
  }
  const cloning = draft.workspaceMode === 'clone';
  const cwd = cloning ? text(draft.cloneDestination).trim() : text(draft.cwd).trim();
  const repositoryClone = cloning ? {
    repository: text(draft.repository).trim(),
    transport: draft.cloneTransport,
    destination: cwd,
    attach: !!draft.attachRepository,
  } : undefined;
  if (!template) {
    return {
      kind: 'blank', name,
      url: '/api/groups',
      body: {
        name,
        parent,
        descr: text(draft.descr).trim(),
        default_cwd: cwd,
        default_context: text(draft.context).trim(),
        environment: requestEnvironment(draft.environment),
        ...(text(draft.attachmentURL).trim() ? {
          attachment_url: text(draft.attachmentURL).trim(),
          attachment_label: text(draft.attachmentLabel).trim(),
        } : {}),
        max_members: Number.parseInt(text(draft.maxMembers).trim(), 10) || 0,
        ...(repositoryClone ? { repository_clone: repositoryClone } : {}),
      },
    };
  }
  const body = {
    group_name: name,
    task: text(draft.task),
    cwd,
    descr_override: text(draft.descr).trim(),
    context_override: text(draft.context),
    environment: requestEnvironment(draft.environment),
  };
  if (text(draft.attachmentURL).trim()) {
    body.attachment_url = text(draft.attachmentURL).trim();
    body.attachment_label = text(draft.attachmentLabel).trim();
  }
  if (repositoryClone) body.repository_clone = repositoryClone;
  if (parent) body.parent = parent;
  else if (draft.source && draft.nested) body.parent = draft.source;
  return {
    kind: 'template', name,
    url: `/api/templates/${encodeURIComponent(template.name)}/instantiate`,
    body,
  };
}

export function derivedCloneDestination(repository) {
  let value = text(repository).trim().replace(/\/$/, '').replace(/\.git$/, '');
  value = value.slice(Math.max(value.lastIndexOf('/'), value.lastIndexOf(':')) + 1);
  return value ? `~/git/${value}` : '';
}
