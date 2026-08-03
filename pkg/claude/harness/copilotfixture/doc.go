// Package copilotfixture is a credential-free compatibility lab for the
// GitHub Copilot CLI.
//
// It exists because tclaude's Copilot harness descriptor (pkg/claude/harness/
// copilot.go) was written from published documentation alone, with no binary
// available. Documentation proves a launch flag; it does not prove a session
// layout, a wire shape, an exit code or a retry policy. This package supplies
// the missing evidence by running the REAL pinned CLI against a deterministic
// local provider and recording sanitized observations that a test can diff.
//
// Two properties are load-bearing:
//
//   - Credential-free. Setting COPILOT_PROVIDER_BASE_URL activates Copilot's
//     BYOK mode, which the CLI documents as not requiring GitHub
//     authentication. The runner additionally REMOVES every GitHub/Copilot
//     token variable from the child environment and sets COPILOT_OFFLINE=true,
//     so a regression that reintroduces an auth dependency fails here rather
//     than silently passing on a developer machine that happens to be logged
//     in. No credential, enterprise policy or real session content is involved.
//
//   - Deterministic. The mock provider is scripted per turn, so a scenario
//     replays identically on every run. Volatile values the CLI mints
//     (session UUIDs, event ids, timestamps, ports, absolute paths) are
//     normalized by the sanitizer before anything is compared or committed.
//
// What deliberately is NOT captured: raw system prompts and raw tool schemas.
// The CLI ships a ~26 kB system prompt and a 17-entry tool catalog in every
// request. Committing either would be a large, version-coupled blob that
// churns on each CLI bump while proving nothing tclaude depends on, so the
// sanitizer records their SHAPE (message roles, tool-name set, byte-length
// buckets) and a digest instead of their text.
package copilotfixture
