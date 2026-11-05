#!/usr/bin/env python3
"""TEMPORARY (#3999, awb tcl-gejtfc; remove before merge).

Replays the Seatbelt profiles tclaude rendered (dumped through
TCLAUDE_TEMP_SEATBELT_PROFILE_DUMP_DIR) against a connect to each allowed
localhost:N port, then drops one top-level rule at a time to find the rule
that turns the allowed connect into EPERM on macOS 27.2.

Usage: seatbelt-profile-replay.py <dump-dir> [runs]
"""
import json
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
            net = [p for p in parts if p.startswith(("(deny network-outbound", "(allow network-outbound"))]
            rest = [p for p in parts if p not in net and not p.startswith("(version") and p != "(allow default)"]
            head = ["(version 1)", "(allow default)"]
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
            for conn_name, fn in (("python", connect_py), ("nc", connect)):
                for p_name, p_val in (("no -D", {}), ("-D", params)):
                    for u_name, block in (("tcp+udp", net), ("tcp only", tcp_only)):
                        ok_m, errs_m = rate_with(fn, "\n".join(head + block), p_val, port, 5)
                        print(f"  matrix: {conn_name:6} {p_name:5} {u_name:8} ok={ok_m}/5 {errs_m}")
            ok_f, errs_f = rate_with(connect_py, profile, params, port, 5)
            print(f"  matrix: python full profile with -D ok={ok_f}/5 {errs_f}")
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
