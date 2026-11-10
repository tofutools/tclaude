package federationcmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAttachHelloText(t *testing.T) {
	for _, tc := range []struct {
		hello attachHello
		want  string
	}{
		{attachHello{Type: "hello", Shared: true}, ""},
		{attachHello{Type: "hello", Shared: true, Others: []string{"a local terminal"}}, "Also attached: a local terminal. Sharing the size: the smallest terminal sets it."},
		{attachHello{Type: "hello", Shared: true, Others: []string{"a local terminal", "another remote viewer"}, TakeOver: true}, "Also attached: a local terminal, another remote viewer. Sharing the size: the smallest terminal sets it. Rerun with --take-over to disconnect them."},
		{attachHello{Type: "hello", Others: []string{"another remote viewer"}}, "Also attached: another remote viewer."},
		{attachHello{Type: "size", Others: []string{"x"}}, ""},
	} {
		if got := attachHelloText(tc.hello); got != tc.want {
			t.Errorf("attachHelloText(%+v) = %q, want %q", tc.hello, got, tc.want)
		}
	}
	if got := attachCloseText("taken_over"); got != "another viewer took over this terminal" {
		t.Errorf("taken_over: %q", got)
	}
}

// A path with a comma is one attachment: --attach is a StringArray, not
// pflag's comma-splitting StringSlice.
func TestSendAttachKeepsACommaInsideItsPath(t *testing.T) {
	cmd := sendCmd()
	require.NoError(t, cmd.ParseFlags([]string{"--attach", "report,v2.pdf", "--attach", "b.png"}))
	got, err := cmd.Flags().GetStringArray("attach")
	require.NoError(t, err)
	require.Equal(t, []string{"report,v2.pdf", "b.png"}, got)
}
