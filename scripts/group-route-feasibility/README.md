# Group-route feasibility probe (TCL-947)

This is a bounded experiment, not a tclaude capability. It does not change
production launch plans, sandbox policy, firewall rules, or agentd state.

Run it from the repository root with:

```bash
scripts/group-route-feasibility/run.sh
```

The Linux arm requires a runner with unprivileged Bubblewrap user namespaces.
The authoritative Darwin arm runs only on the exact-head macOS CI job because
Seatbelt behavior is host- and OS-version-dependent. The probe intentionally
fails on a skipped or unavailable platform arm rather than treating missing
evidence as a pass.

Linux starts three independent `bwrap --unshare-all` processes. A host-side
Unix-socket broker is the stand-in for agentd. The publisher and consumer each
create their own loopback listener after launch, and the broker carries an
opaque byte stream between them. A third group is denied by the broker. The
probe also checks that the helpers cannot reach a host control listener or the
public Internet, and records each network namespace identity.

Darwin renders eight exact TCP slots in a Seatbelt profile. It checks a
post-launch publisher bind, a broker-held consumer endpoint, opaque forwarding,
an EADDRINUSE collision, a denied neighboring port, and the existing
host-wide-`localhost` limitation. The limitation is reported explicitly; the
probe does not add a fragile collision workaround or widen the sandbox floor.
