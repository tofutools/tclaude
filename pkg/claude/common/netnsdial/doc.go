// Package netnsdial opens a TCP connection to a loopback port inside another
// process's network namespace: the live-page stage uses it to reach a dev
// server an agent runs inside its sandbox's private network namespace.
//
// agentd cannot simply setns(CLONE_NEWNET) there. The kernel requires
// CAP_SYS_ADMIN over the user namespace that owns the target network namespace
// AND in the caller's own user namespace, and agentd is unprivileged in the
// initial one. Joining the owning user namespace first would grant that, but
// setns(CLONE_NEWUSER) is illegal from the multithreaded Go runtime. So, like
// the sandbox supervisor installing its nft policy, the join is delegated to
// util-linux nsenter (single-threaded C), which then execs a tiny helper mode
// of this binary. The helper connects to 127.0.0.1:<port> inside the
// namespace, hands the connected socket back over SCM_RIGHTS, and exits. The
// returned connection is an ordinary socket in that namespace: no relay
// process stays behind.
//
// The helper only ever dials the IPv4 loopback literal at the port it was
// given. It takes no host name and no address, so it cannot be pointed
// anywhere else.
package netnsdial
