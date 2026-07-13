package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
	ID               int64                     `json:"id"`
	Name             string                    `json:"name"`
	Filesystem       []SandboxFilesystemGrant  `json:"filesystem"`
	Environment      []SandboxEnvironmentEntry `json:"environment"`
	AgentDirectories []string                  `json:"agent_directories"`
	Includes         []string                  `json:"includes"`
	CreatedAt        time.Time                 `json:"created_at"`
	UpdatedAt        time.Time                 `json:"updated_at"`
}

type SandboxProfileAssignments struct {
	Global string
	Groups map[string]string
}

type SandboxProfileImportResult struct {
	Imported []string
	Skipped  []string
	Warnings []string
}

func CreateSandboxProfile(p *SandboxProfile) (int64, error) {
	p, err := normalizeSandboxProfileForStore(p)
	if err != nil {
		return 0, err
	}
	filesystemJSON, environmentJSON, agentDirectoriesJSON, includesJSON, err := marshalSandboxProfilePayload(p)
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
	now := time.Now().Format(time.RFC3339Nano)
	res, err := tx.Exec(`INSERT INTO sandbox_profiles
		(name, filesystem_json, environment_json, agent_directories_json, includes_json, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.Name, filesystemJSON, environmentJSON, agentDirectoriesJSON, includesJSON, now, now)
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
	filesystemJSON, environmentJSON, agentDirectoriesJSON, includesJSON, err := marshalSandboxProfilePayload(p)
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
	now := time.Now().Format(time.RFC3339Nano)
	query := `UPDATE sandbox_profiles SET name = ?, filesystem_json = ?, environment_json = ?, agent_directories_json = ?, includes_json = ?, updated_at = ? WHERE id = ?`
	args := []any{p.Name, filesystemJSON, environmentJSON, agentDirectoriesJSON, includesJSON, now, p.ID}
	if revision != "" {
		query += ` AND updated_at = ?`
		args = append(args, revision)
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

func marshalSandboxProfilePayload(p *SandboxProfile) (string, string, string, string, error) {
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
		return "", "", "", "", fmt.Errorf("marshal sandbox profile filesystem: %w", err)
	}
	environmentJSON, err := json.Marshal(environment)
	if err != nil {
		return "", "", "", "", fmt.Errorf("marshal sandbox profile environment: %w", err)
	}
	agentDirectoriesJSON, err := json.Marshal(agentDirectories)
	if err != nil {
		return "", "", "", "", fmt.Errorf("marshal sandbox profile agent directories: %w", err)
	}
	includesJSON, err := json.Marshal(includes)
	if err != nil {
		return "", "", "", "", fmt.Errorf("marshal sandbox profile includes: %w", err)
	}
	return string(filesystemJSON), string(environmentJSON), string(agentDirectoriesJSON), string(includesJSON), nil
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
	normalized, _, err := sandboxpolicy.NormalizeForPersistence(sandboxpolicy.Profile{
		Name: p.Name, Filesystem: p.Filesystem, Environment: p.Environment, AgentDirectories: p.AgentDirectories, Includes: p.Includes,
	})
	if err != nil {
		return nil, err
	}
	out := *p
	out.Name = normalized.Name
	out.Filesystem = normalized.Filesystem
	out.Environment = normalized.Environment
	out.AgentDirectories = normalized.AgentDirectories
	out.Includes = normalized.Includes
	return &out, nil
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

const sandboxProfileSelect = `SELECT id, name, filesystem_json, environment_json, agent_directories_json, includes_json, created_at, updated_at FROM sandbox_profiles`

func scanSandboxProfile(row rowScanner) (*SandboxProfile, error) {
	var p SandboxProfile
	var filesystemJSON, environmentJSON, agentDirectoriesJSON, includesJSON, createdAt, updatedAt string
	if err := row.Scan(&p.ID, &p.Name, &filesystemJSON, &environmentJSON, &agentDirectoriesJSON, &includesJSON, &createdAt, &updatedAt); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(filesystemJSON), &p.Filesystem); err != nil {
		return nil, fmt.Errorf("decode sandbox profile %q filesystem: %w", p.Name, err)
	}
	if err := json.Unmarshal([]byte(environmentJSON), &p.Environment); err != nil {
		return nil, fmt.Errorf("decode sandbox profile %q environment: %w", p.Name, err)
	}
	if err := json.Unmarshal([]byte(agentDirectoriesJSON), &p.AgentDirectories); err != nil {
		return nil, fmt.Errorf("decode sandbox profile %q agent directories: %w", p.Name, err)
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
	p.CreatedAt = parseTimeOrZero(createdAt)
	p.UpdatedAt = parseTimeOrZero(updatedAt)
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

// ImportSandboxProfiles validates and applies a portable profile bundle in one
// transaction. Expected conflicts are resolved before the first write, and
// optional assignment restoration rides the same commit, so an error never
// leaves a partially imported registry.
func ImportSandboxProfiles(profiles []*SandboxProfile, onConflict string, assignments *SandboxProfileAssignments) (SandboxProfileImportResult, error) {
	result := SandboxProfileImportResult{Imported: []string{}, Skipped: []string{}, Warnings: []string{}}
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
			Name: profile.Name, Filesystem: profile.Filesystem, Environment: profile.Environment, AgentDirectories: profile.AgentDirectories, Includes: profile.Includes,
		})
		if err != nil {
			return result, fmt.Errorf("%w: profile #%d: %v", ErrSandboxProfileInvalidImport, i+1, err)
		}
		normalizedProfile := *profile
		normalizedProfile.Name = p.Name
		normalizedProfile.Filesystem = p.Filesystem
		normalizedProfile.Environment = p.Environment
		normalizedProfile.AgentDirectories = p.AgentDirectories
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
	type planned struct {
		profile    *SandboxProfile
		existingID int64
	}
	plans := make([]planned, 0, len(normalized))
	for _, profile := range normalized {
		var id int64
		err := tx.QueryRow(`SELECT id FROM sandbox_profiles WHERE name = ?`, profile.Name).Scan(&id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return result, err
		}
		if err == nil && onConflict == "error" {
			return result, fmt.Errorf("%w: %q", ErrSandboxProfileNameTaken, profile.Name)
		}
		plans = append(plans, planned{profile: profile, existingID: id})
	}

	now := time.Now().Format(time.RFC3339Nano)
	for _, item := range plans {
		if item.existingID != 0 && onConflict == "skip" {
			result.Skipped = append(result.Skipped, item.profile.Name)
			continue
		}
		for _, path := range missingByName[item.profile.Name] {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"sandbox profile %q path %q does not exist locally; the rule will target it if created", item.profile.Name, path))
		}
		filesystemJSON, environmentJSON, agentDirectoriesJSON, includesJSON, err := marshalSandboxProfilePayload(item.profile)
		if err != nil {
			return result, err
		}
		if item.existingID != 0 {
			if _, err := tx.Exec(`UPDATE sandbox_profiles SET filesystem_json = ?, environment_json = ?, agent_directories_json = ?, includes_json = ?, updated_at = ? WHERE id = ?`,
				filesystemJSON, environmentJSON, agentDirectoriesJSON, includesJSON, now, item.existingID); err != nil {
				return result, err
			}
		} else if _, err := tx.Exec(`INSERT INTO sandbox_profiles
			(name, filesystem_json, environment_json, agent_directories_json, includes_json, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			item.profile.Name, filesystemJSON, environmentJSON, agentDirectoriesJSON, includesJSON, now, now); err != nil {
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

	if assignments != nil {
		if assignments.Global != "" {
			var id int64
			if err := tx.QueryRow(`SELECT id FROM sandbox_profiles WHERE name = ?`, assignments.Global).Scan(&id); errors.Is(err, sql.ErrNoRows) {
				result.Warnings = append(result.Warnings, fmt.Sprintf("global assignment references missing sandbox profile %q", assignments.Global))
			} else if err != nil {
				return result, err
			} else if _, err := tx.Exec(`INSERT OR REPLACE INTO sandbox_profile_global_assignment (id, profile_name, profile_id) VALUES (1, ?, ?)`, assignments.Global, id); err != nil {
				return result, err
			}
		}
		groups := make([]string, 0, len(assignments.Groups))
		for group := range assignments.Groups {
			groups = append(groups, group)
		}
		sort.Strings(groups)
		for _, group := range groups {
			profile := assignments.Groups[group]
			var groupID, profileID int64
			if err := tx.QueryRow(`SELECT id FROM agent_groups WHERE name = ?`, group).Scan(&groupID); errors.Is(err, sql.ErrNoRows) {
				result.Warnings = append(result.Warnings, fmt.Sprintf("group assignment skipped: no group %q", group))
				continue
			} else if err != nil {
				return result, err
			}
			if err := tx.QueryRow(`SELECT id FROM sandbox_profiles WHERE name = ?`, profile).Scan(&profileID); errors.Is(err, sql.ErrNoRows) {
				result.Warnings = append(result.Warnings, fmt.Sprintf("group %q assignment references missing sandbox profile %q", group, profile))
				continue
			} else if err != nil {
				return result, err
			}
			if _, err := tx.Exec(`UPDATE agent_groups SET sandbox_profile = ?, sandbox_profile_id = ? WHERE id = ?`, profile, profileID, groupID); err != nil {
				return result, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
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
		return &sandboxpolicy.Profile{Name: p.Name, Filesystem: p.Filesystem, Environment: p.Environment, AgentDirectories: p.AgentDirectories, Includes: p.Includes}
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
	flatten := func(p *SandboxProfile) (*sandboxpolicy.Profile, error) {
		if p == nil {
			return nil, nil
		}
		flattened, err := sandboxpolicy.Flatten(*toPolicy(p), lookupForFlatten)
		if err != nil {
			return nil, fmt.Errorf("flatten sandbox profile %q: %w", p.Name, err)
		}
		return &flattened, nil
	}
	globalPolicy, err := flatten(global)
	if err != nil {
		return sandboxpolicy.Snapshot{}, err
	}
	groupPolicy, err := flatten(group)
	if err != nil {
		return sandboxpolicy.Snapshot{}, err
	}
	explicitPolicy, err := flatten(explicit)
	if err != nil {
		return sandboxpolicy.Snapshot{}, err
	}
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{
		Global: globalPolicy, Group: groupPolicy, Explicit: explicitPolicy,
	})
	if err != nil {
		return sandboxpolicy.Snapshot{}, err
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
