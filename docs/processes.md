# Processes

Processes is an experimental template-authoring feature. The legacy execution
engine has been removed while its replacement is designed. You can create,
validate, version, edit, and delete reusable process templates; you cannot
currently instantiate or execute them.

Enable the authoring UI with `features.processes` in tclaude's config. The
dashboard then shows the Templates library and drag-and-drop editor. Template
parameters, performers, edges, layout, snippets, version history, authorship,
and source-hash CAS checks remain available.

Agents can author the same templates through:

```bash
tclaude agent process-templates ls
tclaude agent process-templates show <template-id>
tclaude agent process-templates validate --file template.yaml
tclaude agent process-templates save --file template.yaml --source-hash <hash>
```

Reads require `process.templates.read`; saves require
`process.templates.manage`. These permissions authorize authoring only and do
not execute a template.

The top-level `tclaude process` command retains its former runtime verb names
for discoverability and script diagnostics. Runtime verbs return an explicit
`process runtime is temporarily unavailable: no engine is installed` error.
Template authoring remains available through `tclaude process templates` and
the agent command above.

On daemon startup, tclaude removes obsolete filesystem run data and legacy run
lock files. It deliberately preserves the complete Processes root, including
all template versions, heads, layouts, snippets, authorship records, and
template locks.
