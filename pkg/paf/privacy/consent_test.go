package privacy

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/paf"
)

func consent(t *testing.T, a *paf.Archive, c ConsentRequest) *paf.Archive {
	t.Helper()
	var out bytes.Buffer
	_, err := AddConsent(a, &out, c, paf.Limits{}, paf.Producer{Name: "test"})
	require.NoError(t, err)
	b, err := paf.Open(bytes.NewReader(out.Bytes()), int64(out.Len()), paf.Options{})
	require.NoError(t, err)
	return b
}

func TestAddConsentBindsTheContent(t *testing.T) {
	a := redactSample(t, true)
	digest := paf.ContentDigest(a.Manifest.Entries)
	at := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

	b := consent(t, a, ConsentRequest{By: "operator", Audience: AudienceRecipients, Recipients: []string{"inst_7c21"}, At: at})
	assert.Equal(t, digest, paf.ContentDigest(b.Manifest.Entries), "content is copied byte for byte")
	p := b.Privacy()
	require.NotNil(t, p)
	assert.Equal(t, "shared", p.Sensitivity)
	assert.Equal(t, &paf.Audience{Kind: AudienceRecipients, Recipients: []string{"inst_7c21"}}, p.Audience)
	assert.Equal(t, []ConsentCheck{{Consent: paf.Consent{By: "operator", At: at, Audience: AudienceRecipients, ContentDigest: digest}, Current: true}}, CheckConsent(b))

	// A second consent keeps the first current: the content is unchanged.
	c := consent(t, b, ConsentRequest{By: "operator", Audience: AudiencePublic})
	checks := CheckConsent(c)
	require.Len(t, checks, 2)
	assert.True(t, checks[0].Current)
	assert.True(t, checks[1].Current)
	assert.Equal(t, "public", c.Privacy().Sensitivity)

	// Consent does not survive a change of content.
	var out bytes.Buffer
	_, err := Redact(c, &out, RedactOptions{DropMedia: true}, paf.Producer{Name: "test"})
	require.NoError(t, err)
	d, err := paf.Open(bytes.NewReader(out.Bytes()), int64(out.Len()), paf.Options{})
	require.NoError(t, err)
	assert.Empty(t, CheckConsent(d))
}

func TestStaleConsentIsReported(t *testing.T) {
	a := redactSample(t, true)
	var out bytes.Buffer
	require.NoError(t, paf.WithPrivacy(a, &out, paf.Privacy{Consent: []paf.Consent{{By: "operator", At: time.Now().UTC().Truncate(time.Second), Audience: AudienceSelf, ContentDigest: "0000000000000000000000000000000000000000000000000000000000000000"}}}, paf.Limits{}, paf.Producer{Name: "test"}))
	b, err := paf.Open(bytes.NewReader(out.Bytes()), int64(out.Len()), paf.Options{})
	require.NoError(t, err)
	checks := CheckConsent(b)
	require.Len(t, checks, 1)
	assert.False(t, checks[0].Current)
}

func TestConsentRequestChecks(t *testing.T) {
	a := redactSample(t, true)
	for _, c := range []ConsentRequest{
		{Audience: AudienceSelf},
		{By: "operator", Audience: "everyone"},
		{By: "operator", Audience: AudienceRecipients},
		{By: "operator", Audience: AudienceBoard},
	} {
		_, err := AddConsent(a, &bytes.Buffer{}, c, paf.Limits{}, paf.Producer{Name: "test"})
		assert.Error(t, err, "%+v", c)
	}
}

func TestSummarizeForConsent(t *testing.T) {
	s, err := SummarizeForConsent(redactSample(t, true), 1<<20)
	require.NoError(t, err)
	assert.Equal(t, 2, s.Turns)
	assert.Equal(t, 1, s.Agents)
	assert.Positive(t, s.Bytes)
	assert.True(t, Blocking(s.Findings), "this node's scan finds the token")

	s, err = SummarizeForConsent(redactSample(t, false), 1<<20)
	require.NoError(t, err)
	assert.Equal(t, 1, s.NativeOnly)
	assert.Zero(t, s.Turns)
}
