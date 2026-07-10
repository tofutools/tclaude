package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func challengeErr(raw string) error {
	return &DaemonError{Status: 403, Code: WriteProofRequiredCode, Raw: []byte(raw)}
}

func TestWriteProofChallengeFromError(t *testing.T) {
	ch := writeProofChallengeFromError(challengeErr(
		`{"code":"write_proof_required","write_proof":{"token":"abc","filename":".tclaude-write-proof-abc","dirs":["/x"]}}`))
	require.NotNil(t, ch)
	assert.Equal(t, "abc", ch.Token)
	assert.Equal(t, []string{"/x"}, ch.Dirs)

	// Not a challenge error at all.
	assert.Nil(t, writeProofChallengeFromError(os.ErrPermission))
	assert.Nil(t, writeProofChallengeFromError(&DaemonError{Status: 403, Code: "forbidden", Raw: []byte(`{}`)}))

	// Malformed filenames must be rejected — the CLI never writes outside
	// the challenged dirs, even on a corrupt response.
	for _, filename := range []string{"", "proof", "../.tclaude-x", ".tclaude-write-proof-a/b", "/etc/x"} {
		bad := writeProofChallengeFromError(challengeErr(
			`{"write_proof":{"token":"abc","filename":"` + filename + `","dirs":["/x"]}}`))
		assert.Nilf(t, bad, "filename %q must be rejected", filename)
	}
}

func TestAnswerWriteProofChallenge_CreatesAndCleans(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	ch := &writeProofChallenge{Token: "t", Filename: ".tclaude-write-proof-t", Dirs: []string{dirA, dirB}}

	cleanup, err := answerWriteProofChallenge(ch)
	require.NoError(t, err)
	for _, d := range []string{dirA, dirB} {
		fi, statErr := os.Lstat(filepath.Join(d, ch.Filename))
		require.NoError(t, statErr)
		assert.True(t, fi.Mode().IsRegular())
	}
	cleanup()
	for _, d := range []string{dirA, dirB} {
		_, statErr := os.Lstat(filepath.Join(d, ch.Filename))
		assert.True(t, os.IsNotExist(statErr), "cleanup must remove the proof files")
	}
}

func TestAnswerWriteProofChallenge_UnwritableDir(t *testing.T) {
	writable := t.TempDir()
	sealed := t.TempDir()
	require.NoError(t, os.Chmod(sealed, 0o555))
	t.Cleanup(func() { _ = os.Chmod(sealed, 0o755) })

	ch := &writeProofChallenge{Token: "t", Filename: ".tclaude-write-proof-t", Dirs: []string{writable, sealed}}
	_, err := answerWriteProofChallenge(ch)
	require.Error(t, err)
	assert.Contains(t, err.Error(), sealed, "the error must name the unwritable dir")

	// The file created in the writable dir before the failure is rolled back.
	_, statErr := os.Lstat(filepath.Join(writable, ch.Filename))
	assert.True(t, os.IsNotExist(statErr))
}
