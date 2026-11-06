package agentd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Member verbs on a peer: `tclaude agent stop|resume|retire|rename|task …
// member@peer`. The local /v1/agent/{selector}/{verb} route takes a selector
// that does not resolve here and names a trusted peer, and runs the verb as
// that peer's dashboard route through the peer view: the operator's own view
// for a human caller, an agent peer view (peer_view_agents.go) for an agent,
// with its peer= grants. Nothing here grants anything: it only translates
// the local verb's request into the peer view request.

// remoteMemberVerb is a verb that reaches a peer's agent: the methods it
// takes and the peer's dashboard route it runs as (agents/{id}/<tail>).
type remoteMemberVerb struct {
	methods []string
	tail    string
}

var remoteMemberVerbs = map[string]remoteMemberVerb{
	"stop":        {methods: []string{http.MethodPost}, tail: "stop"},
	"resume":      {methods: []string{http.MethodPost}, tail: "resume"},
	"retire":      {methods: []string{http.MethodPost}, tail: "retire"},
	"rename":      {methods: []string{http.MethodPost}, tail: "rename"},
	"task":        {methods: []string{http.MethodPost}, tail: "task"},
	"interrupt":   {methods: []string{http.MethodPost}, tail: "interrupt"},
	"compact":     {methods: []string{http.MethodPost}, tail: "compact"},
	"reincarnate": {methods: []string{http.MethodPost}, tail: "reincarnate"},
	"context":     {methods: []string{http.MethodGet}, tail: "context-info"},
	"prs":         {methods: []string{http.MethodGet, http.MethodPost}, tail: "prs"},
}

// serveRemoteMemberVerb runs verb on the peer agent selector names. It
// reports false when selector is not a peer member, so the caller answers
// as for any unresolved selector.
func serveRemoteMemberVerb(w http.ResponseWriter, r *http.Request, selector, verb string) bool {
	spec, ok := remoteMemberVerbs[verb]
	if !ok || !isFederatedAddress(selector) {
		return false
	}
	member, peerRef, _ := splitFederatedAddress(selector)
	peer, err := resolveFederationPeerOpt(peerRef, false)
	if err != nil {
		writeFedErr(w, err)
		return true
	}
	agentID, status, msg := resolveRemoteMember(peer.InstanceID, member)
	if status != 0 {
		writeError(w, status, "not_found", msg)
		return true
	}
	if !slices.Contains(spec.methods, r.Method) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "this does not reach a peer's agent; read it with tclaude agent ls --node "+peerRef)
		return true
	}
	body, note, refusal := remoteMemberVerbBody(r, verb)
	if refusal != "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", refusal)
		return true
	}
	tail := "agents/" + agentID + "/" + spec.tail
	if verb == "retire" && r.URL.Query().Get("shutdown") != "" {
		tail += "?shutdown=" + boolQuery(r.URL.Query().Get("shutdown") != "0")
	}
	path, query, _ := strings.Cut(tail, "?")
	req := r.Clone(r.Context())
	req.URL.Path, req.URL.RawPath, req.URL.RawQuery = "/v1/federation/peer/"+peer.InstanceID+"/"+path, "", query
	req.SetPathValue("node", peer.InstanceID)
	req.SetPathValue("tail", path)
	req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	req.Header = r.Header.Clone()
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handleFederationPeerView(rec, req)
	writeRemoteMemberReply(w, rec, member+"@"+peerDisplay(peer), note)
	return true
}

func boolQuery(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// resolveRemoteMember finds the agent member names in the peer's latest
// catalog: an agent ID, or a name that exactly one listed agent has.
func resolveRemoteMember(peer, member string) (string, int, string) {
	cat, _, err := fedCatalogFor(peer)
	if err != nil {
		return "", http.StatusServiceUnavailable, "could not read the peer's catalog"
	}
	found := map[string]bool{}
	if cat != nil {
		for _, g := range cat.Groups {
			for _, m := range g.Members {
				if m.Agent == member || m.Name == member {
					found[m.Agent] = true
				}
			}
			for _, s := range g.AgentStatuses {
				if s.Agent == member || s.Name == member {
					found[s.Agent] = true
				}
			}
		}
	}
	if len(found) == 0 && proto.ValidAgentRef(member) {
		// The peer decides whether it knows the agent; the sender's own gate
		// refuses an agent the catalog does not show.
		return member, 0, ""
	}
	switch len(found) {
	case 0:
		return "", http.StatusNotFound, "the peer shares no agent named " + proto.SafeName(member, false) + "; list them with tclaude agent ls --node"
	case 1:
		for id := range found {
			return id, 0, ""
		}
	}
	return "", http.StatusConflict, "several agents on the peer are named " + proto.SafeName(member, false) + "; use the agt_… ID"
}

// remoteMemberVerbBody translates a local verb's request into the peer's
// dashboard body. note says what the remote verb leaves out; refusal is a
// local option a peer never takes.
func remoteMemberVerbBody(r *http.Request, verb string) (body []byte, note, refusal string) {
	q := r.URL.Query()
	raw, err := io.ReadAll(io.LimitReader(r.Body, fedPeerViewBodyLimit+1))
	if err != nil || len(raw) > fedPeerViewBodyLimit {
		return nil, "", "request body exceeds the peer limit"
	}
	// No text of the caller's is typed into a peer's pane: a follow-up or
	// focus hint never crosses.
	var followUp struct {
		FollowUp string `json:"follow_up"`
	}
	_ = json.Unmarshal(raw, &followUp)
	switch verb {
	case "interrupt":
		if len(q) > 0 {
			return nil, "", "remote interrupt takes no options"
		}
		return nil, "", ""
	case "compact":
		if len(q) > 0 || strings.TrimSpace(followUp.FollowUp) != "" {
			return nil, "", "remote compact takes no follow-up: it would be typed into the peer's pane"
		}
		return nil, "", ""
	case "reincarnate":
		if len(q) > 0 {
			return nil, "", "remote reincarnate takes no options"
		}
		if strings.TrimSpace(followUp.FollowUp) != "" {
			note = "the agent was asked to reincarnate itself with its own handoff; your follow-up was not sent: a peer's agent takes no text into its pane"
		}
		out, _ := json.Marshal(map[string]string{"mode": "self"})
		return out, note, ""
	case "context":
		for k := range q {
			if k != "fresh" {
				return nil, "", "remote context info does not take " + k
			}
		}
		return nil, "", ""
	case "prs":
		if len(q) > 0 {
			return nil, "", "remote PRs take no options"
		}
		if r.Method == http.MethodGet {
			return nil, "", ""
		}
		var in struct {
			URL     string `json:"url"`
			Summary string `json:"summary"`
			State   string `json:"state"`
			Handled bool   `json:"handled"`
			RepoDir string `json:"repo_dir"`
		}
		if json.Unmarshal(raw, &in) != nil || strings.TrimSpace(in.URL) == "" {
			return nil, "", "expected a PR link"
		}
		if in.RepoDir != "" {
			note = "the repository directory was not sent: it is a path on this node, not the peer's"
		}
		out, _ := json.Marshal(map[string]any{"url": in.URL, "summary": in.Summary, "state": in.State, "handled": in.Handled})
		return out, note, ""
	case "stop":
		for k := range q {
			if k != "force" {
				return nil, "", "remote stop takes only --force"
			}
		}
		out, _ := json.Marshal(map[string]bool{"force": q.Get("force") == "1"})
		return out, "", ""
	case "resume":
		if len(q) > 0 {
			return nil, "", "remote resume takes no options: recreating a directory or switching a drive stays the peer operator's"
		}
		return nil, "", ""
	case "retire":
		for k := range q {
			if k != "shutdown" && k != "delete_worktree" && k != "reason" {
				return nil, "", "remote retire does not take " + k
			}
		}
		var notes []string
		if q.Get("delete_worktree") == "1" {
			notes = append(notes, "the agent's worktree was kept: a peer's worktrees stay its operator's to delete")
		}
		if strings.TrimSpace(q.Get("reason")) != "" {
			notes = append(notes, "the reason was not recorded: a peer's retire takes none")
		}
		note = strings.Join(notes, "; ")
		return nil, note, ""
	case "rename":
		var in struct {
			Title string `json:"title"`
			Auto  bool   `json:"auto"`
		}
		if json.Unmarshal(raw, &in) != nil || in.Auto || strings.TrimSpace(in.Title) == "" {
			return nil, "", "remote rename needs an explicit title"
		}
		out, _ := json.Marshal(map[string]string{"title": in.Title})
		return out, "", ""
	case "task":
		var in struct {
			URL   string `json:"url"`
			Label string `json:"label"`
			Clear bool   `json:"clear"`
		}
		if json.Unmarshal(raw, &in) != nil {
			return nil, "", "expected a task link or clear"
		}
		out, _ := json.Marshal(in)
		return out, "", ""
	}
	return nil, "", "verb does not reach a peer"
}

// writeRemoteMemberReply passes the peer's reply on, marked as remote and
// untrusted, with control characters stripped from every string: the CLI
// prints it into an agent's context.
func writeRemoteMemberReply(w http.ResponseWriter, rec *httptest.ResponseRecorder, target, note string) {
	for _, k := range []string{"X-Tclaude-Remote-Content", "Cache-Control"} {
		if v := rec.Header().Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	var obj map[string]any
	if json.Unmarshal(rec.Body.Bytes(), &obj) != nil || obj == nil {
		writeJSON(w, rec.Code, map[string]any{"error": "peer returned an invalid response", "code": "peer_invalid_response"})
		return
	}
	clean := stripRemoteControls(obj).(map[string]any)
	// Its peer view metadata is the dashboard's, not the CLI's.
	delete(clean, "peer_view")
	clean["remote"] = map[string]any{"target": proto.SafeName(target, true), "untrusted": true}
	if note != "" && rec.Code < 300 {
		clean["remote_note"] = note
	}
	writeJSON(w, rec.Code, clean)
}

func stripRemoteControls(v any) any {
	switch t := v.(type) {
	case string:
		return proto.StripControls(t)
	case []any:
		for i := range t {
			t[i] = stripRemoteControls(t[i])
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[proto.StripControls(k)] = stripRemoteControls(val)
		}
		return out
	}
	return v
}
