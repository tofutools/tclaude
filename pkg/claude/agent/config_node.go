package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/common/termtext"
)

// config_node.go is `tclaude config get|set|clear --node <peer>`: a linked
// node's Config tab from the command line, through this node's peer proxy to
// the peer's /api/config (agentd/config_remote.go). The peer decides what
// this operator may read and change; its answers are shown as they come.
// Secret values never leave the peer: they show as withheld, set or not set,
// and can be replaced or cleared, never read.

// nodeConfigPath is the peer's /api/config through the local operator
// API, which serves a peer's /api/{tail} at /v1/federation/peer/{node}/{tail}.
func nodeConfigPath(node string) string {
	return "/v1/federation/peer/" + url.PathEscape(node) + "/config"
}

// nodeConfigView mirrors the GET /api/config answer a peer gets.
type nodeConfigView struct {
	Raw      string `json:"raw"`
	Version  string `json:"version"`
	Warning  string `json:"warning"`
	Redacted []struct {
		Path string `json:"path"`
		Set  bool   `json:"set"`
	} `json:"redacted"`
	Access struct {
		Level   string            `json:"level"`
		Classes map[string]string `json:"classes"`
		Slugs   map[string]string `json:"slugs"`
		Keys    []struct {
			Path   string `json:"path"`
			Class  string `json:"class"`
			Secret bool   `json:"secret"`
		} `json:"keys"`
	} `json:"access"`
}

func nodeConfigOpts() DaemonOpts { return DaemonOpts{Timeout: 20 * time.Second, NoRetry: true} }

func fetchNodeConfig(node string, stderr io.Writer) (json.RawMessage, *nodeConfigView, int) {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return nil, nil, rc
	}
	var raw json.RawMessage
	if err := DaemonRequest(http.MethodGet, nodeConfigPath(node), nil, &raw, nodeConfigOpts()); err != nil {
		return nil, nil, nodeConfigFail(node, err, stderr)
	}
	var view nodeConfigView
	if err := json.Unmarshal(raw, &view); err != nil {
		fmt.Fprintln(stderr, "Error: unexpected answer from the peer:", err)
		return nil, nil, rcIOFailure
	}
	return raw, &view, rcOK
}

// nodeConfigFail explains a refused read or change: drift says re-read, a
// refusal names each key and the grant it needs.
func nodeConfigFail(node string, err error, stderr io.Writer) int {
	var de *DaemonError
	if !errors.As(err, &de) {
		fmt.Fprintln(stderr, "Error:", err)
		return MapDaemonErrorToRC(err)
	}
	var body struct {
		Code   string   `json:"code"`
		Error  string   `json:"error"`
		Errors []string `json:"errors"`
		Denied []struct {
			Path     string `json:"path"`
			Class    string `json:"class"`
			Requires string `json:"requires"`
		} `json:"denied"`
	}
	_ = json.Unmarshal(de.Raw, &body)
	c := termtext.Cell
	switch {
	case de.Status == http.StatusConflict && body.Code == "config_drift":
		fmt.Fprintf(stderr, "Error: the config on %s changed while this change was made; re-read it (tclaude config get --node %s) and retry\n", c(node), c(node))
		return rcIOFailure
	case len(body.Denied) > 0:
		fmt.Fprintf(stderr, "Error: %s refused the change:\n", c(node))
		for _, d := range body.Denied {
			fmt.Fprintf(stderr, "  %s (%s): needs %s\n", c(d.Path), c(d.Class), c(d.Requires))
		}
		return rcAuth
	case len(body.Errors) > 0:
		for _, e := range body.Errors {
			fmt.Fprintln(stderr, "Error:", c(e))
		}
	case body.Error != "":
		fmt.Fprintln(stderr, "Error:", c(body.Error))
	default:
		fmt.Fprintln(stderr, "Error:", err)
	}
	return MapDaemonErrorToRC(err)
}

// nodeConfigRow is one key as `config get --node` prints it.
type nodeConfigRow struct {
	Path     string          `json:"path"`
	Class    string          `json:"class,omitempty"`
	Access   string          `json:"access,omitempty"`
	Value    json.RawMessage `json:"value,omitempty"`
	Withheld bool            `json:"withheld,omitempty"`
	Set      bool            `json:"set"`
}

// nodeConfigRows lists the keys set in the view (or, with key, the key and
// the keys under it), and every withheld secret, with its class and whether
// this operator may change it.
func nodeConfigRows(v *nodeConfigView, key string) ([]nodeConfigRow, error) {
	var tree map[string]any
	dec := json.NewDecoder(strings.NewReader(v.Raw))
	dec.UseNumber()
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("the peer's config is not valid JSON: %w", err)
	}
	under := func(path string) bool {
		return key == "" || path == key || strings.HasPrefix(path, key+".") || strings.HasPrefix(path, key+"[")
	}
	classOf := map[string]string{}
	var rows []nodeConfigRow
	for _, k := range v.Access.Keys {
		classOf[k.Path] = k.Class
		if !under(k.Path) {
			continue
		}
		val, ok := nodeConfigLookup(tree, k.Path)
		if !ok && k.Path != key {
			continue
		}
		row := nodeConfigRow{Path: k.Path, Class: k.Class, Access: v.Access.Classes[k.Class], Set: ok}
		if ok {
			raw, err := json.Marshal(val)
			if err != nil {
				return nil, err
			}
			row.Value = raw
		}
		rows = append(rows, row)
	}
	// A withheld secret replaces its row, or (inside a list, such as an
	// environment variable's value) adds one.
	for _, r := range v.Redacted {
		if !under(r.Path) {
			continue
		}
		row := nodeConfigRow{Path: r.Path, Withheld: true, Set: r.Set}
		replaced := false
		for i := range rows {
			if rows[i].Path == r.Path {
				row.Class, row.Access = rows[i].Class, rows[i].Access
				rows[i], replaced = row, true
				break
			}
		}
		if !replaced {
			row.Class = nodeConfigClassFor(classOf, r.Path)
			row.Access = v.Access.Classes[row.Class]
			rows = append(rows, row)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
	if key != "" && len(rows) == 0 {
		return nil, fmt.Errorf("%s is not a config key on this peer", key)
	}
	return rows, nil
}

// nodeConfigClassFor is the class of the leaf a list-element path is under.
func nodeConfigClassFor(classOf map[string]string, path string) string {
	for p := path; p != ""; {
		if c, ok := classOf[p]; ok {
			return c
		}
		i := strings.LastIndexAny(p, ".[")
		if i < 0 {
			break
		}
		p = p[:i]
	}
	return ""
}

func nodeConfigLookup(tree map[string]any, path string) (any, bool) {
	var cur any = tree
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[seg]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func runNodeConfigGet(node, key string, asJSON bool, stdout, stderr io.Writer) int {
	raw, view, rc := fetchNodeConfig(node, stderr)
	if rc != rcOK {
		return rc
	}
	if asJSON && key == "" {
		_, _ = stdout.Write(append(bytes.TrimSpace(raw), '\n'))
		return rcOK
	}
	rows, err := nodeConfigRows(view, strings.TrimSpace(key))
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return rcNotFound
	}
	if asJSON {
		if err := json.NewEncoder(stdout).Encode(map[string]any{"version": view.Version, "level": view.Access.Level, "keys": rows}); err != nil {
			return rcIOFailure
		}
		return rcOK
	}
	c := termtext.Cell
	fmt.Fprintf(stdout, "Config of %s, as the peer shows it to you (trust: %s). Secret values stay on the node.\n", c(node), c(view.Access.Level))
	if view.Warning != "" {
		fmt.Fprintln(stdout, "Warning:", c(view.Warning))
	}
	for _, r := range rows {
		var val string
		switch {
		case r.Withheld && r.Set:
			val = "(withheld: set)"
		case r.Withheld:
			val = "(withheld: not set)"
		case !r.Set:
			val = "(not set: default)"
		default:
			val = string(r.Value)
		}
		access := r.Access
		if access == "" {
			access = "read"
		}
		fmt.Fprintf(stdout, "%s = %s  [%s, %s]\n", c(r.Path), c(val), c(r.Class), c(access))
	}
	return rcOK
}

// nodeConfigValue is a set value: JSON if it parses as JSON, else a string.
// A secret inside a map or list (a proxy's header value, a launch env value)
// is replaced by setting that whole map or list leaf.
func nodeConfigValue(value string) json.RawMessage {
	if json.Valid([]byte(value)) {
		return json.RawMessage(value)
	}
	raw, _ := json.Marshal(value)
	return raw
}

// runNodeConfigChange reads the peer's config for its version, then PATCHes
// one key: set to value, or (clear) back to its default, or a withheld
// secret removed.
func runNodeConfigChange(node, key string, value *string, stdout, stderr io.Writer) int {
	key = strings.TrimSpace(key)
	if key == "" {
		fmt.Fprintln(stderr, "Error: a config key is required")
		return rcInvalidArg
	}
	_, view, rc := fetchNodeConfig(node, stderr)
	if rc != rcOK {
		return rc
	}
	body := map[string]any{"version": view.Version}
	switch {
	case value != nil:
		body["set"] = map[string]json.RawMessage{key: nodeConfigValue(*value)}
	default:
		secret := false
		for _, r := range view.Redacted {
			if r.Path == key {
				if !r.Set {
					fmt.Fprintf(stderr, "%s is not set on %s\n", termtext.Cell(key), termtext.Cell(node))
					return rcOK
				}
				secret = true
			}
		}
		if secret {
			body["clear"] = []string{key}
		} else {
			body["set"] = map[string]json.RawMessage{key: json.RawMessage("null")}
		}
	}
	var resp struct {
		Changed []string `json:"changed"`
	}
	if err := DaemonRequest(http.MethodPatch, nodeConfigPath(node), body, &resp, nodeConfigOpts()); err != nil {
		return nodeConfigFail(node, err, stderr)
	}
	if len(resp.Changed) == 0 {
		fmt.Fprintf(stdout, "%s on %s: unchanged\n", termtext.Cell(key), termtext.Cell(node))
		return rcOK
	}
	fmt.Fprintf(stdout, "Changed on %s: %s\n", termtext.Cell(node), termtext.Cell(strings.Join(resp.Changed, ", ")))
	return rcOK
}
