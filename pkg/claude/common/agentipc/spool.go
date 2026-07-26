package agentipc

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/common"
)

// The experimental file-spool transport carries the same HTTP
// request/response traffic as the agentd Unix socket, but over plain
// filesystem operations (write/rename/read/unlink) in a per-agent spool
// directory. It exists for sandboxes that deny every socket syscall — e.g.
// Codex's Linux restricted-network seccomp blocks connect(2) for AF_UNIX
// too — where an agent still needs to coordinate through agentd.
//
// Layout of one agent's spool directory (minted at spawn, bound to the
// agent's conv in SQLite — see db.CreateSpoolBinding):
//
//	<dir>/req/   client writes <request-id>.json envelopes (tmp+rename)
//	<dir>/resp/  daemon writes <request-id>.json envelopes (tmp+rename)
//
// Possession of the bound directory IS the caller identity: the daemon
// stamps the binding's conv-id on every request consumed from it. The
// directory name is an unguessable random id, INTENDED to be paired with
// sandbox profiles that deny the spool root and allow-carve only the
// agent's own directory. That carve-out does not exist yet — it is part
// of the sandbox-profile follow-up (TCL-748) — so today, while the flag
// is on, any process that can read ~/.tclaude/api can enumerate spool
// directories and read other agents' envelopes; request/response bodies
// also touch disk, which the socket transport never does. That is the
// central reason this transport is gated behind an experimental flag.
// The same-UID caveat is separate and permanent: unlike SO_PEERCRED, any
// unsandboxed same-UID process that reads the directory can speak as that
// agent — accepted, because a same-UID process can already impersonate
// any agent via its tmux pane or /proc/<pid>/mem.

// SpoolEnv points an agent's tclaude CLI at its private spool directory.
// Set at spawn by session.ApplyAgentSpoolEnv; absolute paths only.
const SpoolEnv = "TCLAUDE_AGENTD_SPOOL"

// TransportEnv optionally forces the client transport selection. The only
// recognised value is "spool"; anything else keeps the default behaviour
// (Unix socket preferred, spool as fallback when no socket is dialable).
const TransportEnv = "TCLAUDE_AGENTD_TRANSPORT"

// FileTransportFlagEnv is the experimental feature flag. When set to
// "1"/"true" on the daemon (and on the process running `tclaude session
// new`), agentd consumes spool requests and every spawned session gets a
// provisioned spool directory alongside its socket access.
const FileTransportFlagEnv = "TCLAUDE_EXPERIMENTAL_FILE_TRANSPORT"

// TransportSpool is the TransportEnv value that forces the spool transport.
const TransportSpool = "spool"

// FileTransportEnabled reports whether the experimental file-spool
// transport feature flag is set in this process's environment.
func FileTransportEnabled() bool {
	switch strings.TrimSpace(os.Getenv(FileTransportFlagEnv)) {
	case "1", "true", "TRUE", "True":
		return true
	}
	return false
}

// SpoolRoot returns the parent directory holding every agent spool
// directory. It lives under the agent-reachable api/ surface — NOT under
// data/ — for the same reason the socket does: sandboxes deny
// ~/.tclaude/data as one subtree while an agent's own spool directory is
// individually allow-listed.
func SpoolRoot() string {
	apiDir := common.TclaudeAPIDir()
	if apiDir == "" {
		return ""
	}
	return filepath.Join(apiDir, "spool")
}

// SpoolDirFromEnv returns the spool directory this process was spawned
// with, or "" when unset. Only an absolute path is accepted — mirroring
// ExplicitSocketPath, a relative value must not turn into an ambient-CWD
// lookup.
func SpoolDirFromEnv() string {
	if dir := strings.TrimSpace(os.Getenv(SpoolEnv)); filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	return ""
}

// SpoolForced reports whether TransportEnv explicitly forces the spool
// transport.
func SpoolForced() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(TransportEnv)), TransportSpool)
}

// SpoolReqDir / SpoolRespDir are the two halves of one spool directory.
func SpoolReqDir(dir string) string  { return filepath.Join(dir, "req") }
func SpoolRespDir(dir string) string { return filepath.Join(dir, "resp") }

// NewSpoolID mints the random identifier used both as a spool directory
// name and as a request id. 128 bits from crypto/rand: the directory name
// doubles as a capability (the sandbox path grant), so it must be
// unguessable.
func NewSpoolID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint spool id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// SpoolRequest is the on-disk request envelope: one HTTP request,
// serialized. Body is raw bytes (JSON base64-encodes it). RequestURI is
// the path+query form ("/v1/info"), never a full URL — the transport has
// no host.
type SpoolRequest struct {
	Method     string      `json:"method"`
	RequestURI string      `json:"request_uri"`
	Header     http.Header `json:"header,omitempty"`
	Body       []byte      `json:"body,omitempty"`
}

// SpoolResponse is the on-disk response envelope.
type SpoolResponse struct {
	Status int         `json:"status"`
	Header http.Header `json:"header,omitempty"`
	Body   []byte      `json:"body,omitempty"`
}

// spoolFileExt is the suffix a complete envelope file carries. Everything
// else in a req/ dir (tmp files mid-write, claim files mid-processing) is
// ignored by scanners.
const spoolFileExt = ".json"

// SpoolEnvelopeFile reports whether name looks like a complete envelope
// file a scanner should pick up.
func SpoolEnvelopeFile(name string) bool {
	return !strings.HasPrefix(name, ".") && strings.HasSuffix(name, spoolFileExt)
}

// SpoolEnvelopePath returns the full path of the envelope file for one
// request id inside dir (a req/ or resp/ dir).
func SpoolEnvelopePath(dir, id string) string {
	return filepath.Join(dir, id+spoolFileExt)
}

// WriteSpoolFile publishes data at path atomically: write to a dotted tmp
// file in the same directory, then rename into place. Readers therefore
// only ever observe complete envelopes. Deliberately no fsync: a HOST
// crash may surface a torn/empty envelope on some filesystems, which the
// daemon answers with a 400 (request) or the client rejects on decode
// (response) — acceptable for an experimental transport whose callers
// already own retry behaviour.
func WriteSpoolFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmpName, path)
	}
	if werr != nil {
		_ = os.Remove(tmpName)
		return werr
	}
	return nil
}

func EncodeSpoolRequest(r SpoolRequest) ([]byte, error)  { return json.Marshal(r) }
func EncodeSpoolResponse(r SpoolResponse) ([]byte, error) { return json.Marshal(r) }

func DecodeSpoolRequest(data []byte) (SpoolRequest, error) {
	var r SpoolRequest
	err := json.Unmarshal(data, &r)
	return r, err
}

func DecodeSpoolResponse(data []byte) (SpoolResponse, error) {
	var r SpoolResponse
	err := json.Unmarshal(data, &r)
	return r, err
}
