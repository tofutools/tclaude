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
            print(f"{f.name}: allowed ports {ports}; full profile ok={ok}/{runs} {errs}")
            if ok == runs:
                continue
            parts = forms(profile)
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
