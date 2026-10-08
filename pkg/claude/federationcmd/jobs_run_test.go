package federationcmd

import (
	"bytes"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJobLiveWriterPreservesSplitUnicodeAndStripsControls(t *testing.T) {
	var out bytes.Buffer
	w := safeJobWriter{out: &out, mu: &sync.Mutex{}}
	for _, chunk := range [][]byte{{0xe2}, {0x82}, {0xac, '\x1b', '[', '3', '1', 'm', '\x07', 'x'}, {0xff}} {
		_, e := w.Write(chunk)
		require.NoError(t, e)
	}
	w.Flush()
	require.Equal(t, "€[31mx�", out.String())
}

func TestSingleJobPrintsUncertaintyAndFailureGuidance(t *testing.T) {
	for _, message := range []string{"execution uncertain; inspect or cancel this job", "job status uncertain; inspect or cancel this job", "log fetch failed"} {
		var out bytes.Buffer
		printJobSummaries(&out, []jobSummary{{Error: message, Code: 1}})
		require.Contains(t, out.String(), message)
	}
}
