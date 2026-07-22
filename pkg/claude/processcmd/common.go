package processcmd

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

func openStore(root string) (*store.FS, error) { return store.NewFS(root) }

func newTable(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func noEngineError() error {
	return fmt.Errorf("process runtime is temporarily unavailable: no engine is installed")
}
