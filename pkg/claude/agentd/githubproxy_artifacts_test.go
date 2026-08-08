package agentd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// humanBytes renders every size an agent reads in an artifact listing or a
// refusal, including the one in the refusal's own threshold. Its unit ladder is
// the kind of arithmetic that is right until it silently is not — an unclamped
// exponent indexes past the end of "KMGT" and panics the daemon.
func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{2048, "2.0 KiB"},
		{1024*1024 - 1, "1024.0 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{512 << 20, "512.0 MiB"},
		{700 << 20, "700.0 MiB"},
		{1 << 30, "1.0 GiB"},
		{1 << 40, "1.0 TiB"},
		// Past the last unit the exponent must CLAMP rather than index on: a
		// petabyte reads as 1024 TiB, and does not panic.
		{1 << 50, "1024.0 TiB"},
		{1 << 60, "1048576.0 TiB"},
	} {
		assert.Equal(t, tc.want, humanBytes(tc.n), "humanBytes(%d)", tc.n)
	}
}

// The download budget is stated to callers in humanBytes terms, so the constant
// and the rendering have to agree — a cap that refuses at "512 MiB" while the
// help text promises 512 MiB is only true if this holds.
func TestArtifactBudgetRendersAsTheDocumentedFigure(t *testing.T) {
	assert.Equal(t, "512.0 MiB", humanBytes(maxGHArtifactBytes))
}

func TestValidateGHArtifactName(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		for _, name := range []string{
			"coverage", "test-results", "build (ubuntu-latest)", "logs_1.22", "täckning",
		} {
			got, fault := validateGHArtifactName(name)
			assert.Nil(t, fault, "%q should be a legal artifact name", name)
			assert.Equal(t, name, got)
		}
	})

	// An empty name is not an error — it means "every live artifact", which is
	// the documented default.
	t.Run("empty means every artifact", func(t *testing.T) {
		got, fault := validateGHArtifactName("  ")
		assert.Nil(t, fault)
		assert.Empty(t, got)
	})

	t.Run("refused", func(t *testing.T) {
		for _, name := range []string{
			"-n",                     // reads as a flag
			"--dir=/etc",             // reads as a flag with a value
			"../../etc/passwd",       // steers where gh writes
			`a\b`,                    // ditto, on the other separator
			"tab\there",              // control character
			"a\x00b",                 // NUL
			"quote\"d",               // GitHub refuses it too
			"glob*",                  // ditto
			strings.Repeat("x", 201), // over the length bound
		} {
			_, fault := validateGHArtifactName(name)
			assert.NotNil(t, fault, "%q must not survive being called an artifact name", name)
		}
	})
}
