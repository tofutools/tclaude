// Package portowner answers one question: is the process listening on a
// loopback TCP port inside the subtree of a PID tclaude launched?
//
// It exists because that answer is a security boundary rather than a
// convenience. Every tclaude harness that reaches a locally launched process
// over TCP has the same unavoidable bind-close-exec gap — tclaude picks a free
// port by binding 127.0.0.1:0 and closing it, because no supported harness
// accepts a pre-bound listener — so between the pick and the harness's own bind
// another process on the host may win the port. Ownership verification is what
// closes that window, and for a harness with no authentication on the endpoint
// (Copilot's `--ui-server`, see TCL-1055) it is the ONLY thing that closes it.
//
// One implementation, not one per harness. A second copy of this walk is a
// second thing that can drift, and a drifted copy fails in the direction that
// matters: it says "yours" about a socket that is not.
//
// Deliberately IPv4-loopback-only, matching what callers dial. Recognising a
// ::1 listener on the same port would be strictly wrong here rather than more
// thorough: it would let a caller conclude "the port is mine" from a socket it
// is not about to talk to, while something else holds 127.0.0.1:N. Callers must
// dial the literal 127.0.0.1 rather than "localhost" so the socket proved and
// the socket used are the same one.
//
// Every function fails CLOSED. An unreadable /proc entry, an lsof that is not
// installed, an unsupported platform — each yields "not owned", never "assume
// owned". A caller that cannot prove ownership must refuse to talk to the port.
package portowner
