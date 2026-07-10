package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Dir write-proof client — the CLI half of agentd's spawn-dir write-proof
// (pkg/claude/agentd/spawn_dir_proof.go). When a sandboxed agent asks the
// daemon to launch another agent (spawn / --join-group), the daemon may
// refuse with a 403 challenge: prove you can write in the directories the
// new agent would get write access to, by creating an empty token-named file
// in each. This client runs the dance transparently: the CLI executes inside
// the calling agent's sandbox, so the file creation succeeds exactly when
// the caller's own sandbox allows writing there — which is the proof.

// WriteProofRequiredCode is the daemon error code that carries a dir
// write-proof challenge.
const WriteProofRequiredCode = "write_proof_required"

// writeProofChallenge mirrors the "write_proof" object of the daemon's 403
// challenge response.
type writeProofChallenge struct {
	Token    string   `json:"token"`
	Filename string   `json:"filename"`
	Dirs     []string `json:"dirs"`
}

// writeProofChallengeFromError extracts a challenge from a DaemonError, or
// nil when err is anything else. The daemon runs as the human and is
// trusted, but the filename is still shape-checked so a malformed response
// can never make the CLI write outside the challenged directories.
func writeProofChallengeFromError(err error) *writeProofChallenge {
	var de *DaemonError
	if !errors.As(err, &de) || de.Code != WriteProofRequiredCode || len(de.Raw) == 0 {
		return nil
	}
	var body struct {
		WriteProof writeProofChallenge `json:"write_proof"`
	}
	if json.Unmarshal(de.Raw, &body) != nil {
		return nil
	}
	ch := body.WriteProof
	if ch.Token == "" || len(ch.Dirs) == 0 ||
		!strings.HasPrefix(ch.Filename, ".tclaude-") ||
		strings.ContainsAny(ch.Filename, "/\\") ||
		filepath.Base(ch.Filename) != ch.Filename {
		return nil
	}
	return &ch
}

// answerWriteProofChallenge creates the proof file in every challenged
// directory. On success it returns a cleanup func that best-effort removes
// them again (the daemon deletes them during verification; the cleanup
// covers a retry that never reaches verification). On failure it removes
// whatever it managed to create and returns an error naming the directory
// the caller cannot write — the actionable "your sandbox does not allow
// this" message.
func answerWriteProofChallenge(ch *writeProofChallenge) (cleanup func(), err error) {
	created := make([]string, 0, len(ch.Dirs))
	remove := func() {
		for _, p := range created {
			_ = os.Remove(p)
		}
	}
	for _, dir := range ch.Dirs {
		path := filepath.Join(dir, ch.Filename)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			remove()
			return nil, fmt.Errorf(
				"cannot prove write access in %s: %v.\nA sandboxed agent may only launch "+
					"agents into directories its own sandbox can write to — otherwise the new "+
					"agent would hand it write access it does not have. Pick a directory you "+
					"can write to, or ask the human to do the launch", dir, err)
		}
		_ = f.Close()
		created = append(created, path)
	}
	return remove, nil
}

// DaemonRequestWithWriteProof is DaemonRequest plus transparent handling of
// the daemon's dir write-proof challenge. mkBody builds the request body for
// a given write_proof_token — it is called with "" for the first attempt
// and, if the daemon answers with a challenge the CLI can satisfy (create
// the token-named file in each challenged directory), once more with the
// challenge's token. Any other error — including a challenge whose proof
// file cannot be created because the caller's sandbox forbids the write —
// is returned as-is for the caller to print.
func DaemonRequestWithWriteProof(method, path string, mkBody func(writeProofToken string) any, out any, opts DaemonOpts) error {
	err := DaemonRequest(method, path, mkBody(""), out, opts)
	ch := writeProofChallengeFromError(err)
	if ch == nil {
		return err
	}
	cleanup, ansErr := answerWriteProofChallenge(ch)
	if ansErr != nil {
		return ansErr
	}
	defer cleanup()
	retryErr := DaemonRequest(method, path, mkBody(ch.Token), out, opts)
	if writeProofChallengeFromError(retryErr) != nil {
		// A second challenge in a row means the daemon would loop us —
		// surface it rather than retrying forever.
		return fmt.Errorf("daemon demanded a new write-permission proof on the proved retry: %w", retryErrOrErr(retryErr, err))
	}
	return retryErr
}

// retryErrOrErr prefers the retry error but falls back to the original —
// both are DaemonErrors here; this only guards a nil slip.
func retryErrOrErr(retryErr, err error) error {
	if retryErr != nil {
		return retryErr
	}
	return err
}
