package agentd

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/skillbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/skillroots"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

type boardSkillRoot struct {
	ID               string `json:"id"`
	Label            string `json:"label"`
	Path             string `json:"path"`
	Kind             string `json:"kind"`
	GroupID          int64  `json:"group_id,omitempty"`
	Identity         string `json:"root_identity,omitempty"`
	BoundaryIdentity string `json:"boundary_identity,omitempty"`
}

func boardSkillRoots() ([]boardSkillRoot, error) {
	claude, e := skillroots.Claude()
	if e != nil {
		return nil, e
	}
	codex, e := skillroots.Codex()
	if e != nil {
		return nil, e
	}
	out := []boardSkillRoot{{ID: "claude", Label: "Claude Code", Path: claude, Kind: "harness"}, {ID: "agents", Label: "Codex shared (.agents)", Path: codex[0], Kind: "harness"}}
	if len(codex) > 1 {
		out = append(out, boardSkillRoot{ID: "codex", Label: "Codex home", Path: codex[1], Kind: "harness"})
	}
	return out, nil
}
func boardSkillPublish(in boardItemInput) ([]byte, error) {
	if len(in.Only) != 1 || !strings.HasPrefix(in.Only[0], "skills/") || len(in.Skip) > 0 || len(in.Values) > 0 {
		return nil, errors.New("publish exactly one skills/NAME item without config selectors")
	}
	name := strings.TrimPrefix(in.Only[0], "skills/")
	roots, e := boardSkillRoots()
	if e != nil {
		return nil, e
	}
	var bundles []skillbundle.Bundle
	for _, root := range roots {
		if in.SkillRoot != "" && root.ID != in.SkillRoot {
			continue
		}
		if _, e := os.Lstat(filepath.Join(root.Path, name)); os.IsNotExist(e) {
			continue
		}
		b, e := skillbundle.Export(root.Path, name)
		if e != nil {
			return nil, e
		}
		bundles = append(bundles, b)
	}
	if len(bundles) == 0 {
		return nil, errors.New("skill not found in selected local skill roots")
	}
	if len(bundles) > 1 {
		return nil, errors.New("skill name exists in several roots; select skill_root (claude, agents or codex)")
	}
	return json.Marshal(bundles[0])
}
func scanBoardItem(raw []byte, kind string) error {
	if kind == "skill" {
		_, e := skillbundle.Decode(raw)
		return e
	}
	_, e := scanBoardConfig(raw)
	return e
}

type boardSkillPreview struct {
	Kind         string                  `json:"kind"`
	Skill        string                  `json:"skill"`
	Files        []boardSkillFile        `json:"files"`
	Roots        []boardSkillRoot        `json:"skill_roots"`
	Changes      []configBundleChange    `json:"changes"`
	Warnings     []string                `json:"warnings"`
	Applied      []string                `json:"applied,omitempty"`
	PreviewToken string                  `json:"preview_token,omitempty"`
	Provenance   proto.BoardItemManifest `json:"provenance"`
}
type boardSkillFile struct {
	Path       string `json:"path"`
	Bytes      int    `json:"bytes"`
	Executable bool   `json:"executable"`
	Security   bool   `json:"security"`
	Text       string `json:"text,omitempty"`
	Binary     bool   `json:"binary,omitempty"`
}

var boardSkillImportMu sync.Mutex

func skillTargetSnapshot(root, name string) (string, bool, error) {
	info, e := os.Lstat(filepath.Join(root, name))
	if os.IsNotExist(e) {
		return "missing", false, nil
	}
	if e != nil {
		return "", false, e
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", false, errors.New("existing skill is not a regular directory; symlink targets are refused")
	}
	b, e := skillbundle.Export(root, name)
	if e != nil {
		return "", false, fmt.Errorf("inspect existing skill: %w", e)
	}
	return boardPreviewHash(b), true, nil
}
func boardSkillImport(w http.ResponseWriter, r *http.Request, op string, ref boardItemRef, m proto.BoardItemManifest, raw []byte, in boardItemInput) {
	boardSkillImportMu.Lock()
	defer boardSkillImportMu.Unlock()
	b, e := skillbundle.Decode(raw)
	if e != nil {
		writeError(w, 400, "unsafe_bundle", e.Error())
		return
	}
	if len(in.Only) > 0 || len(in.Skip) > 0 || len(in.Values) > 0 {
		writeError(w, 400, "skill_options", "skill imports use skill_targets, not config selectors")
		return
	}
	roots, e := boardSkillRoots()
	if e != nil {
		writeError(w, 400, "skill_roots", e.Error())
		return
	}
	groups, e := db.ListAgentGroups()
	if e != nil {
		writeError(w, 500, "skill_roots", e.Error())
		return
	}
	for _, group := range groups {
		if group.DefaultCwd == "" || group.IsArchived() {
			continue
		}
		base, err := filepath.EvalSymlinks(group.DefaultCwd)
		if err != nil {
			continue
		}
		info, err := os.Stat(base)
		if err != nil || !info.IsDir() {
			continue
		}
		path, err := boardSkillCanonicalRoot(filepath.Join(base, ".claude", "skills"))
		if err != nil {
			continue
		}
		roots = append(roots, boardSkillRoot{ID: "group:" + group.Name, Label: "Group " + group.Name, Path: path, Kind: "group", GroupID: group.ID, BoundaryIdentity: boardRootIdentity(info)})
	}
	if in.CustomDir != "" {
		custom, err := boardCustomRoot(in.CustomDir)
		if err != nil {
			writeError(w, 400, "custom_dir", err.Error())
			return
		}
		roots = append(roots, custom)
	}
	selected := []boardSkillRoot{}
	physical := map[string]bool{}
	seen := map[string]bool{}
	for _, id := range in.SkillTargets {
		i := slices.IndexFunc(roots, func(root boardSkillRoot) bool { return root.ID == id })
		if id == "custom" && in.CustomDir == "" {
			writeError(w, 400, "custom_dir", "enter an existing absolute custom directory")
			return
		}
		if i < 0 || seen[id] {
			writeError(w, 400, "skill_targets", "select distinct known skill roots")
			return
		}
		seen[id] = true
		target := roots[i]
		if target.Kind != "custom" {
			target.Path, e = boardSkillCanonicalRoot(target.Path)
		}
		if e != nil {
			writeError(w, 409, "skill_target", e.Error())
			return
		}
		if physical[target.Path] {
			continue
		}
		if info, err := os.Stat(target.Path); err == nil && target.Kind != "custom" {
			target.Identity = boardRootIdentity(info)
		}
		physical[target.Path] = true
		selected = append(selected, target)
	}
	preview := boardSkillPreview{Kind: "skill", Skill: b.Name, Roots: roots, Changes: []configBundleChange{}, Provenance: m}
	expected := b
	expected.Files = slices.Clone(b.Files)
	if !in.PreserveExecutable {
		for i := range expected.Files {
			expected.Files[i].Executable = false
		}
	}
	desired := boardPreviewHash(expected)
	snapshots := map[string]string{}
	for _, target := range selected {
		snapshot, exists, e := skillTargetSnapshot(target.Path, b.Name)
		if e != nil {
			writeError(w, 409, "skill_target", e.Error())
			return
		}
		snapshots[target.ID] = snapshot
		action := "create"
		if exists {
			action = "replace"
		}
		if snapshot == desired {
			action = "unchanged"
		}
		preview.Changes = append(preview.Changes, configBundleChange{Item: "skills/" + b.Name + "@" + target.ID, Action: action, Security: true, Path: filepath.Join(target.Path, b.Name)})
	}
	for _, f := range b.Files {
		file := boardSkillFile{Path: f.Path, Bytes: len(f.Data), Executable: f.Executable, Security: skillbundle.SecurityRelevant(f)}
		if utf8.Valid(f.Data) && !slices.Contains(f.Data, byte(0)) {
			file.Text = string(f.Data)
		} else {
			file.Binary = true
			if file.Security {
				data := f.Data
				if len(data) > 4096 {
					data = data[:4096]
				}
				file.Text = "Binary content, first 4096 bytes as hex (download for full inspection):\n" + hex.Dump(data)
			}
		}
		preview.Files = append(preview.Files, file)
	}
	preview.Warnings = []string{"Skills are agent instructions; scripts and executable files are security-relevant. Inspect their text before importing."}
	if !in.PreserveExecutable {
		preview.Warnings = append(preview.Warnings, "Executable bits will be removed; agents may still invoke scripts through an interpreter.")
	}
	options := boardPreviewHash(struct {
		Ref           boardItemRef
		Manifest      proto.BoardItemManifest
		Targets       []boardSkillRoot
		Replace, Exec bool
		CustomDir     string
	}{ref, m, selected, in.Replace, in.PreserveExecutable, in.CustomDir})
	current := boardPreviewHash(snapshots)
	if op == "import" {
		boardPreviewPermits.Lock()
		permit, ok := boardPreviewPermits.permits[in.PreviewToken]
		delete(boardPreviewPermits.permits, in.PreviewToken)
		boardPreviewPermits.Unlock()
		if !ok || !permit.Expires.After(time.Now()) || permit.Root != config.DataDir() || permit.Options != options {
			writeError(w, 409, "preview_required", "matching unexpired preview token required")
			return
		}
		if permit.Preview != current {
			writeError(w, 409, "preview_changed", "target skill changed since preview; preview again")
			return
		}
		if len(selected) == 0 {
			writeError(w, 400, "skill_targets", "select at least one target skill root")
			return
		}
		for _, c := range preview.Changes {
			if c.Action == "replace" && !in.Replace {
				writeError(w, 409, "overwrite_required", "existing skill requires explicit replace confirmation")
				return
			}
		}
		if e = installBoardSkill(expected, selected, preview.Changes, snapshots); e != nil {
			writeError(w, 409, "skill_import", e.Error())
			return
		}
		for _, c := range preview.Changes {
			if c.Action != "unchanged" {
				preview.Applied = append(preview.Applied, c.Item)
			}
		}
		recordFederationAudit("boards.import", m.Publisher, "", ref.Board, "skill="+b.Name, 200)
	} else {
		nonce := make([]byte, 24)
		if _, e = rand.Read(nonce); e != nil {
			writeError(w, 500, "preview", e.Error())
			return
		}
		preview.PreviewToken = hex.EncodeToString(nonce)
		boardPreviewPermits.Lock()
		for token, p := range boardPreviewPermits.permits {
			if !p.Expires.After(time.Now()) {
				delete(boardPreviewPermits.permits, token)
			}
		}
		if len(boardPreviewPermits.permits) >= 128 {
			boardPreviewPermits.Unlock()
			writeError(w, 429, "preview_limit", "too many active previews")
			return
		}
		boardPreviewPermits.permits[preview.PreviewToken] = boardPreviewPermit{options, current, time.Now().Add(10 * time.Minute), config.DataDir()}
		boardPreviewPermits.Unlock()
	}
	writeJSON(w, 200, preview)
}

func installBoardSkill(b skillbundle.Bundle, targets []boardSkillRoot, changes []configBundleChange, snapshots map[string]string) error {
	type staged struct {
		root              *os.Root
		stage, backup     string
		installed, backed bool
		expectedExisting  bool
		path, snapshot    string
	}
	stages := []*staged{}
	success := false
	defer func() {
		for i := len(stages) - 1; i >= 0; i-- {
			s := stages[i]
			if !success {
				if s.installed {
					_ = s.root.RemoveAll(b.Name)
				}
				if s.backed {
					_ = s.root.Rename(s.backup, b.Name)
				}
			}
			_ = s.root.RemoveAll(s.stage)
			if success && s.backed {
				_ = s.root.RemoveAll(s.backup)
			}
			_ = s.root.Close()
		}
	}()
	for i, target := range targets {
		if changes[i].Action == "unchanged" {
			continue
		}
		if e := os.MkdirAll(target.Path, 0700); e != nil {
			return e
		}
		root, e := os.OpenRoot(target.Path)
		if e != nil {
			return e
		}
		// Verify the actual opened directory before any staging write. Custom
		// directory replacement/retargeting cannot turn validation into an escape.
		if target.Identity != "" {
			dir, err := root.Open(".")
			if err != nil {
				_ = root.Close()
				return err
			}
			info, err := dir.Stat()
			_ = dir.Close()
			if err != nil || boardRootIdentity(info) != target.Identity {
				_ = root.Close()
				return errors.New("skill root changed since preview; preview again")
			}
		}
		token := proto.NewEnvelopeID()
		s := &staged{root: root, stage: ".tclaude-skill-" + token, backup: "tclaude-skill-backup-" + token, expectedExisting: changes[i].Action == "replace", path: target.Path, snapshot: snapshots[target.ID]}
		stages = append(stages, s)
		if e = root.Mkdir(s.stage, 0700); e != nil {
			return e
		}
		for _, f := range b.Files {
			rel := filepath.Join(s.stage, filepath.FromSlash(f.Path))
			if e = root.MkdirAll(filepath.Dir(rel), 0700); e != nil {
				return e
			}
			mode := os.FileMode(0600)
			if f.Executable {
				mode = 0700
			}
			file, e := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if e != nil {
				return e
			}
			_, e = file.Write(f.Data)
			ce := file.Close()
			if e != nil {
				return e
			}
			if ce != nil {
				return ce
			}
		}
	}
	for _, s := range stages {
		if info, e := s.root.Lstat(b.Name); e == nil {
			if !s.expectedExisting || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return errors.New("skill target changed since preview; preview again")
			}
			if e = s.root.Rename(b.Name, s.backup); e != nil {
				return e
			}
			s.backed = true
			// Check the directory actually removed from the live name, not a
			// pathname checked before staging. A concurrent edit is restored by
			// rollback rather than silently discarded with the backup.
			old, e := skillbundle.Export(s.path, s.backup)
			if e != nil {
				return fmt.Errorf("skill target changed since preview: %w", e)
			}
			old.Name = b.Name
			if boardPreviewHash(old) != s.snapshot {
				return errors.New("skill target changed since preview; preview again")
			}
		} else if !os.IsNotExist(e) {
			return e
		} else if s.expectedExisting {
			return errors.New("skill target changed since preview; preview again")
		}
		if e := s.root.Rename(s.stage, b.Name); e != nil {
			return e
		}
		s.installed = true
	}
	success = true
	return nil
}

// Resolve existing root aliases once, and bind that destination into preview
// options. Missing roots are resolved relative to their nearest real ancestor.
func boardSkillCanonicalRoot(root string) (string, error) {
	candidate := root
	var missing []string
	for {
		resolved, e := filepath.EvalSymlinks(candidate)
		if e == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(e) {
			return "", e
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return "", e
		}
		missing = append(missing, filepath.Base(candidate))
		candidate = parent
	}
}
