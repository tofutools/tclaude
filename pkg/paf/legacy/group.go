package legacy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/groupexport"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/privacy"
)

// SourceGroupExport names group export (v1 to v5) input.
const SourceGroupExport = "group-export"

// GroupStatePath is where the tclaude extension keeps a group's tclaude
// state: members, owners, permissions, enrollment, cron jobs, messages and
// the rest of the group export's tables.
const GroupStatePath = paf.ExtDir + paf.ExtTclaude + "/group-state.json"

// ClaudeHistoryFormat is the native history format of a Claude Code
// conversation.
const ClaudeHistoryFormat = "claude-jsonl"

// GroupExt is the tclaude extension on group.json.
type GroupExt struct {
	Group *groupexport.Group `json:"group,omitempty"`
}

// MemberExt is the tclaude extension on a group.json member.
type MemberExt struct {
	Owner bool `json:"owner,omitempty"`
}

// GroupWriteOptions configures WriteGroupExport.
type GroupWriteOptions struct {
	Producer paf.Producer
	Keys     []paf.SigningKey
	// MaxArchiveBytes bounds the written archive; 0 uses the PAF default.
	MaxArchiveBytes int64
	// Findings, when set, are recorded as the credential scan in
	// privacy.json; AllowedFindings records a human's allowance.
	Findings        []paf.Finding
	AllowedFindings bool
	// EarlierLosses holds the stored loss reports of converted conversations,
	// by conversation id. Each is written as that agent's losses.json.
	EarlierLosses map[string]*paf.Losses
	// AddContent, when set, adds to the archive before it closes; it gets
	// the agent keys in the order of exp.Convs.
	AddContent func(w *paf.Writer, agents []string) error
}

// WriteGroupExport writes a group export as a group PAF. group.json carries
// the name, description and members; each conversation becomes an agent
// with its Claude Code history as the native blob; tclaude's own tables
// travel in the tclaude extension, so a tclaude import loses nothing.
func WriteGroupExport(exp *groupexport.Export, out io.Writer, opts GroupWriteOptions) error {
	if exp == nil {
		return errors.New("legacy: nil group export")
	}
	w := paf.NewWriter(out, paf.KindGroup, paf.Limits{MaxArchiveBytes: opts.MaxArchiveBytes})
	if t, err := time.Parse(time.RFC3339Nano, exp.ExportedAt); err == nil {
		w.SetCreatedAt(t)
	}
	if err := w.DeclareExtension(paf.ExtTclaude, TclaudeExtension(opts.Producer.Version), false); err != nil {
		return err
	}

	roles := map[string]string{}
	for _, m := range exp.Members {
		roles[m.ConvID] = m.Role
	}
	keys := map[string]string{}
	used := map[string]bool{}
	for _, c := range exp.Convs {
		name := c.Title
		if strings.TrimSpace(name) == "" {
			name = c.ConvID
		}
		key := uniqueKey(AgentKey(name), used)
		keys[c.ConvID] = key
		harnessName, format := c.Harness, c.HistoryFormat
		if harnessName == "" {
			harnessName = "claude"
		}
		if format == "" {
			format = ClaudeHistoryFormat
		}
		if !paf.ValidEntryPath(format) || strings.Contains(format, "/") {
			return fmt.Errorf("legacy: invalid history format %q", format)
		}
		ag := paf.Agent{
			Name:         name,
			Conversation: &paf.Conversation{ID: c.ConvID, Harness: harnessName},
		}
		if _, member := roles[c.ConvID]; member {
			ag.Groups = []paf.GroupRef{{Name: exp.SourceGroup, Role: roles[c.ConvID]}}
		}
		env := paf.Environment{Harness: paf.Harness{Name: harnessName}}
		if c.SourceCwd != "" {
			env.Workspace = &paf.Workspace{Cwd: c.SourceCwd}
		}
		if !c.Missing {
			ag.Native = &paf.Native{Format: format, Path: paf.AgentPath(key, "native/"+format+"/history.jsonl"), Harness: harnessName, SourceConvID: c.ConvID}
		}
		if err := w.AddAgent(key, ag, env); err != nil {
			return err
		}
		if ag.Native != nil {
			if err := w.WriteFile(ag.Native.Path, "application/jsonl", c.Content); err != nil {
				return err
			}
		}
		if l := opts.EarlierLosses[c.ConvID]; l != nil {
			if err := w.WriteJSON(paf.AgentPath(key, "context/losses.json"), l); err != nil {
				return err
			}
		}
	}

	owners := map[string]bool{}
	for _, o := range exp.Owners {
		owners[o.ConvID] = true
	}
	g := paf.Group{Name: exp.SourceGroup, Description: exp.Group.Descr, Members: []paf.GroupMember{}}
	for _, m := range exp.Members {
		key, ok := keys[m.ConvID]
		if !ok {
			// A member without a conversation row has no agent to point at.
			continue
		}
		gm := paf.GroupMember{Agent: key, Role: m.Role}
		if owners[m.ConvID] {
			if err := gm.Extensions.Set(paf.ExtTclaude, MemberExt{Owner: true}); err != nil {
				return err
			}
		}
		g.Members = append(g.Members, gm)
	}
	group := exp.Group
	if err := g.Extensions.Set(paf.ExtTclaude, GroupExt{Group: &group}); err != nil {
		return err
	}
	if err := w.WriteJSON(paf.GroupPath, g); err != nil {
		return err
	}

	// The tables beyond group.json, in the group export's own shape. The
	// group row lives in group.json and each conversation's bytes in its
	// agent, so neither is repeated here.
	state := *exp
	state.FormatVersion = groupexport.FormatVersion
	state.Group = groupexport.Group{}
	// The state can outgrow the metadata bound (message history), so it is
	// an extension data entry rather than metadata.
	raw, err := json.Marshal(&state)
	if err != nil {
		return err
	}
	if err := w.WriteFile(GroupStatePath, "application/json", raw); err != nil {
		return err
	}

	if len(opts.Findings) > 0 {
		pr := paf.Privacy{Scan: &paf.Scan{Scanner: privacy.Scanner, Findings: opts.Findings, Allowed: opts.AllowedFindings}}
		if err := w.WriteJSON(paf.PrivacyPath, pr); err != nil {
			return err
		}
	}
	if opts.AddContent != nil {
		order := make([]string, 0, len(exp.Convs))
		for _, c := range exp.Convs {
			order = append(order, keys[c.ConvID])
		}
		if err := opts.AddContent(w, order); err != nil {
			return err
		}
	}
	var root paf.Extensions
	if err := root.Set(paf.ExtTclaude, map[string]any{"converted_from": ConvertedFrom{Format: SourceGroupExport, FormatVersion: groupexport.FormatVersion, CreatedAt: exp.ExportedAt}}); err != nil {
		return err
	}
	w.SetManifestExtensions(root)
	_, err = w.Close(opts.Producer, opts.Keys...)
	return err
}

func uniqueKey(key string, used map[string]bool) string {
	k := key
	for i := 2; used[k]; i++ {
		suffix := "-" + strconv.Itoa(i)
		base := key
		if len(base)+len(suffix) > 64 {
			base = base[:64-len(suffix)]
		}
		k = base + suffix
	}
	used[k] = true
	return k
}

// ToGroupExport reads a group PAF into the group export model the group
// import uses. A PAF tclaude wrote comes back exactly. A group PAF from
// another producer imports its base fields: the group's name, description
// and members, each with its native history or neutral context. Each Conv
// records its harness, native format and agent key; the importer decides
// where each lands.
func ToGroupExport(a *paf.Archive, stateLimit int64) (*groupexport.Export, error) {
	if a.Manifest.Kind != paf.KindGroup {
		return nil, errors.New("a group import needs a group PAF")
	}
	g := a.Group()
	tclaude := a.UnderstandsExtension(paf.ExtTclaude)
	if tclaude && a.HasEntry(GroupStatePath) {
		raw, err := a.ReadEntry(GroupStatePath, stateLimit)
		if err != nil {
			return nil, fmt.Errorf("tclaude group state: %w", err)
		}
		var exp groupexport.Export
		if err := json.Unmarshal(raw, &exp); err != nil {
			return nil, fmt.Errorf("tclaude group state: %w", err)
		}
		if exp.FormatVersion <= 0 || exp.FormatVersion > groupexport.FormatVersion {
			return nil, fmt.Errorf("%w: tclaude group state is format v%d, this tclaude understands up to v%d", groupexport.ErrUnsupportedFormat, exp.FormatVersion, groupexport.FormatVersion)
		}
		var ext GroupExt
		if _, err := g.Extensions.Decode(paf.ExtTclaude, &ext); err != nil {
			return nil, fmt.Errorf("tclaude extension: %w", err)
		}
		if ext.Group != nil {
			exp.Group = *ext.Group
		}
		byConv := map[string]string{}
		for _, key := range a.Manifest.Agents {
			if ag, ok := a.Agent(key); ok && ag.Conversation != nil {
				byConv[ag.Conversation.ID] = key
			}
		}
		for i := range exp.Convs {
			c := &exp.Convs[i]
			if c.Missing {
				continue
			}
			key, ok := byConv[c.ConvID]
			if !ok {
				return nil, fmt.Errorf("tclaude group state lists conversation %s, which no agent in the archive carries", c.ConvID)
			}
			if err := readConversation(a, key, c); err != nil {
				return nil, err
			}
		}
		return &exp, nil
	}
	return foreignGroup(a, g)
}

// foreignGroup builds a group export from a group PAF's base fields.
func foreignGroup(a *paf.Archive, g *paf.Group) (*groupexport.Export, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	exp := &groupexport.Export{
		FormatVersion: groupexport.FormatVersion,
		ExportedAt:    a.Manifest.CreatedAt.UTC().Format(time.RFC3339Nano),
		SourceGroup:   g.Name,
		Group:         groupexport.Group{Descr: g.Description, CreatedAt: now},
	}
	seenKey, seenConv := map[string]bool{}, map[string]bool{}
	for _, mem := range g.Members {
		if seenKey[mem.Agent] {
			return nil, fmt.Errorf("group member %q is listed twice", mem.Agent)
		}
		seenKey[mem.Agent] = true
		ag, _ := a.Agent(mem.Agent)
		env, _ := a.Environment(mem.Agent)
		convID := ""
		if ag.Native != nil {
			convID = ag.Native.SourceConvID
		}
		if convID == "" && ag.Conversation != nil {
			convID = ag.Conversation.ID
		}
		if convID == "" {
			convID = newConvID()
		}
		if seenConv[convID] {
			return nil, fmt.Errorf("group members share conversation %s", convID)
		}
		seenConv[convID] = true
		title := ag.Name
		if title == "" {
			title = mem.Agent
		}
		c := groupexport.Conv{ConvID: convID, Title: title, Harness: env.Harness.Name}
		if env.Workspace != nil {
			c.SourceCwd = env.Workspace.Cwd
		}
		if err := readConversation(a, mem.Agent, &c); err != nil {
			return nil, err
		}
		exp.Convs = append(exp.Convs, c)
		exp.Members = append(exp.Members, groupexport.Member{ConvID: convID, Role: mem.Role, Descr: ag.Definition.Description, JoinedAt: now})
		exp.Enrollments = append(exp.Enrollments, groupexport.Enrollment{ConvID: convID, EnrolledAt: now, EnrolledVia: "paf-import"})
		exp.Workdirs = append(exp.Workdirs, groupexport.Workdir{ConvID: convID, UpdatedAt: now})
	}
	return exp, nil
}

// readConversation fills c from agent key: its native history when it has
// one, else a pointer at its neutral context, else Missing (configuration
// only).
func readConversation(a *paf.Archive, key string, c *groupexport.Conv) error {
	ag, _ := a.Agent(key)
	env, _ := a.Environment(key)
	c.AgentKey = key
	if env != nil && c.Harness == "" {
		c.Harness = env.Harness.Name
	}
	if ag == nil || ag.Native == nil {
		c.Missing = !a.HasContext(key, paf.ScopeEffective)
		return nil
	}
	if ag.Native.Harness != "" {
		c.Harness = ag.Native.Harness
	}
	c.HistoryFormat = ag.Native.Format
	rc, err := a.OpenEntry(ag.Native.Path)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	content, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	c.Content = content
	return nil
}

// newConvID returns a random UUIDv4 for a conversation that arrived
// without one.
func newConvID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
