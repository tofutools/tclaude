package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

var (
	ErrSandboxProfileNameTaken      = errors.New("a sandbox profile with that name already exists")
	ErrSandboxProfileNotFound       = errors.New("sandbox profile not found")
	ErrSandboxProfileChanged        = errors.New("sandbox profile changed since preview")
	ErrSandboxProfileInvalidImport  = errors.New("invalid sandbox profile import")
	ErrSandboxProfileInvalidInclude = errors.New("invalid sandbox profile include")
	ErrSandboxProfileIncludedBy     = errors.New("sandbox profile is included by other profiles")
)

// SandboxFilesystemGrant is one normalized filesystem capability in a sandbox
// profile. Profiles may retain canonical lexical paths that do not yet exist
// locally; resolution preserves those rules for the harness sandbox.
type SandboxFilesystemGrant = sandboxpolicy.FilesystemGrant

// SandboxEnvironmentEntry remains a slice element (rather than a map value)
// so duplicate keys survive decoding long enough for the normalization seam to
// distinguish identical duplicates from conflicting values.
type SandboxEnvironmentEntry = sandboxpolicy.EnvironmentEntry

// SandboxProfile is a stable-ID registry row. Environment values are
// deliberately non-secret plaintext configuration, not a credential store.
// Includes composes other profiles by name in authored order; the write
// paths keep the reference graph dangling-free and acyclic, and resolution
// flattens it before any value becomes launch authority.
type SandboxProfile struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Legacy rows and exports may still carry read_baseline /
	// read_baseline_exclusions (TCL-623) or break_glass_filesystem (TCL-791)
	// fields. Neither has a field to decode into any more. Stored rows drop
	// them, which strictly narrows; operator-supplied input is refused loudly
	// at the daemon boundary instead, so nothing is imported that differs from
	// what the file says.
	Filesystem              []SandboxFilesystemGrant           `json:"filesystem"`
	FilesystemSpellings     *sandboxpolicy.FilesystemSpellings `json:"filesystem_spellings,omitempty"`
	Environment             []SandboxEnvironmentEntry          `json:"environment"`
	AgentDirectories        []string                           `json:"agent_directories"`
	FilesystemRoot          sandboxpolicy.FilesystemRootMode   `json:"filesystem_root,omitempty"`
	NetworkAccess           sandboxpolicy.NetworkAccess        `json:"network_access,omitempty"`
	Network                 *sandboxpolicy.NetworkRules        `json:"network,omitempty"`
	UnixSockets             *sandboxpolicy.UnixSocketRules     `json:"unix_sockets,omitempty"`
	ResourceLimits          sandboxpolicy.ResourceLimits       `json:"resource_limits,omitempty"`
	DarwinAllowMachRegister bool                               `json:"darwin_allow_mach_register,omitempty"`
	PreLaunch               []sandboxpolicy.PreLaunchBlock     `json:"pre_launch,omitempty"`
	Includes                []string                           `json:"includes"`
	CreatedAt               time.Time                          `json:"created_at"`
	UpdatedAt               time.Time                          `json:"updated_at"`
}

type SandboxProfileAssignments struct {
	Global string
	Groups map[string]string
}

type SandboxProfileImportResult struct {
	Imported      []string
	Skipped       []string
	Warnings      []string
	AccessNotices []sandboxpolicy.AccessNotice
}

type sandboxProfileImportPlan struct {
	profile    *SandboxProfile
	existingID int64
	skipped    bool
}

type sandboxProfileGroupAssignmentPlan struct {
	group   string
	groupID int64
	profile string
}

type sandboxProfileAssignmentPlan struct {
	global   string
	groups   []sandboxProfileGroupAssignmentPlan
	warnings []string
}

// SandboxProfileImportOptions keeps every import decision together at the DB
// authority boundary. In particular, acknowledgement is consumed inside the
// same transaction that reads the conflict rows and applies the mutations.
type SandboxProfileImportOptions struct {
	OnConflict  string
	Assignments *SandboxProfileAssignments
}

func CreateSandboxProfile(p *SandboxProfile) (int64, error) {
	p, err := normalizeSandboxProfileForStore(p)
	if err != nil {
		return 0, err
	}
	payload, err := marshalSandboxProfilePayload(p)
	if err != nil {
		return 0, err
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	now := dbTime(time.Now())
	res, err := tx.Exec(`INSERT INTO sandbox_profiles
		(name, filesystem_json, filesystem_spellings_json, environment_json, agent_directories_json, filesystem_root, network_access, network_json, unix_sockets_json, resource_limits_json, darwin_allow_mach_register, pre_launch_json, includes_json, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Name, payload.filesystem, payload.filesystemSpellings, payload.environment, payload.agentDirectories,
		p.FilesystemRoot, p.NetworkAccess, payload.network, payload.unixSockets, payload.resourceLimits, p.DarwinAllowMachRegister,
		payload.preLaunch, payload.includes, now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrSandboxProfileNameTaken
		}
		return 0, err
	}
	if err := validateSandboxProfileIncludeGraph(tx); err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// UpdateSandboxProfile atomically replaces the complete payload. The row ID
// is immutable; rename snapshots for both assignment surfaces — and include
// references held by other profiles — are refreshed in the same transaction.
func UpdateSandboxProfile(p *SandboxProfile) error {
	return updateSandboxProfile(p, "")
}

// UpdateSandboxProfileIfUnchanged applies the complete replacement only when
// updated_at still matches revision. The comparison is part of the UPDATE so
// another writer cannot slip between a handler-side check and the write.
func UpdateSandboxProfileIfUnchanged(p *SandboxProfile, revision string) error {
	return updateSandboxProfile(p, revision)
}

func updateSandboxProfile(p *SandboxProfile, revision string) error {
	p, err := normalizeSandboxProfileForStore(p)
	if err != nil {
		return err
	}
	payload, err := marshalSandboxProfilePayload(p)
	if err != nil {
		return err
	}
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var oldName string
	if err := tx.QueryRow(`SELECT name FROM sandbox_profiles WHERE id = ?`, p.ID).Scan(&oldName); errors.Is(err, sql.ErrNoRows) {
		return sql.ErrNoRows
	} else if err != nil {
		return err
	}
	now := dbTime(time.Now())
	query := `UPDATE sandbox_profiles SET name = ?, filesystem_json = ?, filesystem_spellings_json = ?, environment_json = ?, agent_directories_json = ?, filesystem_root = ?, network_access = ?, network_json = ?, unix_sockets_json = ?, resource_limits_json = ?, darwin_allow_mach_register = ?, pre_launch_json = ?, includes_json = ?, updated_at = ? WHERE id = ?`
	args := []any{p.Name, payload.filesystem, payload.filesystemSpellings, payload.environment, payload.agentDirectories,
		p.FilesystemRoot, p.NetworkAccess, payload.network, payload.unixSockets, payload.resourceLimits, p.DarwinAllowMachRegister,
		payload.preLaunch, payload.includes, now, p.ID}
	if revision != "" {
		query += ` AND updated_at = ?`
		args = append(args, dbTimeText(revision))
	}
	res, err := tx.Exec(query, args...)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrSandboxProfileNameTaken
		}
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		if revision != "" {
			return ErrSandboxProfileChanged
		}
		return sql.ErrNoRows
	}
	if _, err := tx.Exec(`UPDATE agent_groups SET sandbox_profile = ? WHERE sandbox_profile_id = ?`, p.Name, p.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE sandbox_profile_global_assignment SET profile_name = ? WHERE profile_id = ?`, p.Name, p.ID); err != nil {
		return err
	}
	if oldName != p.Name {
		if err := renameSandboxProfileIncludeRefs(tx, oldName, p.Name); err != nil {
			return err
		}
	}
	if err := validateSandboxProfileIncludeGraph(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// renameSandboxProfileIncludeRefs follows a rename into every profile whose
// include list references the old name, mirroring how assignment name
// snapshots track the stable ID. Referrers' timestamps are left untouched —
// their effective content did not change.
func renameSandboxProfileIncludeRefs(tx *sql.Tx, oldName, newName string) error {
	graph, err := loadSandboxProfileIncludeGraph(tx)
	if err != nil {
		return err
	}
	for name, includes := range graph {
		changed := false
		for i, include := range includes {
			if include == oldName {
				includes[i] = newName
				changed = true
			}
		}
		if !changed {
			continue
		}
		includesJSON, err := json.Marshal(includes)
		if err != nil {
			return fmt.Errorf("marshal sandbox profile %q includes: %w", name, err)
		}
		if _, err := tx.Exec(`UPDATE sandbox_profiles SET includes_json = ? WHERE name = ?`, string(includesJSON), name); err != nil {
			return err
		}
	}
	return nil
}

// loadSandboxProfileIncludeGraph reads every profile's include list as
// name → includes, the working shape for reference validation and rewrites.
func loadSandboxProfileIncludeGraph(tx *sql.Tx) (map[string][]string, error) {
	rows, err := tx.Query(`SELECT name, includes_json FROM sandbox_profiles`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	graph := map[string][]string{}
	for rows.Next() {
		var name, includesJSON string
		if err := rows.Scan(&name, &includesJSON); err != nil {
			return nil, err
		}
		var includes []string
		if err := json.Unmarshal([]byte(includesJSON), &includes); err != nil {
			return nil, fmt.Errorf("decode sandbox profile %q includes: %w", name, err)
		}
		graph[name] = includes
	}
	return graph, rows.Err()
}

// validateSandboxProfileIncludeGraph re-checks the whole registry inside the
// writing transaction: every include must reference an existing profile and
// the graph must stay acyclic within the policy depth bound. Validating the
// complete graph after the write keeps create, edit, rename, and import on
// one shared invariant instead of per-path reasoning about what could have
// changed.
func validateSandboxProfileIncludeGraph(tx *sql.Tx) error {
	graph, err := loadSandboxProfileIncludeGraph(tx)
	if err != nil {
		return err
	}
	return validateIncludeGraphMap(graph)
}

// SandboxProfileImportGraphInspection reports, per conflict policy, the
// include-graph error a bundle would hit on import. Empty strings mean that
// policy's graph shape is valid. The two shapes can genuinely differ: under
// "overwrite" every bundle profile replaces its local namesake, while under
// "skip" a clashing local profile keeps its own includes — so a bundle that
// closes a cycle through an overwritten profile can still be validly imported
// with "skip". The "error" policy either aborts on the first clash or, with
// no clashes, degenerates to the same shape as the other two.
type SandboxProfileImportGraphInspection struct {
	OverwriteError string
	SkipError      string
}

// InspectSandboxProfileImportGraph validates the include graphs an import
// would produce, without writing anything: the bundle is overlaid on the
// current registry once per conflict-policy shape and each combined graph is
// checked for dangling references, cycles, and depth. The transactional
// import remains the final authority.
func InspectSandboxProfileImportGraph(profiles []*SandboxProfile) (SandboxProfileImportGraphInspection, error) {
	d, err := Open()
	if err != nil {
		return SandboxProfileImportGraphInspection{}, err
	}
	tx, err := d.Begin()
	if err != nil {
		return SandboxProfileImportGraphInspection{}, err
	}
	defer func() { _ = tx.Rollback() }()
	local, err := loadSandboxProfileIncludeGraph(tx)
	if err != nil {
		return SandboxProfileImportGraphInspection{}, err
	}
	shape := func(skipClashing bool) string {
		graph := make(map[string][]string, len(local)+len(profiles))
		maps.Copy(graph, local)
		for _, profile := range profiles {
			if profile == nil {
				continue
			}
			if _, clashes := local[profile.Name]; clashes && skipClashing {
				continue
			}
			graph[profile.Name] = profile.Includes
		}
		if err := validateIncludeGraphMap(graph); err != nil {
			return err.Error()
		}
		return ""
	}
	return SandboxProfileImportGraphInspection{
		OverwriteError: shape(false),
		SkipError:      shape(true),
	}, nil
}

// validateIncludeGraphMap is the pure invariant shared by the write paths and
// import inspection: every edge target exists, no cycles, and no profile's
// longest include-edge chain exceeds the policy bound.
func validateIncludeGraphMap(graph map[string][]string) error {
	for name, includes := range graph {
		for _, include := range includes {
			if _, exists := graph[include]; !exists {
				return fmt.Errorf("%w: profile %q includes unknown sandbox profile %q", ErrSandboxProfileInvalidInclude, name, include)
			}
		}
	}
	depth := map[string]int{}
	onPath := map[string]bool{}
	var visit func(name string) (int, error)
	visit = func(name string) (int, error) {
		if d, done := depth[name]; done {
			return d, nil
		}
		if onPath[name] {
			return 0, fmt.Errorf("%w: include cycle through sandbox profile %q", ErrSandboxProfileInvalidInclude, name)
		}
		onPath[name] = true
		defer delete(onPath, name)
		deepest := 0
		for _, include := range graph[name] {
			d, err := visit(include)
			if err != nil {
				return 0, err
			}
			if d+1 > deepest {
				deepest = d + 1
			}
		}
		if deepest > sandboxpolicy.MaxIncludeDepth {
			return 0, fmt.Errorf("%w: profile %q nests includes deeper than %d levels", ErrSandboxProfileInvalidInclude, name, sandboxpolicy.MaxIncludeDepth)
		}
		depth[name] = deepest
		return deepest, nil
	}
	names := make([]string, 0, len(graph))
	for name := range graph {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := visit(name); err != nil {
			return err
		}
	}
	return nil
}

type sandboxProfilePayloadJSON struct {
	filesystem          string
	filesystemSpellings string
	environment         string
	agentDirectories    string
	network             string
	unixSockets         string
	resourceLimits      string
	preLaunch           string
	includes            string
}

func marshalSandboxProfilePayload(p *SandboxProfile) (sandboxProfilePayloadJSON, error) {
	filesystem := p.Filesystem
	if filesystem == nil {
		filesystem = []SandboxFilesystemGrant{}
	}
	environment := p.Environment
	if environment == nil {
		environment = []SandboxEnvironmentEntry{}
	}
	agentDirectories := p.AgentDirectories
	if agentDirectories == nil {
		agentDirectories = []string{}
	}
	includes := p.Includes
	if includes == nil {
		includes = []string{}
	}
	filesystemJSON, err := json.Marshal(filesystem)
	if err != nil {
		return sandboxProfilePayloadJSON{}, fmt.Errorf("marshal sandbox profile filesystem: %w", err)
	}
	filesystemSpellingsJSON := []byte{}
	if p.FilesystemSpellings != nil {
		filesystemSpellingsJSON, err = json.Marshal(p.FilesystemSpellings)
		if err != nil {
			return sandboxProfilePayloadJSON{}, fmt.Errorf(
				"marshal sandbox profile filesystem spellings: %w", err,
			)
		}
	}
	environmentJSON, err := json.Marshal(environment)
	if err != nil {
		return sandboxProfilePayloadJSON{}, fmt.Errorf("marshal sandbox profile environment: %w", err)
	}
	agentDirectoriesJSON, err := json.Marshal(agentDirectories)
	if err != nil {
		return sandboxProfilePayloadJSON{}, fmt.Errorf("marshal sandbox profile agent directories: %w", err)
	}
	networkJSON := []byte{}
	if p.Network != nil {
		networkJSON, err = json.Marshal(p.Network)
		if err != nil {
			return sandboxProfilePayloadJSON{}, fmt.Errorf("marshal sandbox profile network rules: %w", err)
		}
	}
	unixSocketsJSON := []byte{}
	if p.UnixSockets != nil {
		unixSocketsJSON, err = json.Marshal(p.UnixSockets)
		if err != nil {
			return sandboxProfilePayloadJSON{}, fmt.Errorf("marshal sandbox profile Unix-socket rules: %w", err)
		}
	}
	resourceLimitsJSON, err := json.Marshal(p.ResourceLimits)
	if err != nil {
		return sandboxProfilePayloadJSON{}, fmt.Errorf("marshal sandbox profile resource limits: %w", err)
	}
	preLaunch := p.PreLaunch
	if preLaunch == nil {
		preLaunch = []sandboxpolicy.PreLaunchBlock{}
	}
	preLaunchJSON, err := json.Marshal(preLaunch)
	if err != nil {
		return sandboxProfilePayloadJSON{}, fmt.Errorf("marshal sandbox profile pre-launch blocks: %w", err)
	}
	includesJSON, err := json.Marshal(includes)
	if err != nil {
		return sandboxProfilePayloadJSON{}, fmt.Errorf("marshal sandbox profile includes: %w", err)
	}
	return sandboxProfilePayloadJSON{
		filesystem:          string(filesystemJSON),
		filesystemSpellings: string(filesystemSpellingsJSON),
		environment:         string(environmentJSON),
		agentDirectories:    string(agentDirectoriesJSON),
		network:             string(networkJSON),
		unixSockets:         string(unixSocketsJSON),
		resourceLimits:      string(resourceLimitsJSON),
		preLaunch:           string(preLaunchJSON),
		includes:            string(includesJSON),
	}, nil
}

// normalizeSandboxProfileForStore is the single defensive persistence seam
// shared by create/update (and reusable by import). Missing filesystem paths
// are valid profile data: they are retained in canonical lexical form so a
// profile can be prepared before its directories exist. Existing ancestors,
// protected-state paths, and environment entries still receive the full
// validation here. Resolution uses the same persistence normalization so the
// canonical missing rules can be passed through to the harness sandbox.
func normalizeSandboxProfileForStore(p *SandboxProfile) (*SandboxProfile, error) {
	if p == nil {
		return nil, errors.New("sandbox profile is nil")
	}
	input := sandboxpolicy.Profile{
		Name: p.Name, Filesystem: p.Filesystem, FilesystemSpellings: p.FilesystemSpellings,
		Environment: p.Environment, AgentDirectories: p.AgentDirectories, FilesystemRoot: p.FilesystemRoot, NetworkAccess: p.NetworkAccess,
		Network: p.Network, UnixSockets: p.UnixSockets, ResourceLimits: p.ResourceLimits, Includes: p.Includes,
		DarwinAllowMachRegister: p.DarwinAllowMachRegister,
		PreLaunch:               p.PreLaunch,
	}
	var normalized sandboxpolicy.Profile
	var err error
	authoring, err := sandboxFilesystemNeedsAuthoring(
		p.Filesystem, p.FilesystemSpellings,
	)
	if err != nil {
		return nil, err
	}
	if authoring {
		normalized, _, err = sandboxpolicy.NormalizeForAuthoring(input)
	} else {
		normalized, _, err = sandboxpolicy.NormalizeForPersistence(input)
	}
	if err != nil {
		return nil, err
	}
	out := *p
	out.Name = normalized.Name
	out.Filesystem = normalized.Filesystem
	out.FilesystemSpellings = normalized.FilesystemSpellings
	out.Environment = normalized.Environment
	out.AgentDirectories = normalized.AgentDirectories
	out.FilesystemRoot = normalized.FilesystemRoot
	out.NetworkAccess = normalized.NetworkAccess
	out.Network = normalized.Network
	out.UnixSockets = normalized.UnixSockets
	out.ResourceLimits = normalized.ResourceLimits
	out.DarwinAllowMachRegister = normalized.DarwinAllowMachRegister
	out.PreLaunch = normalized.PreLaunch
	out.Includes = normalized.Includes
	return &out, nil
}

// sandboxFilesystemNeedsAuthoring distinguishes an intentional filesystem
// replacement from an ordinary edit carrying a pinned spelling sidecar.
// Direct DB callers historically replace p.Filesystem in place; they cannot
// be required to know the sidecar's lifecycle. A structurally matching sidecar
// remains pinned and receives full drift validation. A changed row set enters
// the authoring seam and produces fresh metadata from only the new spellings.
func sandboxFilesystemNeedsAuthoring(
	filesystem []SandboxFilesystemGrant,
	spellings *sandboxpolicy.FilesystemSpellings,
) (bool, error) {
	if spellings == nil {
		return true, nil
	}
	authoritative := make(map[string]struct{}, len(filesystem))
	retained := make(map[string]map[string]struct{}, len(spellings.Rules))
	for _, rule := range spellings.Rules {
		set := retained[rule.ResolvedPath]
		if set == nil {
			set = map[string]struct{}{}
			retained[rule.ResolvedPath] = set
		}
		for _, spelling := range rule.Spellings {
			set[filepath.Clean(spelling)] = struct{}{}
		}
	}
	for _, grant := range filesystem {
		clean := filepath.Clean(grant.Path)
		authoritative[clean] = struct{}{}
		if _, pinned := retained[clean]; pinned {
			// This lexical path is already launch authority for retained
			// spellings. Do not reinterpret a filesystem retarget as an
			// intentional row edit; persistence validation must fail closed.
			continue
		}
		probe, _, err := sandboxpolicy.NormalizeForPersistence(sandboxpolicy.Profile{
			Name: "filesystem-authoring-probe",
			Filesystem: []sandboxpolicy.FilesystemGrant{{
				Path: clean, Access: grant.Access,
			}},
		})
		if err != nil {
			// The normal authoring/persistence validator owns the actionable
			// error; structural detection must not weaken or replace it.
			continue
		}
		resolved := probe.Filesystem[0].Path
		if clean == resolved {
			continue
		}
		if _, ok := retained[resolved][clean]; !ok {
			return true, nil
		}
	}
	for _, rule := range spellings.Rules {
		if _, ok := authoritative[rule.ResolvedPath]; !ok {
			return true, nil
		}
	}
	return false, nil
}

func GetSandboxProfile(name string) (*SandboxProfile, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return scanSandboxProfile(d.QueryRow(sandboxProfileSelect+` WHERE name = ?`, name))
}

func GetSandboxProfileByID(id int64) (*SandboxProfile, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return scanSandboxProfile(d.QueryRow(sandboxProfileSelect+` WHERE id = ?`, id))
}

const sandboxProfileSelect = `SELECT id, name, filesystem_json, filesystem_spellings_json, environment_json, agent_directories_json, filesystem_root, network_access, network_json, unix_sockets_json, resource_limits_json, darwin_allow_mach_register, pre_launch_json, includes_json, created_at, updated_at FROM sandbox_profiles`

func scanSandboxProfile(row rowScanner) (*SandboxProfile, error) {
	var p SandboxProfile
	var filesystemJSON, filesystemSpellingsJSON, environmentJSON, agentDirectoriesJSON, networkJSON, unixSocketsJSON, resourceLimitsJSON, preLaunchJSON, includesJSON string
	var createdAt, updatedAt dbTimestamp
	if err := row.Scan(
		&p.ID, &p.Name, &filesystemJSON, &filesystemSpellingsJSON, &environmentJSON, &agentDirectoriesJSON,
		&p.FilesystemRoot, &p.NetworkAccess, &networkJSON, &unixSocketsJSON, &resourceLimitsJSON, &p.DarwinAllowMachRegister,
		&preLaunchJSON, &includesJSON, &createdAt, &updatedAt,
	); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(filesystemJSON), &p.Filesystem); err != nil {
		return nil, fmt.Errorf("decode sandbox profile %q filesystem: %w", p.Name, err)
	}
	if filesystemSpellingsJSON != "" {
		if err := json.Unmarshal(
			[]byte(filesystemSpellingsJSON), &p.FilesystemSpellings,
		); err != nil {
			return nil, fmt.Errorf(
				"decode sandbox profile %q filesystem spellings: %w", p.Name, err,
			)
		}
	}
	if err := json.Unmarshal([]byte(environmentJSON), &p.Environment); err != nil {
		return nil, fmt.Errorf("decode sandbox profile %q environment: %w", p.Name, err)
	}
	if err := json.Unmarshal([]byte(agentDirectoriesJSON), &p.AgentDirectories); err != nil {
		return nil, fmt.Errorf("decode sandbox profile %q agent directories: %w", p.Name, err)
	}
	if networkJSON != "" {
		if err := json.Unmarshal([]byte(networkJSON), &p.Network); err != nil {
			return nil, fmt.Errorf("decode sandbox profile %q network rules: %w", p.Name, err)
		}
	}
	if unixSocketsJSON != "" {
		if err := json.Unmarshal([]byte(unixSocketsJSON), &p.UnixSockets); err != nil {
			return nil, fmt.Errorf("decode sandbox profile %q Unix-socket rules: %w", p.Name, err)
		}
	}
	if resourceLimitsJSON != "" {
		if err := json.Unmarshal([]byte(resourceLimitsJSON), &p.ResourceLimits); err != nil {
			return nil, fmt.Errorf("decode sandbox profile %q resource limits: %w", p.Name, err)
		}
	}
	// A row written before the column existed decodes to no blocks, which is
	// what an absent payload means.
	if preLaunchJSON != "" {
		if err := json.Unmarshal([]byte(preLaunchJSON), &p.PreLaunch); err != nil {
			return nil, fmt.Errorf("decode sandbox profile %q pre-launch blocks: %w", p.Name, err)
		}
	}
	if err := json.Unmarshal([]byte(includesJSON), &p.Includes); err != nil {
		return nil, fmt.Errorf("decode sandbox profile %q includes: %w", p.Name, err)
	}
	if p.Filesystem == nil {
		p.Filesystem = []SandboxFilesystemGrant{}
	}
	if p.Environment == nil {
		p.Environment = []SandboxEnvironmentEntry{}
	}
	if p.AgentDirectories == nil {
		p.AgentDirectories = []string{}
	}
	if p.Includes == nil {
		p.Includes = []string{}
	}
	p.CreatedAt = createdAt.Time()
	p.UpdatedAt = updatedAt.Time()
	// These paths were canonical at persistence time, but that is not a durable
	// authorization proof: directories can be replaced by symlinks later. The
	// TCL-320 launch/application boundary must call sandboxpolicy.Normalize
	// again immediately before rendering any harness grant.
	return &p, nil
}

func ListSandboxProfiles() ([]*SandboxProfile, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(sandboxProfileSelect + ` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*SandboxProfile{}
	for rows.Next() {
		p, err := scanSandboxProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ImportSandboxProfiles preserves the original source-compatible import API.
// Callers that need to pass additional import decisions use
// ImportSandboxProfilesWithOptions.
func ImportSandboxProfiles(profiles []*SandboxProfile, onConflict string, assignments *SandboxProfileAssignments) (SandboxProfileImportResult, error) {
	return ImportSandboxProfilesWithOptions(profiles, SandboxProfileImportOptions{
		OnConflict: onConflict, Assignments: assignments,
	})
}

// ImportSandboxProfilesWithOptions validates and applies a portable profile
// bundle in one transaction. Expected conflicts, acknowledgement exposure, and
// optional assignment restoration all use the same plan before the first
// write, so an error never leaves a partially imported registry.
func ImportSandboxProfilesWithOptions(profiles []*SandboxProfile, opts SandboxProfileImportOptions) (SandboxProfileImportResult, error) {
	result := SandboxProfileImportResult{
		Imported: []string{}, Skipped: []string{}, Warnings: []string{},
		AccessNotices: []sandboxpolicy.AccessNotice{},
	}
	onConflict := opts.OnConflict
	onConflict = strings.ToLower(strings.TrimSpace(onConflict))
	if onConflict == "" {
		onConflict = "error"
	}
	if onConflict != "error" && onConflict != "skip" && onConflict != "overwrite" {
		return result, fmt.Errorf("%w: on_conflict must be error, skip, or overwrite", ErrSandboxProfileInvalidImport)
	}
	normalized := make([]*SandboxProfile, 0, len(profiles))
	missingByName := make(map[string][]string, len(profiles))
	seen := map[string]bool{}
	for i, profile := range profiles {
		if profile == nil {
			return result, fmt.Errorf("%w: profile #%d: sandbox profile is nil", ErrSandboxProfileInvalidImport, i+1)
		}
		p, missing, err := sandboxpolicy.NormalizeForImport(sandboxpolicy.Profile{
			Name: profile.Name, Filesystem: profile.Filesystem,
			FilesystemSpellings: profile.FilesystemSpellings,
			Environment:         profile.Environment, AgentDirectories: profile.AgentDirectories, FilesystemRoot: profile.FilesystemRoot, NetworkAccess: profile.NetworkAccess,
			Network: profile.Network, UnixSockets: profile.UnixSockets, ResourceLimits: profile.ResourceLimits, Includes: profile.Includes,
			DarwinAllowMachRegister: profile.DarwinAllowMachRegister,
			PreLaunch:               profile.PreLaunch,
		})
		if err != nil {
			return result, fmt.Errorf("%w: profile #%d: %v", ErrSandboxProfileInvalidImport, i+1, err)
		}
		normalizedProfile := *profile
		normalizedProfile.Name = p.Name
		normalizedProfile.Filesystem = p.Filesystem
		normalizedProfile.FilesystemSpellings = p.FilesystemSpellings
		normalizedProfile.Environment = p.Environment
		normalizedProfile.AgentDirectories = p.AgentDirectories
		normalizedProfile.FilesystemRoot = p.FilesystemRoot
		normalizedProfile.NetworkAccess = p.NetworkAccess
		normalizedProfile.Network = p.Network
		normalizedProfile.UnixSockets = p.UnixSockets
		normalizedProfile.ResourceLimits = p.ResourceLimits
		normalizedProfile.DarwinAllowMachRegister = p.DarwinAllowMachRegister
		normalizedProfile.Includes = p.Includes
		if seen[normalizedProfile.Name] {
			return result, fmt.Errorf("%w: sandbox profile %q appears more than once", ErrSandboxProfileInvalidImport, normalizedProfile.Name)
		}
		seen[normalizedProfile.Name] = true
		missingByName[normalizedProfile.Name] = missing
		normalized = append(normalized, &normalizedProfile)
	}

	d, err := Open()
	if err != nil {
		return result, err
	}
	tx, err := d.Begin()
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	registry, err := sandboxProfileRegistryInTx(tx)
	if err != nil {
		return result, err
	}
	plans := make([]sandboxProfileImportPlan, 0, len(normalized))
	for _, profile := range normalized {
		existing := registry[profile.Name]
		if existing != nil && onConflict == "error" {
			return result, fmt.Errorf("%w: %q", ErrSandboxProfileNameTaken, profile.Name)
		}
		item := sandboxProfileImportPlan{profile: profile}
		if existing != nil {
			item.existingID = existing.ID
			item.skipped = onConflict == "skip"
		}
		plans = append(plans, item)
		if !item.skipped {
			plannedProfile := *profile
			plannedProfile.ID = item.existingID
			registry[profile.Name] = &plannedProfile
		}
	}
	if err := validateSandboxProfileRegistry(registry); err != nil {
		return result, fmt.Errorf("%w: %v", ErrSandboxProfileInvalidImport, err)
	}
	assignmentPlan, err := planSandboxProfileAssignments(tx, registry, opts.Assignments)
	if err != nil {
		return result, err
	}
	now := dbTime(time.Now())
	for _, item := range plans {
		if item.skipped {
			result.Skipped = append(result.Skipped, item.profile.Name)
			continue
		}
		for _, path := range missingByName[item.profile.Name] {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"sandbox profile %q path %q does not exist locally; the rule will target it if created", item.profile.Name, path))
		}
		payload, err := marshalSandboxProfilePayload(item.profile)
		if err != nil {
			return result, err
		}
		if item.existingID != 0 {
			if _, err := tx.Exec(`UPDATE sandbox_profiles SET filesystem_json = ?, filesystem_spellings_json = ?, environment_json = ?, agent_directories_json = ?, filesystem_root = ?, network_access = ?, network_json = ?, unix_sockets_json = ?, resource_limits_json = ?, darwin_allow_mach_register = ?, pre_launch_json = ?, includes_json = ?, updated_at = ? WHERE id = ?`,
				payload.filesystem, payload.filesystemSpellings, payload.environment, payload.agentDirectories, item.profile.FilesystemRoot, item.profile.NetworkAccess,
				payload.network, payload.unixSockets, payload.resourceLimits, item.profile.DarwinAllowMachRegister,
				payload.preLaunch, payload.includes, now, item.existingID); err != nil {
				return result, err
			}
		} else if _, err := tx.Exec(`INSERT INTO sandbox_profiles
			(name, filesystem_json, filesystem_spellings_json, environment_json, agent_directories_json, filesystem_root, network_access, network_json, unix_sockets_json, resource_limits_json, darwin_allow_mach_register, pre_launch_json, includes_json, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			item.profile.Name, payload.filesystem, payload.filesystemSpellings, payload.environment, payload.agentDirectories,
			item.profile.FilesystemRoot, item.profile.NetworkAccess, payload.network, payload.unixSockets, payload.resourceLimits, item.profile.DarwinAllowMachRegister,
			payload.preLaunch, payload.includes, now, now); err != nil {
			if isUniqueViolation(err) {
				return result, ErrSandboxProfileNameTaken
			}
			return result, err
		}
		result.Imported = append(result.Imported, item.profile.Name)
	}
	// The bundle and the local registry are one graph after the writes above:
	// a bundle profile may include a local one and vice versa (overwrite).
	// Dangling or cyclic includes roll the whole import back.
	if err := validateSandboxProfileIncludeGraph(tx); err != nil {
		return result, fmt.Errorf("%w: %v", ErrSandboxProfileInvalidImport, err)
	}
	finalRegistry, err := sandboxProfileRegistryInTx(tx)
	if err != nil {
		return result, err
	}
	for _, item := range plans {
		if item.skipped {
			continue
		}
		_, notices, err := flattenSandboxProfileInRegistry(finalRegistry[item.profile.Name], finalRegistry)
		if err != nil {
			return result, fmt.Errorf("%w: %v", ErrSandboxProfileInvalidImport, err)
		}
		result.AccessNotices = append(result.AccessNotices, notices...)
	}

	result.Warnings = append(result.Warnings, assignmentPlan.warnings...)
	if assignmentPlan.global != "" {
		var id int64
		if err := tx.QueryRow(`SELECT id FROM sandbox_profiles WHERE name = ?`, assignmentPlan.global).Scan(&id); err != nil {
			return result, fmt.Errorf("resolve planned global sandbox profile %q: %w", assignmentPlan.global, err)
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO sandbox_profile_global_assignment (id, profile_name, profile_id) VALUES (1, ?, ?)`, assignmentPlan.global, id); err != nil {
			return result, err
		}
	}
	for _, assignment := range assignmentPlan.groups {
		var profileID int64
		if err := tx.QueryRow(`SELECT id FROM sandbox_profiles WHERE name = ?`, assignment.profile).Scan(&profileID); err != nil {
			return result, fmt.Errorf("resolve planned sandbox profile %q for group %q: %w", assignment.profile, assignment.group, err)
		}
		if _, err := tx.Exec(`UPDATE agent_groups SET sandbox_profile = ?, sandbox_profile_id = ? WHERE id = ?`, assignment.profile, profileID, assignment.groupID); err != nil {
			return result, err
		}
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

func sandboxProfileRegistryInTx(tx *sql.Tx) (map[string]*SandboxProfile, error) {
	rows, err := tx.Query(sandboxProfileSelect + ` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	registry := map[string]*SandboxProfile{}
	for rows.Next() {
		profile, err := scanSandboxProfile(rows)
		if err != nil {
			return nil, err
		}
		registry[profile.Name] = profile
	}
	return registry, rows.Err()
}

func validateSandboxProfileRegistry(registry map[string]*SandboxProfile) error {
	for name, profile := range registry {
		if _, _, err := flattenSandboxProfileInRegistry(profile, registry); err != nil {
			return fmt.Errorf("sandbox profile %q: %w", name, err)
		}
	}
	return nil
}

func flattenSandboxProfileInRegistry(
	profile *SandboxProfile,
	registry map[string]*SandboxProfile,
) (sandboxpolicy.Profile, []sandboxpolicy.AccessNotice, error) {
	toPolicy := func(p *SandboxProfile) sandboxpolicy.Profile {
		return sandboxpolicy.Profile{
			Name: p.Name, Filesystem: p.Filesystem,
			FilesystemSpellings: p.FilesystemSpellings,
			Environment:         p.Environment,
			AgentDirectories:    p.AgentDirectories, FilesystemRoot: p.FilesystemRoot, NetworkAccess: p.NetworkAccess,
			Network: p.Network, UnixSockets: p.UnixSockets, ResourceLimits: p.ResourceLimits, Includes: p.Includes,
			DarwinAllowMachRegister: p.DarwinAllowMachRegister,
			PreLaunch:               p.PreLaunch,
		}
	}
	return sandboxpolicy.FlattenWithNotices(toPolicy(profile), func(name string) (*sandboxpolicy.Profile, error) {
		included := registry[name]
		if included == nil {
			return nil, nil
		}
		policy := toPolicy(included)
		return &policy, nil
	})
}

// SandboxProfileCompositionNotices evaluates one proposed create/update
// against the current include registry without mutating it. HTTP preview and
// save responses use this sibling diagnostic channel; the profile value and
// export wire remain free of transient notices.
func SandboxProfileCompositionNotices(profile *SandboxProfile) ([]sandboxpolicy.AccessNotice, error) {
	if profile == nil {
		return nil, errors.New("sandbox profile is nil")
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	registry, err := sandboxProfileRegistryInTx(tx)
	if err != nil {
		return nil, err
	}
	proposed := *profile
	registry[proposed.Name] = &proposed
	_, notices, err := flattenSandboxProfileInRegistry(&proposed, registry)
	return notices, err
}

// planSandboxProfileAssignments decides exactly which requested assignments
// can mutate authority in this transaction. Exposure calculation and writes
// consume this same value; missing groups or profiles are warnings, never
// acknowledgement carriers that the mutation path would later discard.
func planSandboxProfileAssignments(
	tx *sql.Tx,
	registry map[string]*SandboxProfile,
	assignments *SandboxProfileAssignments,
) (sandboxProfileAssignmentPlan, error) {
	var plan sandboxProfileAssignmentPlan
	if assignments == nil {
		return plan, nil
	}
	if assignments.Global != "" {
		if registry[assignments.Global] == nil {
			plan.warnings = append(plan.warnings, fmt.Sprintf(
				"global assignment references missing sandbox profile %q", assignments.Global))
		} else {
			plan.global = assignments.Global
		}
	}
	groups := make([]string, 0, len(assignments.Groups))
	for group := range assignments.Groups {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	for _, group := range groups {
		profile := assignments.Groups[group]
		var groupID int64
		if err := tx.QueryRow(`SELECT id FROM agent_groups WHERE name = ?`, group).Scan(&groupID); errors.Is(err, sql.ErrNoRows) {
			plan.warnings = append(plan.warnings, fmt.Sprintf("group assignment skipped: no group %q", group))
			continue
		} else if err != nil {
			return plan, err
		}
		if registry[profile] == nil {
			plan.warnings = append(plan.warnings, fmt.Sprintf(
				"group %q assignment references missing sandbox profile %q", group, profile))
			continue
		}
		plan.groups = append(plan.groups, sandboxProfileGroupAssignmentPlan{
			group: group, groupID: groupID, profile: profile,
		})
	}
	return plan, nil
}

func DeleteSandboxProfile(name string) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var id int64
	if err := tx.QueryRow(`SELECT id FROM sandbox_profiles WHERE name = ?`, name).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	// Assignments merely reference the profile and are cleared below, but an
	// include is part of another profile's content: silently dropping it would
	// silently shrink that profile. Fail loudly and let the operator edit the
	// referrers first.
	graph, err := loadSandboxProfileIncludeGraph(tx)
	if err != nil {
		return 0, err
	}
	referrers := make([]string, 0, len(graph))
	for referrer, includes := range graph {
		if slices.Contains(includes, name) {
			referrers = append(referrers, referrer)
		}
	}
	if len(referrers) > 0 {
		sort.Strings(referrers)
		return 0, fmt.Errorf("%w: %s", ErrSandboxProfileIncludedBy, strings.Join(referrers, ", "))
	}
	if _, err := tx.Exec(`UPDATE agent_groups SET sandbox_profile = '', sandbox_profile_id = NULL WHERE sandbox_profile_id = ?`, id); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM sandbox_profile_global_assignment WHERE profile_id = ?`, id); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`DELETE FROM sandbox_profiles WHERE id = ?`, id)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// SetGlobalSandboxProfile sets the single durable operator-wide assignment. A
// blank name clears it; non-blank names must resolve to an existing profile.
func SetGlobalSandboxProfile(name string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if name == "" {
		if _, err := tx.Exec(`DELETE FROM sandbox_profile_global_assignment WHERE id = 1`); err != nil {
			return err
		}
		return tx.Commit()
	}
	var id int64
	if err := tx.QueryRow(`SELECT id FROM sandbox_profiles WHERE name = ?`, name).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return ErrSandboxProfileNotFound
	} else if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO sandbox_profile_global_assignment (id, profile_name, profile_id) VALUES (1, ?, ?)`, name, id); err != nil {
		return err
	}
	return tx.Commit()
}

func GetGlobalSandboxProfile() (*SandboxProfile, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var id int64
	if err := d.QueryRow(`SELECT profile_id FROM sandbox_profile_global_assignment WHERE id = 1`).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return GetSandboxProfileByID(id)
}

func SetAgentGroupSandboxProfile(group, name string) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var profileID sql.NullInt64
	if name != "" {
		profileID, err = registryIDByName(tx, "sandbox_profiles", name)
		if err != nil {
			return 0, err
		}
		if !profileID.Valid {
			return 0, ErrSandboxProfileNotFound
		}
	}
	res, err := tx.Exec(`UPDATE agent_groups SET sandbox_profile = ?, sandbox_profile_id = ? WHERE name = ?`, name, profileID, group)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

func GetAgentGroupSandboxProfile(group string) (*SandboxProfile, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var id sql.NullInt64
	if err := d.QueryRow(`SELECT sandbox_profile_id FROM agent_groups WHERE name = ?`, group).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if !id.Valid {
		return nil, nil
	}
	return GetSandboxProfileByID(id.Int64)
}

// ResolveEffectiveSandboxSnapshot atomically reads the stable global/group
// assignments plus an optional explicit human selection, then freezes their
// composed values. Mutable profile references are never returned as launch
// authority: only the versioned value snapshot is.
func ResolveEffectiveSandboxSnapshot(groupID int64, explicitName string) (sandboxpolicy.Snapshot, error) {
	return resolveEffectiveSandboxSnapshot(groupID, strings.TrimSpace(explicitName), 0)
}

// ResolveEffectiveSandboxSnapshotByID is the lifecycle-boundary counterpart to
// ResolveEffectiveSandboxSnapshot. A resumed agent's explicit profile is
// identified by the stable registry ID recorded in its previous snapshot, so a
// profile rename does not silently drop or retarget that explicit policy.
func ResolveEffectiveSandboxSnapshotByID(groupID, explicitProfileID int64) (sandboxpolicy.Snapshot, error) {
	return resolveEffectiveSandboxSnapshot(groupID, "", explicitProfileID)
}

func resolveEffectiveSandboxSnapshot(groupID int64, explicitName string, explicitProfileID int64) (sandboxpolicy.Snapshot, error) {
	d, err := Open()
	if err != nil {
		return sandboxpolicy.Snapshot{}, err
	}
	tx, err := d.Begin()
	if err != nil {
		return sandboxpolicy.Snapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()

	loadByID := func(id int64) (*SandboxProfile, error) {
		if id == 0 {
			return nil, nil
		}
		profile, err := scanSandboxProfile(tx.QueryRow(sandboxProfileSelect+` WHERE id = ?`, id))
		if err != nil {
			return nil, err
		}
		if profile == nil {
			return nil, fmt.Errorf("sandbox profile id %d referenced by assignment was not found", id)
		}
		return profile, nil
	}
	var globalID, groupProfileID int64
	if err := tx.QueryRow(`SELECT COALESCE((SELECT profile_id FROM sandbox_profile_global_assignment WHERE id = 1), 0)`).Scan(&globalID); err != nil {
		return sandboxpolicy.Snapshot{}, err
	}
	if groupID > 0 {
		if err := tx.QueryRow(`SELECT COALESCE(sandbox_profile_id, 0) FROM agent_groups WHERE id = ?`, groupID).Scan(&groupProfileID); errors.Is(err, sql.ErrNoRows) {
			return sandboxpolicy.Snapshot{}, fmt.Errorf("agent group %d not found", groupID)
		} else if err != nil {
			return sandboxpolicy.Snapshot{}, err
		}
	}
	global, err := loadByID(globalID)
	if err != nil {
		return sandboxpolicy.Snapshot{}, err
	}
	group, err := loadByID(groupProfileID)
	if err != nil {
		return sandboxpolicy.Snapshot{}, err
	}
	var explicit *SandboxProfile
	if explicitProfileID > 0 {
		explicit, err = scanSandboxProfile(tx.QueryRow(sandboxProfileSelect+` WHERE id = ?`, explicitProfileID))
		if errors.Is(err, sql.ErrNoRows) || explicit == nil {
			return sandboxpolicy.Snapshot{}, ErrSandboxProfileNotFound
		}
		if err != nil {
			return sandboxpolicy.Snapshot{}, err
		}
	} else if explicitName != "" {
		explicit, err = scanSandboxProfile(tx.QueryRow(sandboxProfileSelect+` WHERE name = ?`, explicitName))
		if errors.Is(err, sql.ErrNoRows) || explicit == nil {
			return sandboxpolicy.Snapshot{}, ErrSandboxProfileNotFound
		}
		if err != nil {
			return sandboxpolicy.Snapshot{}, err
		}
	}

	toPolicy := func(p *SandboxProfile) *sandboxpolicy.Profile {
		if p == nil {
			return nil
		}
		return &sandboxpolicy.Profile{
			Name: p.Name, Filesystem: p.Filesystem,
			FilesystemSpellings: p.FilesystemSpellings,
			Environment:         p.Environment, AgentDirectories: p.AgentDirectories, NetworkAccess: p.NetworkAccess,
			FilesystemRoot: p.FilesystemRoot,
			Network:        p.Network, UnixSockets: p.UnixSockets, ResourceLimits: p.ResourceLimits, Includes: p.Includes,
			DarwinAllowMachRegister: p.DarwinAllowMachRegister,
			PreLaunch:               p.PreLaunch,
		}
	}
	// Includes are expanded inside the same transaction that read the
	// assignments, so the flattened values and the applied provenance describe
	// one consistent registry state. A dangling or cyclic include (possible
	// only if the DB was edited outside tclaude) fails the launch closed.
	lookupForFlatten := func(name string) (*sandboxpolicy.Profile, error) {
		p, err := scanSandboxProfile(tx.QueryRow(sandboxProfileSelect+` WHERE name = ?`, name))
		if err != nil {
			return nil, err
		}
		return toPolicy(p), nil
	}
	flatten := func(p *SandboxProfile) (*sandboxpolicy.Profile, []sandboxpolicy.AccessNotice, error) {
		if p == nil {
			return nil, nil, nil
		}
		flattened, notices, err := sandboxpolicy.FlattenWithNotices(*toPolicy(p), lookupForFlatten)
		if err != nil {
			return nil, nil, fmt.Errorf("flatten sandbox profile %q: %w", p.Name, err)
		}
		return &flattened, notices, nil
	}
	globalPolicy, globalNotices, err := flatten(global)
	if err != nil {
		return sandboxpolicy.Snapshot{}, err
	}
	groupPolicy, groupNotices, err := flatten(group)
	if err != nil {
		return sandboxpolicy.Snapshot{}, err
	}
	explicitPolicy, explicitNotices, err := flatten(explicit)
	if err != nil {
		return sandboxpolicy.Snapshot{}, err
	}
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{
		Global: globalPolicy, Group: groupPolicy, Explicit: explicitPolicy,
	})
	if err != nil {
		return sandboxpolicy.Snapshot{}, err
	}
	resolutionNotices := effective.AccessNotices
	effective.AccessNotices = nil
	for _, notices := range [][]sandboxpolicy.AccessNotice{
		globalNotices, groupNotices, explicitNotices, resolutionNotices,
	} {
		if len(notices) > 0 {
			effective.AccessNotices = append(effective.AccessNotices, notices...)
		}
	}
	applied := make([]sandboxpolicy.AppliedProfile, 0, 3)
	for _, item := range []struct {
		scope   sandboxpolicy.Scope
		profile *SandboxProfile
	}{
		{sandboxpolicy.ScopeGlobal, global},
		{sandboxpolicy.ScopeGroup, group},
		{sandboxpolicy.ScopeExplicit, explicit},
	} {
		if item.profile != nil {
			applied = append(applied, sandboxpolicy.AppliedProfile{
				Scope: item.scope, ID: item.profile.ID, Name: item.profile.Name, UpdatedAt: item.profile.UpdatedAt,
			})
		}
	}
	if err := tx.Commit(); err != nil {
		return sandboxpolicy.Snapshot{}, err
	}
	snapshot := sandboxpolicy.NewSnapshot(effective, applied)
	snapshot.ResolutionGroupID = groupID
	return snapshot, nil
}
