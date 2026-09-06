# Adding a harness

Implement a cohesive provider under `internal/backend/providers`. Start with the
contracts in `ports` and the durable behavior in `app`; do not add native parsing
to HTTP handlers or product commands.

The provider owns prepare/abort/release, exact recovery evidence, observation,
interaction and stop. Add focused capability views for supported history,
attachment, usage or native guidance. Unsupported operations must be explicit.
Prepare describes actual requirements and enforced policy; consume the
application's one-shot permit immediately before the native effect.

Keep credentials in protected owned resources and out of durable public
evidence. Separate an inherited execution action credential from primary-native
observation proof. Recovery must prove the exact retained resource before
restoring control or callback admission; never adopt a process by PID alone.

Validate the common journey through real application/SQLite boundaries with
native subprocess doubles, plus suitable native feasibility evidence. Exercise
restart, stale identities, uncertain release, no replay, history precision and
cleanup ownership. Record the limits of authenticated/native coverage. Register
the provider in product composition only after independent review.
