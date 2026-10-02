// Package geminifixture runs the REAL pinned Gemini CLI against a local mock
// model and checks tclaude's Gemini harness code against what the binary
// actually does.
//
// The Gemini adapter was written from the CLI's source tree without a Gemini
// account. Source proves a flag; it does not prove that the session file the
// binary writes is the one tclaude's reader expects, that the hook settings
// tclaude installs are accepted and fired, or that a trust entry tclaude
// records satisfies the CLI. These scenarios supply that evidence.
//
// Credential-free: GOOGLE_GEMINI_BASE_URL points the CLI's model client at a
// loopback server, and an API key placeholder satisfies its auth check. The
// runner builds the child environment from scratch, so no credential, setting
// or session of the machine running the suite is read or written.
//
// The suite is gated by TCLAUDE_GEMINI_FIXTURE_SMOKE=1, and with the gate set
// a missing `gemini` binary FAILS rather than skips, so CI cannot go green by
// running nothing.
package geminifixture
