package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/legacy"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func consentFixture(t *testing.T) string {
	t.Helper()
	b := &agentbundle.Bundle{Manifest: agentbundle.Manifest{Format: agentbundle.Format, FormatVersion: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339), TclaudeVersion: "test",
		Agent: agentbundle.Definition{Name: "reviewer", Harness: "claude", Profile: json.RawMessage(`{}`)}}}
	var buf bytes.Buffer
	require.NoError(t, legacy.WriteAgentBundle(b, &buf, legacy.WriteOptions{Producer: paf.Producer{Name: "test"}}))
	p := filepath.Join(testutil.CanonicalTempDir(t), "reviewer.paf")
	require.NoError(t, os.WriteFile(p, buf.Bytes(), 0o600))
	return p
}

func TestPAFConsentPromptsBeforeAnyRequest(t *testing.T) {
	file := consentFixture(t)
	out := filepath.Join(filepath.Dir(file), "out.paf")
	p := &pafConsentParams{File: file, Out: out, Audience: "self"}
	var stdout, stderr bytes.Buffer

	// Without a terminal the human must pass --yes after reading the summary.
	assert.Equal(t, rcInvalidArg, runPAFConsent(p, strings.NewReader(""), &stdout, &stderr, false))
	assert.Contains(t, stdout.String(), "would go to self")
	assert.Contains(t, stdout.String(), "Redactions:  none; the history is carried verbatim")
	assert.Contains(t, stderr.String(), "pass --yes")

	// Declining records nothing.
	stdout.Reset()
	assert.Equal(t, rcOK, runPAFConsent(p, strings.NewReader("n\n"), &stdout, &stderr, true))
	assert.Contains(t, stdout.String(), "No consent recorded.")
	assert.NoFileExists(t, out)

	assert.Equal(t, rcInvalidArg, runPAFConsent(&pafConsentParams{File: file, Audience: "self"}, strings.NewReader(""), &stdout, &stderr, true), "--out is required")
}
