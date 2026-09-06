// Package codex provides the cohesive terminal-authoritative OpenAI Codex
// implementation for the replacement backend.
//
// New requires an executable and an absolute PrivateRoot. NativeHome defaults
// to PrivateRoot/native-home and, when overridden, must remain inside
// PrivateRoot. It is the durable provider-owned CODEX_HOME shared by native
// sessions; terminal identities, observation spools, and execution action
// credentials remain separate resources. Before first use, setup must run the
// native `codex login` with CODEX_HOME set to that exact directory. The
// provider never imports ambient ~/.codex credentials because Codex owns and
// refreshes OAuth state in the home where login created it.
package codex
