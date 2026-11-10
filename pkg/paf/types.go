// Package paf reads, writes, validates and signs Portable Agent Format
// archives (docs/paf.md). The wire types here are the format, defined by the
// spec and its JSON Schemas, not by tclaude's internal structures: internal
// code converts to and from them explicitly.
//
// Extension data is kept as raw JSON keyed by extension id, so a reader that
// does not understand an extension still round-trips it, and a reader that
// does decodes it into its own types.
package paf

import (
	"encoding/json"
	"time"
)

const (
	// Format is the manifest's format marker.
	Format = "paf"
	// Major is the PAF major version this package reads and writes.
	Major = 0
	// Minor is the highest minor version of Major this package knows.
	Minor = 2
	// Version is the version this package stamps on files that hold agents
	// only: writers stamp the lowest version that expresses the content.
	Version = "0.1"
	// EnvelopeVersion is the version stamped on a file that also carries
	// workflows, and the newest version this package reads.
	EnvelopeVersion = "0.2"

	KindAgent = "agent"
	KindGroup = "group"
	// KindBundle holds workflows with the agents that perform them, and no
	// group.
	KindBundle = "bundle"

	// MediaType is the media type of a .paf file.
	MediaType = "application/vnd.tclaude.paf+zip"

	ManifestPath = "manifest.json"
	PrivacyPath  = "privacy.json"
	GroupPath    = "group.json"

	SignaturesDir  = "signatures/"
	AttachmentsDir = "attachments/"
	ExtDir         = "ext/"
	AgentsDir      = "agents/"
	WorkflowsDir   = "workflows/"

	ScopeEffective = "effective"
	ScopeFull      = "full"

	// ExtTclaude is the id of the extension tclaude documents in the spec.
	ExtTclaude = "tclaude"

	// LinkPerformer says a workflow ships an agent for one of its roles.
	LinkPerformer = "performer"
	// LinkCarries says an agent carries a workflow it owns or runs.
	LinkCarries = "carries"
)

// Extensions is extension data keyed by extension id.
type Extensions map[string]json.RawMessage

// Manifest is manifest.json.
type Manifest struct {
	Format             string                   `json:"format"`
	PafVersion         string                   `json:"paf_version"`
	Kind               string                   `json:"kind"`
	CreatedAt          time.Time                `json:"created_at"`
	Producer           Producer                 `json:"producer"`
	Requires           []string                 `json:"requires"`
	ExtensionsUsed     map[string]ExtensionDecl `json:"extensions_used,omitempty"`
	ExtensionsRequired []string                 `json:"extensions_required,omitempty"`
	// MinReaderVersion marks a deliberate break: the oldest PAF version
	// whose reader can import the file. Writers leave it out otherwise.
	MinReaderVersion string   `json:"min_reader_version,omitempty"`
	Agents           []string `json:"agents"`
	// Workflows and Links are the envelope: workflows side by side with the
	// agents, and how the two refer to each other.
	Workflows  []WorkflowItem `json:"workflows,omitempty"`
	Links      []Link         `json:"links,omitempty"`
	Entries    []Entry        `json:"entries"`
	Extensions Extensions     `json:"extensions,omitempty"`
}

// WorkflowItem is one workflow in the envelope. Its entries live under
// workflows/<key>/ in PWF's layout; Digest is SectionDigest over them, and
// pkg/pwf reads and checks them.
type WorkflowItem struct {
	Key  string `json:"key"`
	Root string `json:"root"`
	// Requires are the PWF features the workflow uses.
	Requires []string `json:"requires"`
	Digest   string   `json:"digest"`
}

// Link is a reference between an agent and a workflow, by key and by hash.
// Links sit in the manifest rather than in either side, so neither side's
// hash depends on the other and references cannot form a hash cycle. Digest
// is the SectionDigest of what the link points at: the agent of a performer
// link, the workflow of a carries link.
type Link struct {
	Type     string `json:"type"`
	Workflow string `json:"workflow"`
	Agent    string `json:"agent"`
	// Role is the workflow role a performer link fills.
	Role   string `json:"role,omitempty"`
	Digest string `json:"digest"`
}

// Producer names the software that wrote the file.
type Producer struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// ExtensionDecl declares one extension in manifest.extensions_used.
type ExtensionDecl struct {
	Version       string `json:"version"`
	System        string `json:"system"`
	SystemVersion string `json:"system_version,omitempty"`
	Spec          string `json:"spec"`
	Schema        string `json:"schema,omitempty"`
	Copy          string `json:"copy,omitempty"`
}

// CopySafe reports whether the declaration allows a tool that does not
// understand the extension to keep its data across ordinary edits. Unknown
// values mean unsafe.
func (d ExtensionDecl) CopySafe() bool { return d.Copy == "safe" }

// Entry is one manifest entry row.
type Entry struct {
	Path       string     `json:"path"`
	Bytes      int64      `json:"bytes"`
	SHA256     string     `json:"sha256"`
	MediaType  string     `json:"media_type,omitempty"`
	Extensions Extensions `json:"extensions,omitempty"`
}

// Ref references an agent or conversation somewhere. All fields are
// informational.
type Ref struct {
	Instance     string     `json:"instance,omitempty"`
	Agent        string     `json:"agent,omitempty"`
	Conversation string     `json:"conversation,omitempty"`
	Snapshot     string     `json:"snapshot,omitempty"`
	Extensions   Extensions `json:"extensions,omitempty"`
}

// Agent is agents/<key>/agent.json.
type Agent struct {
	ID           string        `json:"id,omitempty"`
	Name         string        `json:"name"`
	Title        string        `json:"title,omitempty"`
	Instance     *Instance     `json:"instance,omitempty"`
	Conversation *Conversation `json:"conversation,omitempty"`
	Definition   Definition    `json:"definition"`
	Groups       []GroupRef    `json:"groups,omitempty"`
	Lineage      []LineageStep `json:"lineage,omitempty"`
	Snapshot     *Snapshot     `json:"snapshot,omitempty"`
	Native       *Native       `json:"native,omitempty"`
	Extensions   Extensions    `json:"extensions,omitempty"`
}

type Instance struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type Conversation struct {
	ID      string `json:"id,omitempty"`
	Title   string `json:"title,omitempty"`
	Harness string `json:"harness,omitempty"`
}

// Definition is the persona and task.
type Definition struct {
	Role           string     `json:"role,omitempty"`
	Description    string     `json:"description,omitempty"`
	RoleGuidance   string     `json:"role_guidance,omitempty"`
	StartupContext string     `json:"startup_context,omitempty"`
	InitialMessage string     `json:"initial_message,omitempty"`
	Task           *Task      `json:"task,omitempty"`
	License        string     `json:"license,omitempty"`
	Extensions     Extensions `json:"extensions,omitempty"`
}

type Task struct {
	URL        string     `json:"url,omitempty"`
	Label      string     `json:"label,omitempty"`
	Extensions Extensions `json:"extensions,omitempty"`
}

// GroupRef is a team the agent belonged to. Advisory.
type GroupRef struct {
	Name       string     `json:"name"`
	Role       string     `json:"role,omitempty"`
	Extensions Extensions `json:"extensions,omitempty"`
}

// Lineage operations the spec names. The set is open.
const (
	OpSpawn       = "spawn"
	OpClone       = "clone"
	OpMorph       = "morph"
	OpReincarnate = "reincarnate"
	OpMove        = "move"
	OpTeleport    = "teleport"
	OpRecall      = "recall"
	OpSnapshot    = "snapshot"
	OpImport      = "import"
	OpRecover     = "recover"
	OpPublish     = "publish"
)

type LineageStep struct {
	Op         string     `json:"op"`
	At         time.Time  `json:"at"`
	From       *Ref       `json:"from,omitempty"`
	Detail     string     `json:"detail,omitempty"`
	Extensions Extensions `json:"extensions,omitempty"`
}

type Snapshot struct {
	Name    string    `json:"name"`
	Note    string    `json:"note,omitempty"`
	TakenAt time.Time `json:"taken_at"`
	State   string    `json:"state,omitempty"`
}

// Native describes the native history blob.
type Native struct {
	Format         string `json:"format"`
	Path           string `json:"path"`
	Harness        string `json:"harness,omitempty"`
	HarnessVersion string `json:"harness_version,omitempty"`
	SourceConvID   string `json:"source_conv_id,omitempty"`
}

// Environment is agents/<key>/environment.json.
type Environment struct {
	Harness      Harness        `json:"harness"`
	Model        *Model         `json:"model,omitempty"`
	Approval     string         `json:"approval,omitempty"`
	Modes        map[string]any `json:"modes,omitempty"`
	Sandbox      *Sandbox       `json:"sandbox,omitempty"`
	Workspace    *Workspace     `json:"workspace,omitempty"`
	Placeholders []Placeholder  `json:"placeholders,omitempty"`
	Skills       []SkillRef     `json:"skills,omitempty"`
	Tools        []ToolRef      `json:"tools,omitempty"`
	MCPServers   []MCPServerRef `json:"mcp_servers,omitempty"`
	Extensions   Extensions     `json:"extensions,omitempty"`
}

type Harness struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type Model struct {
	ID                  string     `json:"id,omitempty"`
	Provider            string     `json:"provider,omitempty"`
	Effort              string     `json:"effort,omitempty"`
	ContextWindowTokens int64      `json:"context_window_tokens,omitempty"`
	ContextWindowSource string     `json:"context_window_source,omitempty"`
	Usage               *Usage     `json:"usage,omitempty"`
	Extensions          Extensions `json:"extensions,omitempty"`
}

type Usage struct {
	At               *time.Time `json:"at,omitempty"`
	ContextTokens    int64      `json:"context_tokens,omitempty"`
	InputTokens      int64      `json:"input_tokens,omitempty"`
	OutputTokens     int64      `json:"output_tokens,omitempty"`
	CacheReadTokens  int64      `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64      `json:"cache_write_tokens,omitempty"`
}

type Sandbox struct {
	Layers     []SandboxLayer `json:"layers,omitempty"`
	Extensions Extensions     `json:"extensions,omitempty"`
}

type SandboxLayer struct {
	Provider   string     `json:"provider"`
	Mode       string     `json:"mode"`
	Extensions Extensions `json:"extensions,omitempty"`
}

type Workspace struct {
	Cwd        string     `json:"cwd,omitempty"`
	RepoURL    string     `json:"repo_url,omitempty"`
	Branch     string     `json:"branch,omitempty"`
	Commit     string     `json:"commit,omitempty"`
	Dirty      *bool      `json:"dirty,omitempty"`
	Worktree   string     `json:"worktree,omitempty"`
	Extensions Extensions `json:"extensions,omitempty"`
}

type Placeholder struct {
	Name        string     `json:"name"`
	Original    string     `json:"original,omitempty"`
	Description string     `json:"description,omitempty"`
	Extensions  Extensions `json:"extensions,omitempty"`
}

type SkillRef struct {
	Name       string     `json:"name"`
	Source     string     `json:"source,omitempty"`
	Version    string     `json:"version,omitempty"`
	SHA256     string     `json:"sha256,omitempty"`
	Extensions Extensions `json:"extensions,omitempty"`
}

type ToolRef struct {
	Name       string     `json:"name"`
	Kind       string     `json:"kind,omitempty"`
	Server     string     `json:"server,omitempty"`
	Extensions Extensions `json:"extensions,omitempty"`
}

type MCPServerRef struct {
	Name       string     `json:"name"`
	Transport  string     `json:"transport,omitempty"`
	Version    string     `json:"version,omitempty"`
	Extensions Extensions `json:"extensions,omitempty"`
}

// Group is group.json.
type Group struct {
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Members     []GroupMember `json:"members"`
	Extensions  Extensions    `json:"extensions,omitempty"`
}

type GroupMember struct {
	Agent      string     `json:"agent"`
	Role       string     `json:"role,omitempty"`
	Extensions Extensions `json:"extensions,omitempty"`
}

// Privacy is privacy.json.
type Privacy struct {
	Sensitivity string      `json:"sensitivity,omitempty"`
	Audience    *Audience   `json:"audience,omitempty"`
	Scan        *Scan       `json:"scan,omitempty"`
	Redactions  []Redaction `json:"redactions,omitempty"`
	Consent     []Consent   `json:"consent,omitempty"`
	Retention   *Retention  `json:"retention,omitempty"`
	Extensions  Extensions  `json:"extensions,omitempty"`
}

type Audience struct {
	Kind       string   `json:"kind"`
	Recipients []string `json:"recipients,omitempty"`
	Board      string   `json:"board,omitempty"`
}

type Scan struct {
	Scanner  string     `json:"scanner"`
	At       *time.Time `json:"at,omitempty"`
	Findings []Finding  `json:"findings"`
	Allowed  bool       `json:"allowed,omitempty"`
}

// Finding is a scan finding: counts and locations, never matched text.
type Finding struct {
	Kind      string   `json:"kind"`
	Count     int      `json:"count"`
	Locations []string `json:"locations,omitempty"`
}

type Redaction struct {
	Kind   string `json:"kind"`
	Count  int    `json:"count,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type Consent struct {
	By            string    `json:"by"`
	At            time.Time `json:"at"`
	Audience      string    `json:"audience"`
	ContentDigest string    `json:"content_digest"`
}

type Retention struct {
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Losses is agents/<key>/context/losses.json.
type Losses struct {
	Source                 string     `json:"source"`
	Target                 string     `json:"target,omitempty"`
	SourceEntries          int        `json:"source_entries,omitempty"`
	CarriedEntries         int        `json:"carried_entries,omitempty"`
	DroppedPercent         int        `json:"dropped_percent,omitempty"`
	KeptPercent            int        `json:"kept_percent,omitempty"`
	SourceEstimatedTokens  int64      `json:"source_estimated_tokens,omitempty"`
	CarriedEstimatedTokens int64      `json:"carried_estimated_tokens,omitempty"`
	ContextWindowTokens    int64      `json:"context_window_tokens,omitempty"`
	ContextWindowSource    string     `json:"context_window_source,omitempty"`
	BudgetTokens           int64      `json:"budget_tokens,omitempty"`
	LandingRung            string     `json:"landing_rung,omitempty"`
	Fallback               string     `json:"fallback,omitempty"`
	Items                  []LossItem `json:"items"`
	// Prior are the reports of earlier translations of the same
	// conversation, oldest first.
	Prior      []Losses   `json:"prior,omitempty"`
	Extensions Extensions `json:"extensions,omitempty"`
}

type LossItem struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Count    int    `json:"count"`
	Detail   string `json:"detail,omitempty"`
}

// Landing rungs, from most faithful to most robust.
const (
	RungNative      = "native"
	RungTranslated  = "translated"
	RungContextFile = "context_file"
	RungArchive     = "archive"
)

// LandingRungFromFallback maps a legacy morph delivery name to a landing rung.
// The legacy "native" meant translated into a native conversation.
func LandingRungFromFallback(fallback string) string {
	switch fallback {
	case "native":
		return RungTranslated
	case "context_file", "handoff":
		return RungContextFile
	}
	return ""
}

// Signature is one signatures/<key-id>.json file.
type Signature struct {
	Alg            string  `json:"alg"`
	PublicKey      []byte  `json:"public_key"`
	KeyID          string  `json:"key_id"`
	Signer         *Signer `json:"signer,omitempty"`
	ManifestSHA256 string  `json:"manifest_sha256"`
	Signature      []byte  `json:"signature"`
}

type Signer struct {
	Kind string `json:"kind,omitempty"`
	Name string `json:"name,omitempty"`
}
