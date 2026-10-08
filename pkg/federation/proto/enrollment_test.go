package proto

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEnrollmentTokenPinsTermsAndSecret(t *testing.T) {
	id, e := NewIdentity()
	require.NoError(t, e)
	bearer, tok, e := NewEnrollmentToken(id, "profile-id", "rigs", 3, "restricted", time.Now().Add(time.Hour))
	require.NoError(t, e)
	parsed, e := ParseEnrollmentToken(bearer)
	require.NoError(t, e)
	require.Equal(t, tok, parsed)
	_, e = ParsePublicEnrollmentToken(tok.Public)
	require.NoError(t, e)
	parts := strings.Split(bearer, ".")
	require.NotContains(t, tok.Public, parts[3])
	for _, i := range []int{0, 1, 2, 3} {
		bad := append([]string{}, parts...)
		if i == 0 {
			bad[i] = "tcle2"
		} else {
			bad[i] = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
		}
		_, e = ParseEnrollmentToken(strings.Join(bad, "."))
		require.ErrorIs(t, e, ErrEnrollmentToken)
		require.NotContains(t, e.Error(), parts[3])
	}
	_, e = ParseEnrollmentToken(tok.Public)
	require.ErrorIs(t, e, ErrEnrollmentToken)
	_, _, e = NewEnrollmentToken(id, "p", "rigs", 1, "invalid", time.Now())
	require.ErrorIs(t, e, ErrEnrollmentToken)
}
