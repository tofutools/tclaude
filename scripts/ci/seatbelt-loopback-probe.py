#!/usr/bin/env python3
"""TEMPORARY (#3999, awb tcl-gejtfc; remove before merge).

Isolates the macOS 27.2 Seatbelt EPERM on an allowed localhost:N connect.
For each profile form, with and without a same-port listener on the host's
non-loopback address, it runs a sandboxed connect to 127.0.0.1:N several
times and prints the tally.

  allow-after-deny  the Local-access form tclaude renders today:
                    (deny network-outbound (remote ip "*:*"))
                    (allow network-outbound (remote tcp "localhost:N"))
  allow-ip-form     the same, but the allow names (remote ip "localhost:N")
  inside-deny       the exception inside the deny predicate, as the isolated
                    posture renders its exceptions
"""
import socket
import subprocess
import sys

RUNS = int(sys.argv[1]) if len(sys.argv) > 1 else 10

CONNECT = (
    "import socket,sys\n"
    "s=socket.socket()\n"
    "s.settimeout(2)\n"
    "try:\n"
    "    s.connect(('127.0.0.1', int(sys.argv[1])))\n"
    "    print('ok')\n"
    "except OSError as e:\n"
    "    print('err', e.errno, e.strerror)\n"
)


def profile(form, port):
    head = "(version 1)\n(allow default)\n"
    if form == "allow-after-deny":
        return head + (
            '(deny network-outbound (remote ip "*:*"))\n'
            f'(allow network-outbound (remote tcp "localhost:{port}"))\n')
    if form == "allow-ip-form":
        return head + (
            '(deny network-outbound (remote ip "*:*"))\n'
            f'(allow network-outbound (remote ip "localhost:{port}"))\n')
    if form == "inside-deny":
        return head + (
            "(deny network-outbound\n  (require-all\n"
            '    (remote ip "*:*")\n'
            f'    (require-not (remote tcp "localhost:{port}"))))\n')
    raise ValueError(form)


UNIX_DENY_CASES = ("unix-literal", "unix-path-literal", "unix-literal-param", "unix-path-literal-param")


def unix_profile(case, port, sock_path):
    """The passing allow-after-deny pair, then one unix-socket outbound deny
    naming sock_path, inline or through a -D param, as literal or
    path-literal."""
    head = ("(version 1)\n(allow default)\n"
            '(deny network-outbound (remote ip "*:*"))\n'
            f'(allow network-outbound (remote tcp "localhost:{port}"))\n')
    filt = "path-literal" if "path-literal" in case else "literal"
    target = '(param "SOCK")' if case.endswith("-param") else f'"{sock_path}"'
    return head + f"(deny network-outbound\n  (remote unix-socket ({filt} {target})))\n"


def unix_case(case, present):
    """One sandboxed TCP connect under unix_profile, with the unix socket
    path present (a bound listener) or absent."""
    import os
    import tempfile
    base = tempfile.mkdtemp(prefix="sbp", dir="/tmp")
    sock_path = os.path.join(base, "agentd.sock")
    unix = None
    if present:
        unix = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        unix.bind(sock_path)
        unix.listen(4)
    listener = socket.socket()
    listener.bind(("127.0.0.1", 0))
    listener.listen(8)
    port = listener.getsockname()[1]
    try:
        args = ["/usr/bin/sandbox-exec", "-p", unix_profile(case, port, sock_path)]
        if case.endswith("-param"):
            args.append(f"-DSOCK={sock_path}")
        args += [sys.executable, "-c", CONNECT, str(port)]
        out = subprocess.run(args, capture_output=True, text=True, timeout=20)
        return out.stdout.strip() or out.stderr.strip()[-100:] or "no-output"
    finally:
        listener.close()
        if unix is not None:
            unix.close()
        try:
            os.unlink(sock_path)
        except OSError:
            pass
        os.rmdir(base)


def lan_address():
    probe = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        probe.connect(("192.0.2.1", 9))
        return probe.getsockname()[0]
    except OSError:
        return None
    finally:
        probe.close()


def one(form, same_port):
    listener = socket.socket()
    listener.bind(("127.0.0.1", 0))
    listener.listen(8)
    port = listener.getsockname()[1]
    other = None
    if same_port:
        addr = lan_address()
        if addr is None:
            listener.close()
            return "no-lan-address"
        other = socket.socket()
        try:
            other.bind((addr, port))
        except OSError:
            other.close()
            listener.close()
            return "port-busy"
        other.listen(8)
    try:
        out = subprocess.run(
            ["/usr/bin/sandbox-exec", "-p", profile(form, port),
             sys.executable, "-c", CONNECT, str(port)],
            capture_output=True, text=True, timeout=20)
        return (out.stdout.strip() or out.stderr.strip() or "no-output")
    finally:
        listener.close()
        if other is not None:
            other.close()


def main():
    for case in UNIX_DENY_CASES:
        for present in (False, True):
            results = [unix_case(case, present) for _ in range(RUNS)]
            ok = sum(r == "ok" for r in results)
            others = sorted({r for r in results if r != "ok"})
            print(f"{case:24} socket_present={str(present):5} ok={ok}/{RUNS} {others}")
    for form in ("allow-after-deny", "allow-ip-form", "inside-deny"):
        for same_port in (False, True):
            results = [one(form, same_port) for _ in range(RUNS)]
            ok = sum(r == "ok" for r in results)
            others = sorted({r for r in results if r != "ok"})
            print(f"{form:17} same_port={str(same_port):5} ok={ok}/{RUNS} {others}")


if __name__ == "__main__":
    main()
