#!/bin/bash

# This is an evidence probe, not a product smoke. It must only run on an
# disposable GitHub-hosted macOS runner because it briefly enables PF and
# installs rules in a child of macOS's existing com.apple/* anchor.
set -euo pipefail

if [[ "$(uname -s)" != Darwin || -z "${GITHUB_ACTIONS:-}" ]]; then
  echo "refusing to run outside GitHub Actions on macOS" >&2
  exit 1
fi

anchor="com.apple/tclaude-ci-${GITHUB_RUN_ID:?}"
runner_user="$(id -un)"
probe_group="tclaude_pf_${GITHUB_RUN_ID}"
rules="${RUNNER_TEMP:?}/tclaude-pf-rules.conf"
listener="${RUNNER_TEMP}/tclaude-pf-listener.py"
pf_token=""
listener_pid=""
group_created=0

cleanup() {
  status=$?
  trap - EXIT INT TERM
  if [[ -n "$listener_pid" ]]; then
    kill "$listener_pid" 2>/dev/null || true
    wait "$listener_pid" 2>/dev/null || true
  fi
  sudo pfctl -a "$anchor" -F all 2>/dev/null || true
  if [[ -n "$pf_token" ]]; then
    sudo pfctl -X "$pf_token" 2>/dev/null || true
  fi
  if (( group_created )); then
    sudo dseditgroup -o delete "$probe_group" 2>/dev/null || true
  fi
  exit "$status"
}
trap cleanup EXIT INT TERM

echo "macOS: $(sw_vers -productVersion) ($(sw_vers -buildVersion))"
echo "runner image: ${ImageOS:-unknown} ${ImageVersion:-unknown}"
echo "interfaces:"
ifconfig -l
echo "PF status before probe:"
sudo pfctl -s info || true
echo "PF anchors before probe:"
sudo pfctl -s Anchors || true
echo "/etc/pf.conf anchor hooks:"
grep -E '^(nat-|rdr-|scrub-)?anchor ' /etc/pf.conf || true

# Bind all test endpoints before changing PF. Each accepted connection returns
# the name of the port reached, making rdr observable without external network.
read -r block_port original_port redirect_port gid_port < <(
  python3 - <<'PY'
import socket
sockets = []
for _ in range(4):
    sock = socket.socket()
    sock.bind(("127.0.0.1", 0))
    sockets.append(sock)
print(*(sock.getsockname()[1] for sock in sockets))
for sock in sockets:
    sock.close()
PY
)

sudo dseditgroup -o create "$probe_group"
group_created=1
sudo dseditgroup -o edit -a "$runner_user" -t user "$probe_group"
probe_gid="$(dscl . -read "/Groups/$probe_group" PrimaryGroupID | awk '{print $2}')"

cat >"$listener" <<'PY'
import socket
import sys
import threading

def serve(port, label):
    server = socket.socket()
    server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    server.bind(("127.0.0.1", port))
    server.listen()
    while True:
        conn, _ = server.accept()
        with conn:
            conn.recv(4096)
            body = (label + "\n").encode()
            conn.sendall(b"HTTP/1.0 200 OK\r\nContent-Length: "
                         + str(len(body)).encode() + b"\r\n\r\n" + body)

ports = list(map(int, sys.argv[1:]))
labels = ["BLOCK_TARGET", "ORIGINAL", "REDIRECTED", "GID_TARGET"]
for port, label in zip(ports, labels):
    threading.Thread(target=serve, args=(port, label), daemon=True).start()
threading.Event().wait()
PY
python3 "$listener" "$block_port" "$original_port" "$redirect_port" "$gid_port" &
listener_pid=$!

curl_probe() {
  curl --fail --silent --show-error --noproxy '*' --connect-timeout 2 \
    "http://127.0.0.1:$1/"
}

for port in "$block_port" "$original_port" "$redirect_port" "$gid_port"; do
  for _ in {1..20}; do
    if curl_probe "$port" >/dev/null 2>&1; then
      continue 2
    fi
    sleep 0.1
  done
  echo "listener on port $port did not become ready" >&2
  exit 1
done

cat >"$rules" <<EOF
rdr pass on lo0 inet proto tcp from any to 127.0.0.1 port $original_port -> 127.0.0.1 port $redirect_port
block return-rst out quick on lo0 inet proto tcp from any to 127.0.0.1 port $block_port
block return out quick on lo0 inet proto tcp from any to 127.0.0.1 port $gid_port group $probe_gid
EOF

echo "probe rules:"
cat "$rules"
sudo pfctl -vnf "$rules"
sudo pfctl -a "$anchor" -f "$rules"
enable_output="$(sudo pfctl -E 2>&1)"
echo "$enable_output"
pf_token="$(sed -nE 's/.*Token[[:space:]]*:[[:space:]]*([0-9]+).*/\1/p' <<<"$enable_output" | tail -1)"

block_error="${RUNNER_TEMP}/tclaude-pf-block-curl.err"
set +e
block_time="$(curl --fail --silent --show-error --noproxy '*' \
  --connect-timeout 2 --output /dev/null --write-out '%{time_total}' \
  "http://127.0.0.1:$block_port/" 2>"$block_error")"
block_exit=$?
set -e
block_stderr="$(<"$block_error")"
printf 'scoped block evidence: curl exit=%s time=%ss stderr=%s\n' \
  "$block_exit" "$block_time" "$block_stderr"
if [[ "$block_exit" -ne 7 ]]; then
  echo "scoped return-rst wanted curl exit 7 (connection refused), got $block_exit" >&2
  exit 1
fi
if ! awk -v elapsed="$block_time" 'BEGIN { exit !(elapsed < 0.5) }'; then
  echo "scoped return-rst was not immediate: ${block_time}s" >&2
  exit 1
fi
echo "--- PASS: macOSPFScopedBlock (${block_time}s)"

rdr_result="$(curl_probe "$original_port")"
if [[ "$rdr_result" != "REDIRECTED" ]]; then
  echo "rdr reached '$rdr_result', wanted 'REDIRECTED'" >&2
  exit 1
fi
echo "--- PASS: macOSPFRedirect (0.00s)"

# Adding the account to a supplementary group must not be mistaken for PF
# matching it. The macOS kernel records the socket credential's effective gid.
echo "credential used for supplementary-group assertion:"
sudo -u "$runner_user" id
supplementary_result="$(sudo -u "$runner_user" \
  curl --fail --silent --show-error --noproxy '*' --connect-timeout 2 \
    "http://127.0.0.1:$gid_port/")"
if [[ "$supplementary_result" != "GID_TARGET" ]]; then
  echo "supplementary group unexpectedly matched PF group rule" >&2
  exit 1
fi

echo "credential used for effective-gid assertion:"
sudo -u "$runner_user" -g "$probe_group" id
if sudo -u "$runner_user" -g "$probe_group" \
    curl --fail --silent --show-error --noproxy '*' --connect-timeout 2 \
      "http://127.0.0.1:$gid_port/" >/dev/null 2>&1; then
  echo "effective gid did not match PF group rule" >&2
  exit 1
fi
echo "--- PASS: macOSPFGIDMatchIsEffectiveOnly (0.00s)"

echo "PF anchor after assertions:"
sudo pfctl -a "$anchor" -sr
sudo pfctl -a "$anchor" -sn
