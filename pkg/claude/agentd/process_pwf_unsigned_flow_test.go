package agentd_test

import (
	"bytes"
	"crypto/ed25519"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/pwf"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func pwfFile(t *testing.T, keys ...paf.SigningKey) []byte {
	t.Helper()
	_, ref, err := pwf.ParseTemplate([]byte(pwfChildSource))
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, pwf.Write(&buf, pwf.Bundle{Root: ref, Templates: []pwf.Template{{Head: true, Source: []byte(pwfChildSource)}}},
		pwf.WriteOptions{Keys: keys}))
	return buf.Bytes()
}

// By default an unsigned file imports. With the policy on it is still shown in
// a review but cannot be applied, while a file signed by any key can.
func TestProcessWorkflowImportRefusesUnsignedByPolicy(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	unsigned := pwfFile(t)
	signed := pwfFile(t, paf.SigningKey{Key: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32)), KeyID: "someone", Signer: &paf.Signer{Kind: "instance"}})

	review := func(file []byte) (string, string) {
		rec := pwfRaw(t, f, http.MethodPost, "/v1/process/import", file)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var view struct{ Refused, ReviewDigest string }
		testharness.DecodeJSON(t, rec, &view)
		return view.Refused, view.ReviewDigest
	}

	refused, _ := review(unsigned)
	assert.Empty(t, refused, "off by default")

	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte(`{"features":{"processes":true},"processes":{"refuse_unsigned_imports":true}}`), 0o644))

	refused, digest := review(unsigned)
	assert.Contains(t, refused, "without a valid signature")
	apply := pwfRaw(t, f, http.MethodPost, "/v1/process/import?apply=1&review="+digest, unsigned)
	require.Equal(t, http.StatusForbidden, apply.Code, apply.Body.String())
	assert.Contains(t, apply.Body.String(), "process_import_unsigned")
	missing := pwfRaw(t, f, http.MethodGet, "/v1/process/templates/pwf-review/export", nil)
	assert.Equal(t, http.StatusNotFound, missing.Code, "nothing was stored")

	refused, _ = review(signed)
	assert.Empty(t, refused, "an unknown key is still a signature")
	applied := pwfApply(t, f, signed, "")
	require.Equal(t, http.StatusOK, applied.Code, applied.Body.String())
}
