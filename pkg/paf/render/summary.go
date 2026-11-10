// Package render turns a verified PAF into things a person reads: a
// structured summary, and Markdown or HTML documents that need no harness,
// model or network (landing rung 4, the archive rung).
package render

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/paf"
)

// Summary describes a PAF without its conversation content.
type Summary struct {
	Source      string             `json:"source"`
	Format      string             `json:"format"`
	PafVersion  string             `json:"paf_version"`
	Kind        string             `json:"kind"`
	CreatedAt   time.Time          `json:"created_at"`
	Producer    paf.Producer       `json:"producer"`
	Requires    []string           `json:"requires,omitempty"`
	Unsupported []string           `json:"unsupported,omitempty"`
	Warnings    []string           `json:"warnings,omitempty"`
	Extensions  []ExtensionSummary `json:"extensions,omitempty"`
	Signatures  []SignatureSummary `json:"signatures"`
	Entries     int                `json:"entries"`
	Bytes       int64              `json:"bytes"`
	Group       *GroupSummary      `json:"group,omitempty"`
	Agents      []AgentSummary     `json:"agents"`
	// Workflows are the workflows the file carries beside its agents.
	Workflows []WorkflowSummary `json:"workflows,omitempty"`
	Privacy   *PrivacySummary   `json:"privacy,omitempty"`
	// Rescan is the reader's own credential scan, when it ran one.
	Rescan []paf.Finding `json:"rescan,omitempty"`
}

// WorkflowSummary is one workflow of the envelope, and who it is tied to.
type WorkflowSummary struct {
	Key        string            `json:"key"`
	Root       string            `json:"root"`
	Performers map[string]string `json:"performers,omitempty"`
	CarriedBy  []string          `json:"carried_by,omitempty"`
}

type ExtensionSummary struct {
	ID            string `json:"id"`
	Version       string `json:"version"`
	System        string `json:"system"`
	SystemVersion string `json:"system_version,omitempty"`
	Required      bool   `json:"required,omitempty"`
	Understood    bool   `json:"understood"`
}

type SignatureSummary struct {
	Path   string `json:"path"`
	Alg    string `json:"alg"`
	KeyID  string `json:"key_id"`
	Signer string `json:"signer,omitempty"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	// Trust says who holds a validly signing key, when the caller knows:
	// "this node", "known peer <name>" or "unknown key".
	Trust string `json:"trust,omitempty"`
}

type GroupSummary struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Members     []string `json:"members"`
}

type AgentSummary struct {
	Key         string         `json:"key"`
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Role        string         `json:"role,omitempty"`
	Harness     string         `json:"harness"`
	Model       string         `json:"model,omitempty"`
	Effort      string         `json:"effort,omitempty"`
	Cwd         string         `json:"cwd,omitempty"`
	Groups      []string       `json:"groups,omitempty"`
	Lineage     []string       `json:"lineage,omitempty"`
	Native      *NativeSummary `json:"native,omitempty"`
	Context     []ContextFile  `json:"context,omitempty"`
	Handoff     bool           `json:"handoff,omitempty"`
	LandingRung string         `json:"landing_rung,omitempty"`
	Losses      []paf.LossItem `json:"losses,omitempty"`
	// PriorReports counts the loss reports of earlier conversions.
	PriorReports int             `json:"prior_reports,omitempty"`
	Placeholders []string        `json:"placeholders,omitempty"`
	Workspace    *paf.Workspace  `json:"workspace,omitempty"`
	Sandbox      []string        `json:"sandbox,omitempty"`
	Approval     string          `json:"approval,omitempty"`
	Definition   *paf.Definition `json:"-"`
}

type NativeSummary struct {
	Format  string `json:"format"`
	Harness string `json:"harness,omitempty"`
	Bytes   int64  `json:"bytes"`
}

type ContextFile struct {
	Scope string `json:"scope"`
	Bytes int64  `json:"bytes"`
}

type PrivacySummary struct {
	Sensitivity string        `json:"sensitivity,omitempty"`
	Audience    string        `json:"audience,omitempty"`
	Findings    []paf.Finding `json:"findings,omitempty"`
	Redactions  int           `json:"redactions,omitempty"`
	Consent     []ConsentLine `json:"consent,omitempty"`
}

// ConsentLine is one consent record. Current is true when it binds the
// archive's content as it is now. A consent record is only the signer's
// claim: Vouched is true when a signature the caller trusts (this node or
// a known peer, see SignatureSummary.Trust) covers it.
type ConsentLine struct {
	Audience string    `json:"audience"`
	By       string    `json:"by"`
	At       time.Time `json:"at"`
	Current  bool      `json:"current"`
	Vouched  bool      `json:"vouched"`
}

// VouchConsent marks the consent records vouched for when a valid
// signature is from this node or a known peer. Call it after setting
// SignatureSummary.Trust.
func (s *Summary) VouchConsent() {
	if s.Privacy == nil {
		return
	}
	vouched := false
	for _, sig := range s.Signatures {
		if sig.Status == string(paf.SignatureValid) && (sig.Trust == "this node" || strings.HasPrefix(sig.Trust, "known peer")) {
			vouched = true
		}
	}
	for i := range s.Privacy.Consent {
		s.Privacy.Consent[i].Vouched = vouched
	}
}

// Summarize describes a verified archive. source names the file's own
// format ("paf", or a legacy format it was converted from).
func Summarize(a *paf.Archive, source string) Summary {
	m := a.Manifest
	s := Summary{
		Source: source, Format: m.Format, PafVersion: m.PafVersion, Kind: m.Kind,
		CreatedAt: m.CreatedAt, Producer: m.Producer, Requires: m.Requires,
		Unsupported: a.Unsupported, Warnings: a.Warnings, Entries: len(m.Entries), Signatures: []SignatureSummary{},
		Agents: []AgentSummary{},
	}
	for _, wf := range m.Workflows {
		row := WorkflowSummary{Key: wf.Key, Root: wf.Root}
		for _, l := range m.Links {
			switch {
			case l.Workflow != wf.Key:
			case l.Type == paf.LinkPerformer:
				if row.Performers == nil {
					row.Performers = map[string]string{}
				}
				row.Performers[l.Role] = l.Agent
			case l.Type == paf.LinkCarries:
				row.CarriedBy = append(row.CarriedBy, l.Agent)
			}
		}
		s.Workflows = append(s.Workflows, row)
	}
	sizes := map[string]int64{}
	for _, e := range m.Entries {
		s.Bytes += e.Bytes
		sizes[e.Path] = e.Bytes
	}
	for id, d := range m.ExtensionsUsed {
		required := false
		for _, r := range m.ExtensionsRequired {
			required = required || r == id
		}
		s.Extensions = append(s.Extensions, ExtensionSummary{ID: id, Version: d.Version, System: d.System, SystemVersion: d.SystemVersion, Required: required, Understood: a.UnderstandsExtension(id)})
	}
	sort.Slice(s.Extensions, func(i, j int) bool { return s.Extensions[i].ID < s.Extensions[j].ID })
	for _, r := range a.Signatures {
		ss := SignatureSummary{Path: r.Path, Alg: r.Signature.Alg, KeyID: r.Signature.KeyID, Status: string(r.Status)}
		if r.Signature.Signer != nil {
			ss.Signer = r.Signature.Signer.Kind
			if r.Signature.Signer.Name != "" {
				ss.Signer += " " + r.Signature.Signer.Name
			}
		}
		if r.Err != nil {
			ss.Error = r.Err.Error()
		}
		s.Signatures = append(s.Signatures, ss)
	}
	if g := a.Group(); g != nil {
		gs := &GroupSummary{Name: g.Name, Description: g.Description}
		for _, mem := range g.Members {
			gs.Members = append(gs.Members, mem.Agent)
		}
		s.Group = gs
	}
	for _, key := range m.Agents {
		s.Agents = append(s.Agents, summarizeAgent(a, key, sizes))
	}
	if p := a.Privacy(); p != nil {
		ps := &PrivacySummary{Sensitivity: p.Sensitivity, Redactions: len(p.Redactions)}
		if p.Audience != nil {
			ps.Audience = p.Audience.Kind
		}
		if p.Scan != nil {
			ps.Findings = p.Scan.Findings
		}
		digest := paf.ContentDigest(m.Entries)
		for _, c := range p.Consent {
			ps.Consent = append(ps.Consent, ConsentLine{Audience: c.Audience, By: c.By, At: c.At, Current: c.ContentDigest == digest})
		}
		s.Privacy = ps
	}
	return s
}

func summarizeAgent(a *paf.Archive, key string, sizes map[string]int64) AgentSummary {
	as := AgentSummary{Key: key}
	if ag, ok := a.Agent(key); ok {
		as.Name, as.Title, as.Role = ag.Name, ag.Title, ag.Definition.Role
		def := ag.Definition
		as.Definition = &def
		for _, g := range ag.Groups {
			as.Groups = append(as.Groups, g.Name)
		}
		for _, l := range ag.Lineage {
			step := l.Op
			if !l.At.IsZero() {
				step += " " + l.At.UTC().Format(time.RFC3339)
			}
			if l.From != nil && l.From.Instance != "" {
				step += " from " + l.From.Instance
			}
			as.Lineage = append(as.Lineage, step)
		}
		if n := ag.Native; n != nil {
			as.Native = &NativeSummary{Format: n.Format, Harness: n.Harness, Bytes: sizes[n.Path]}
		}
	}
	if env, ok := a.Environment(key); ok {
		as.Harness = env.Harness.Name
		if env.Model != nil {
			as.Model, as.Effort = env.Model.ID, env.Model.Effort
		}
		if env.Workspace != nil {
			as.Cwd = env.Workspace.Cwd
			as.Workspace = env.Workspace
		}
		as.Approval = env.Approval
		if env.Sandbox != nil {
			for _, l := range env.Sandbox.Layers {
				as.Sandbox = append(as.Sandbox, l.Provider+": "+l.Mode)
			}
		}
		for _, p := range env.Placeholders {
			as.Placeholders = append(as.Placeholders, p.Name)
		}
	}
	for _, scope := range []string{paf.ScopeEffective, paf.ScopeFull} {
		if p := contextPath(key, scope); a.HasEntry(p) {
			as.Context = append(as.Context, ContextFile{Scope: scope, Bytes: sizes[p]})
		}
	}
	as.Handoff = a.HasEntry(paf.AgentPath(key, "context/handoff.md"))
	if l, ok := a.Losses(key); ok {
		as.LandingRung, as.Losses, as.PriorReports = l.LandingRung, l.Items, len(l.Prior)
	}
	return as
}

func contextPath(key, scope string) string {
	if scope == paf.ScopeFull {
		return paf.AgentPath(key, "context/full.jsonl")
	}
	return paf.AgentPath(key, "context/transcript.jsonl")
}

// NativeDecoder decodes a native history blob into the neutral transcript.
// It returns an error when it cannot read that format.
type NativeDecoder func(n paf.Native, r io.Reader) (*transcript.Transcript, error)

// Options selects the transcript a document shows.
type Options struct {
	// Scope picks a context file. Empty prefers the full transcript, then
	// the effective one.
	Scope string
	// Native decodes the native blob when the agent has no context file.
	Native NativeDecoder
	// Media bounds attachments loaded from the archive.
	Media paf.ReadOptions
}

// ErrNoTranscript reports an agent with neither a context file nor a
// native history this reader can decode.
var ErrNoTranscript = errors.New("no readable conversation")

// Transcript loads an agent's conversation and names where it came from:
// "context (full)", "context (effective)" or "native <format>".
func Transcript(a *paf.Archive, key string, opts Options) (*transcript.Transcript, string, error) {
	scopes := []string{paf.ScopeFull, paf.ScopeEffective}
	if opts.Scope != "" {
		scopes = []string{opts.Scope}
	}
	for _, scope := range scopes {
		if a.HasEntry(contextPath(key, scope)) {
			t, err := paf.ReadTranscript(a, key, scope, opts.Media)
			return t, "context (" + scope + ")", err
		}
	}
	if opts.Scope != "" {
		return nil, "", fmt.Errorf("%w: the agent has no %s context", ErrNoTranscript, opts.Scope)
	}
	ag, ok := a.Agent(key)
	if !ok || ag.Native == nil {
		return nil, "", ErrNoTranscript
	}
	if opts.Native == nil {
		return nil, "", fmt.Errorf("%w: native %s history needs a decoder", ErrNoTranscript, ag.Native.Format)
	}
	rc, err := a.OpenEntry(ag.Native.Path)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = rc.Close() }()
	t, err := opts.Native(*ag.Native, rc)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrNoTranscript, err)
	}
	return t, "native " + ag.Native.Format, nil
}
