#!/usr/bin/env python3
"""TEMPORARY (#3999, awb tcl-gejtfc; remove before merge).

Replays the Seatbelt profiles tclaude rendered (dumped through
TCLAUDE_TEMP_SEATBELT_PROFILE_DUMP_DIR) against a connect to each allowed
localhost:N port, then drops one top-level rule at a time to find the rule
that turns the allowed connect into EPERM on macOS 27.2.

Usage: seatbelt-profile-replay.py <dump-dir> [runs]
"""
import json
import os
import pathlib
import re
import socket
import subprocess
import sys

ALLOW = re.compile(r'\(allow network-outbound \(remote tcp "localhost:(\d+)"\)\)')
# The isolated/floor posture's exceptions: require-not inside the outbound deny.
EXCEPT = re.compile(r'\(require-not \(remote tcp "localhost:(\d+)"\)\)')


def forms(text):
    """Top-level s-expressions, each with its source text."""
    out, depth, start, i, n = [], 0, None, 0, len(text)
    while i < n:
        c = text[i]
        if c == ";":
            while i < n and text[i] != "\n":
                i += 1
            continue
        if c == '"':
            i += 1
            while i < n and text[i] != '"':
                i += 2 if text[i] == "\\" else 1
        elif c == "(":
            if depth == 0:
                start = i
            depth += 1
        elif c == ")":
            depth -= 1
            if depth == 0:
                out.append(text[start:i + 1])
        i += 1
    return out


def connect(profile, params, port):
    args = ["/usr/bin/sandbox-exec", "-p", profile]
    args += [f"-D{k}={v}" for k, v in params.items()]
    args += ["--", "/usr/bin/nc", "-z", "-G", "2", "127.0.0.1", str(port)]
    res = subprocess.run(args, capture_output=True, text=True, timeout=30)
    if res.returncode == 0:
        return "ok"
    msg = (res.stderr or res.stdout).strip().splitlines()
    return msg[-1][-80:] if msg else f"exit {res.returncode}"


def timing(profile, params, port):
    """One sandbox, connects at 0, +100ms, +1s and +5s: does a refusal clear
    with time (rules settling) or persist (profile content)?"""
    script = ("for d in 0 0.1 1 5; do sleep $d; "
              f"if /usr/bin/nc -z -G 2 127.0.0.1 {port} 2>/dev/null; "
              "then printf 'ok@%s ' $d; else printf 'refused@%s ' $d; fi; done")
    args = ["/usr/bin/sandbox-exec", "-p", profile]
    args += [f"-D{k}={v}" for k, v in params.items()]
    args += ["--", "/bin/sh", "-c", script]
    res = subprocess.run(args, capture_output=True, text=True, timeout=60)
    return res.stdout.strip() or res.stderr.strip()


PY_CONNECT = ("import socket,sys\n"
              "s=socket.socket(socket.AF_INET)\n"
              "s.settimeout(2)\n"
              "try:\n"
              "    s.connect(('127.0.0.1', int(sys.argv[1]))); print('ok')\n"
              "except OSError as e:\n"
              "    print('err', e.errno)\n")


def connect_py(profile, params, port):
    args = ["/usr/bin/sandbox-exec", "-p", profile]
    args += [f"-D{k}={v}" for k, v in params.items()]
    args += ["--", sys.executable, "-c", PY_CONNECT, str(port)]
    res = subprocess.run(args, capture_output=True, text=True, timeout=30)
    return res.stdout.strip() or res.stderr.strip()[-80:] or f"exit {res.returncode}"


def connect_bin(binary):
    """A dialer binary called as `<binary> 127.0.0.1 <port>`; exit 0 is ok."""
    def fn(profile, params, port):
        args = ["/usr/bin/sandbox-exec", "-p", profile]
        args += [f"-D{k}={v}" for k, v in params.items()]
        args += ["--", binary, "127.0.0.1", str(port)]
        res = subprocess.run(args, capture_output=True, text=True, timeout=30)
        if res.returncode == 0:
            return "ok"
        return (res.stderr or res.stdout).strip()[-80:] or f"exit {res.returncode}"
    return fn


def signature(binary):
    res = subprocess.run(["/usr/bin/codesign", "-dv", binary], capture_output=True, text=True)
    keep = [l for l in res.stderr.splitlines()
            if l.startswith(("Identifier=", "Signature=", "Authority=", "TeamIdentifier=", "Platform", "flags", "CodeDirectory"))]
    return "; ".join(keep[:5]) or res.stderr.strip()[:120]


def connectors():
    """(name, fn, binary) for every connector: python, nc, and the dialers in
    $SEATBELT_DIALERS (name=path,...)."""
    out = [("python", connect_py, sys.executable), ("nc", connect, "/usr/bin/nc")]
    for item in filter(None, os.environ.get("SEATBELT_DIALERS", "").split(",")):
        name, _, path = item.partition("=")
        if os.path.exists(path):
            out.append((name, connect_bin(path), path))
    return out


def rate_with(fn, profile, params, port, runs):
    results = [fn(profile, params, port) for _ in range(runs)]
    return sum(r == "ok" for r in results), sorted({r for r in results if r != "ok"})


def rate(profile, params, port, runs):
    results = [connect(profile, params, port) for _ in range(runs)]
    return sum(r == "ok" for r in results), sorted({r for r in results if r != "ok"})


def main():
    dump = pathlib.Path(sys.argv[1])
    runs = int(sys.argv[2]) if len(sys.argv) > 2 else 10
    files = sorted(dump.glob("profile-*.json"))
    print(f"{len(files)} dumped profiles")
    for f in files:
        data = json.loads(f.read_text())
        profile, params = data["profile"], data["params"]
        ports = [int(p) for p in ALLOW.findall(profile)]
        kind = "allow"
        if not ports:
            ports = [int(p) for p in EXCEPT.findall(profile)]
            kind = "require-not exception"
        if not ports:
            continue
        port = ports[0]
        listener = socket.socket()
        try:
            listener.bind(("127.0.0.1", port))
        except OSError as err:
            print(f"{f.name}: cannot rebind 127.0.0.1:{port}: {err}")
            continue
        listener.listen(64)
        try:
            ok, errs = rate(profile, params, port, runs)
            print(f"{f.name}: {kind} ports {ports}; full profile ok={ok}/{runs} {errs}")
            if ok == runs:
                continue
            debug = profile.replace("(version 1)", "(version 1)\n(debug deny)", 1)
            print(f"  debug-deny run: {connect(debug, params, port)} (see the Sandbox log below)")
            for attempt in range(3):
                print(f"  timing {attempt + 1}: {timing(profile, params, port)}")
            parts = forms(profile)
            head = ["(version 1)", "(allow default)"]
            # The IP block (broad deny + localhost allows) and the unix-socket
            # outbound denies are separate: the unix ones reference -D params.
            net = [p for p in parts if p.startswith(("(deny network-outbound", "(allow network-outbound"))
                   and "unix-socket" not in p]
            unix = [p for p in parts if p.startswith("(deny network-outbound") and "unix-socket" in p]
            rest = [p for p in parts if p not in net and not p.startswith("(version") and p != "(allow default)"]
            print(f"  forms: {len(net)} IP network, {len(unix)} unix-socket outbound denies, {len(rest)} other")
            for name, value in sorted(params.items()):
                kind_p = ("socket" if os.path.exists(value) and not os.path.isdir(value) and not os.path.isfile(value)
                          else "dir" if os.path.isdir(value) else "file" if os.path.isfile(value)
                          else "MISSING")
                print(f"  param {name}={value} [{kind_p}]")
            # Direct test of the existence hypothesis: the unix-socket deny
            # params present (throwaway unix listeners at their exact paths)
            # vs absent, for literal and path-literal.
            unix_params = sorted(set(re.findall(r'\(param "([^"]+)"\)', "\n".join(unix))))
            def with_sockets(fn_inner):
                made, created_dirs = [], []
                try:
                    for name in unix_params:
                        path = params.get(name, "")
                        if not path or os.path.exists(path):
                            continue
                        parent = os.path.dirname(path)
                        if not os.path.isdir(parent):
                            os.makedirs(parent, exist_ok=True)
                            created_dirs.append(parent)
                        try:
                            sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                            sock.bind(path)
                            sock.listen(4)
                            made.append((sock, path))
                        except OSError as err:
                            print(f"  exist-test: cannot bind {path}: {err}")
                    return fn_inner(), len(made)
                finally:
                    for sock, path in made:
                        sock.close()
                        try:
                            os.unlink(path)
                        except OSError:
                            pass
            for form_name, block in (("literal", unix),
                                     ("path-literal", [u.replace("(literal (param", "(path-literal (param") for u in unix])):
                text = "\n".join(head + net + block)
                absent = rate_with(connect_py, text, params, port, 5)
                present, bound = with_sockets(lambda: rate_with(connect_py, text, params, port, 5))
                print(f"  exist-test: {form_name}: sockets absent ok={absent[0]}/5; "
                      f"sockets present ({bound} bound) ok={present[0]}/5 {present[1]}")
            for k, u in enumerate(unix):
                pl = u.replace("(literal (param", "(path-literal (param")
                for conn_name, fn, _ in connectors()[:2]:
                    ok_pl, errs_pl = rate_with(fn, "\n".join(head + net + [pl]), params, port, 3)
                    print(f"  pair: IP block + unix deny {k} as path-literal ({conn_name}): ok={ok_pl}/3 {errs_pl}")
                for conn_name, fn, _ in connectors():
                    ok_u, errs_u = rate_with(fn, "\n".join(head + net + [u]), params, port, 3)
                    print(f"  pair: IP block + unix deny {k} ({conn_name}): ok={ok_u}/3 {errs_u}")
                print("    " + u.replace("\n", "\n    ")[:600])
            variants = {
                "minimal network block only": head + net,
                "network block moved to the end": head + rest + net,
                "network block + first other form after it": head + net + rest[:1],
                "network block + all other forms after it (as rendered)": head + net + rest,
            }
            for label, chosen in variants.items():
                ok_v, errs_v = rate("\n".join(chosen), params, port, 5)
                print(f"  variant: {label}: ok={ok_v}/5 {errs_v}")
            # Confound matrix on the network block alone: connector (python
            # connect() vs nc), -D parameters, and the UDP allow line.
            tcp_only = [p for p in net if "remote udp" not in p]
            for conn_name, fn, binary in connectors():
                print(f"  signature: {conn_name}: {binary}: {signature(binary)}")
            for conn_name, fn, _ in connectors():
                for p_name, p_val in (("no -D", {}), ("-D", params)):
                    for u_name, block in (("tcp+udp", net), ("tcp only", tcp_only)):
                        ok_m, errs_m = rate_with(fn, "\n".join(head + block), p_val, port, 5)
                        print(f"  matrix: {conn_name:6} {p_name:5} {u_name:8} ok={ok_m}/5 {errs_m}")
            ok_f, errs_f = rate_with(connect_py, profile, params, port, 5)
            print(f"  matrix: python full profile with -D ok={ok_f}/5 {errs_f}")
            # Port axis: the same network block with N rewritten, a listener
            # on that port, python and nc.
            for alt in (8080, 20000, 40000, 49151, 49152, 55000, 65000):
                alt_block = [p.replace(f"localhost:{port}", f"localhost:{alt}") for p in net]
                alt_listener = socket.socket()
                try:
                    alt_listener.bind(("127.0.0.1", alt))
                except OSError as err:
                    print(f"  port {alt}: cannot bind: {err}")
                    continue
                alt_listener.listen(16)
                try:
                    cells = []
                    for conn_name, fn, _ in connectors():
                        ok_p, _ = rate_with(fn, "\n".join(head + alt_block), params, alt, 3)
                        cells.append(f"{conn_name}={ok_p}/3")
                    print(f"  port {alt}: " + " ".join(cells))
                finally:
                    alt_listener.close()
            # Minimal pair: the passing network block, plus exactly one other
            # form of the rendered profile, appended after it; with and
            # without the -D parameters (the minimal probe passed none).
            base_ok, _ = rate("\n".join(head + net), {}, port, 5)
            print(f"  add-one: base (network block, no -D params): ok={base_ok}/5")
            base_ok, _ = rate("\n".join(head + net), params, port, 5)
            print(f"  add-one: base (network block, with -D params): ok={base_ok}/5")
            for j, extra in enumerate(rest):
                ok_j, errs_j = rate("\n".join(head + net + [extra]), params, port, 3)
                if ok_j < 3:
                    print(f"  add-one: form {j} flips it (ok={ok_j}/3 {errs_j}):")
                    print("    " + extra.replace("\n", "\n    ")[:1500])
            print(f"  add-one: {len(rest)} other forms tried")
            for i, part in enumerate(parts):
                if part.startswith("(version") or part == "(allow default)":
                    continue
                reduced = "\n".join(p for j, p in enumerate(parts) if j != i)
                ok_i, _ = rate(reduced, params, port, 3)
                if ok_i:
                    print(f"  dropping form {i} restores the connect ({ok_i}/3):")
                    print("    " + part.replace("\n", "\n    ")[:1500])
            (dump / (f.stem + ".sb")).write_text(profile)
        finally:
            listener.close()


if __name__ == "__main__":
    main()
