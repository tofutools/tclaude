package federationcmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"unicode"
)

type hubReadParams struct {
	JSON bool `long:"json" help:"Print raw JSON"`
}

func runHubRead(resource string, p *hubReadParams, stdout, stderr io.Writer) int {
	if p.JSON || resource != "status" {
		return runHubCall("GET", resource, nil, stdout, stderr)
	}
	var buf bytes.Buffer
	if rc := runHubCall("GET", resource, nil, &buf, stderr); rc != 0 {
		return rc
	}
	var out any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		return fail(stderr, err)
	}
	return printRecordingTable(stdout, "status", out)
}
func printRecordingTable(stdout io.Writer, kind string, out any) int {
	raw, err := json.Marshal(out)
	if err != nil {
		return fail(stdout, err)
	}
	var obj map[string]any
	if err = json.Unmarshal(raw, &obj); err != nil {
		return fail(stdout, err)
	}
	var header, fields []string
	key := kind
	switch kind {
	case "list":
		key = "boards"
		header = []string{"ID", "NAME", "ROLE", "EPOCH", "FROZEN"}
		fields = []string{"id", "name", "role", "epoch", "frozen"}
	case "items":
		header = []string{"ID", "NAME", "KIND", "VERSION", "PINNED", "UPDATE"}
		fields = []string{"id", "name", "kind", "version", "pinned_version", "update_available"}
	case "invites":
		header = []string{"TOKEN ID", "ROLE", "EXPIRES", "USED BY"}
		fields = []string{"token_id", "role", "expires_at", "used_by"}
	case "status":
		header = []string{"HUB", "VERSION", "CONNECTED", "ADMIN"}
		fields = []string{"hub_id", "hub_version", "connected", "admin"}
	case "files":
		key = "entries"
		header = []string{"PATH", "KIND", "BYTES"}
		fields = []string{"path", "kind", "size"}
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, strings.Join(header, "\t"))
	rows, _ := obj[key].([]any)
	if kind == "status" {
		rows = []any{obj}
	}
	for _, row := range rows {
		m, _ := row.(map[string]any)
		values := make([]string, len(fields))
		for i, field := range fields {
			if v := m[field]; v != nil {
				values[i] = strings.Map(func(r rune) rune {
					if unicode.IsControl(r) {
						return ' '
					}
					return r
				}, fmt.Sprint(v))
			}
		}
		_, _ = fmt.Fprintln(w, strings.Join(values, "\t"))
	}
	_ = w.Flush()
	if cursor, _ := obj["next_cursor"].(string); cursor != "" {
		_, _ = fmt.Fprintf(stdout, "More: --cursor %s\n", cursor)
	}
	if truncated, _ := obj["truncated"].(bool); truncated {
		_, _ = fmt.Fprintln(stdout, "Listing capped; narrow the directory to see more.")
	}
	return 0
}
