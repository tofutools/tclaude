package agentd

import (
	"crypto/hmac"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

const (
	// A proof may wait behind the longest supported --ask-human approval
	// (five minutes), so keep it valid long enough for that round trip while
	// still making leaked challenges short-lived.
	spawnCwdProofTTL = 10 * time.Minute
	spawnCwdNonceLen = 16
	spawnCwdMACLen   = sha256.Size
)

var spawnCwdProofSecret = func() []byte {
	b := make([]byte, 32)
	if _, err := cryptorand.Read(b); err != nil {
		panic("agentd: crypto/rand failed generating spawn cwd proof secret: " + err.Error())
	}
	return b
}()

type spawnCwdProofRequest struct {
	Cwd string `json:"cwd"`
}

// handleSpawnCwdProof issues a stateless, signed challenge bound to the
// authenticated agent and the canonical target directory. The daemon does not
// create the marker itself: the caller's CLI must do that while it is still
// inside the parent's filesystem sandbox. Human callers are the trust root and
// are told no proof is required.
func handleSpawnCwdProof(w http.ResponseWriter, r *http.Request) {
	caller, isHuman, ok := authedCaller(w, r)
	if !ok {
		return
	}
	if isHuman {
		writeJSON(w, http.StatusOK, map[string]any{"required": false})
		return
	}

	var body spawnCwdProofRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "json", err.Error())
			return
		}
	}
	cwd, err := canonicalSpawnProofCwd(body.Cwd)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_cwd", err.Error())
		return
	}
	proof, err := issueSpawnCwdProof(caller, cwd, time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "random", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"required":    true,
		"proof":       proof,
		"cwd":         cwd,
		"marker_path": filepath.Join(cwd, clcommon.SpawnCwdProofPrefix+proof),
	})
}

func canonicalSpawnProofCwd(raw string) (string, error) {
	cwd, err := resolveSpawnCwd(raw)
	if err != nil {
		return "", err
	}
	if cwd == "" {
		cwd, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve daemon working directory: %v", err)
		}
	}
	real, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve working directory %s: %v", cwd, err)
	}
	abs, err := filepath.Abs(real)
	if err != nil {
		return "", fmt.Errorf("resolve working directory %s: %v", real, err)
	}
	return filepath.Clean(abs), nil
}

func issueSpawnCwdProof(caller, cwd string, now time.Time) (string, error) {
	payload := make([]byte, 8+spawnCwdNonceLen)
	binary.BigEndian.PutUint64(payload[:8], uint64(now.Add(spawnCwdProofTTL).Unix()))
	if _, err := cryptorand.Read(payload[8:]); err != nil {
		return "", fmt.Errorf("generate spawn cwd proof: %v", err)
	}
	mac := spawnCwdProofMAC(caller, cwd, payload)
	return base64.RawURLEncoding.EncodeToString(append(payload, mac...)), nil
}

func spawnCwdProofMAC(caller, cwd string, payload []byte) []byte {
	mac := hmac.New(sha256.New, spawnCwdProofSecret)
	_, _ = mac.Write([]byte(caller))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(cwd))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}

// validateSpawnCwdWriteProof verifies that proof was issued to caller for cwd,
// then checks the exact marker. A valid token without a marker is intentionally
// insufficient: only creating the marker demonstrates that the parent
// process's own sandbox admitted a write in the target directory.
//
// The marker deliberately remains in place. The forked session launcher
// consumes it only after tmux has established the pane's cwd inode; that second
// check prevents a caller from swapping this now-validated pathname to a
// forbidden directory before launch.
func validateSpawnCwdWriteProof(caller, rawCwd, proof string, now time.Time) (string, *spawnFailure) {
	if caller == "" {
		return rawCwd, nil
	}
	proof = strings.TrimSpace(proof)
	if proof == "" {
		return "", &spawnFailure{http.StatusForbidden, "cwd_proof_required",
			"agent-initiated spawns require a cwd write proof; use `tclaude agent spawn` so the CLI can prove its sandbox may write the target directory"}
	}
	cwd, err := canonicalSpawnProofCwd(rawCwd)
	if err != nil {
		return "", &spawnFailure{http.StatusBadRequest, "invalid_cwd", err.Error()}
	}
	raw, err := base64.RawURLEncoding.DecodeString(proof)
	if err != nil || len(raw) != 8+spawnCwdNonceLen+spawnCwdMACLen {
		return "", invalidSpawnCwdProof("malformed proof token")
	}
	payload := raw[:8+spawnCwdNonceLen]
	gotMAC := raw[8+spawnCwdNonceLen:]
	if !hmac.Equal(gotMAC, spawnCwdProofMAC(caller, cwd, payload)) {
		return "", invalidSpawnCwdProof("proof was not issued for this agent and working directory")
	}
	expires := int64(binary.BigEndian.Uint64(payload[:8]))
	if now.Unix() > expires {
		return "", invalidSpawnCwdProof("proof expired before the spawn was authorised")
	}

	marker := filepath.Join(cwd, clcommon.SpawnCwdProofPrefix+proof)
	info, err := os.Lstat(marker)
	if err != nil {
		if os.IsNotExist(err) {
			return "", invalidSpawnCwdProof("proof marker is missing; the parent sandbox could not demonstrate write access")
		}
		return "", invalidSpawnCwdProof("cannot inspect proof marker: " + err.Error())
	}
	if !info.Mode().IsRegular() || info.Size() != 0 {
		return "", invalidSpawnCwdProof("proof marker must be an empty regular file")
	}
	return cwd, nil
}

func invalidSpawnCwdProof(detail string) *spawnFailure {
	return &spawnFailure{http.StatusForbidden, "invalid_cwd_proof", "invalid spawn cwd write proof: " + detail}
}
