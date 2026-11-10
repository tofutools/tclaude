package agentd

import (
	"bufio"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompleteLinesReader(t *testing.T) {
	read := func(in string) (string, error) {
		raw, err := io.ReadAll(&completeLinesReader{r: bufio.NewReader(strings.NewReader(in)), limit: 1 << 20})
		return string(raw), err
	}
	got, err := read("{\"a\":1}\n{\"b\":")
	require.NoError(t, err)
	assert.Equal(t, "{\"a\":1}\n", got, "a half-written record is dropped")

	got, err = read("{\"a\":1}\n{\"b\":2}")
	require.NoError(t, err)
	assert.Equal(t, "{\"a\":1}\n{\"b\":2}", got, "a whole record without a newline stays")

	_, err = read(strings.Repeat("x", 2<<20))
	require.ErrorContains(t, err, "history record exceeds", "one record cannot fill memory")
}
