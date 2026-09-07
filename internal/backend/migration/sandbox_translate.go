package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/tofutools/tclaude/internal/backend/app"
	sourcev228 "github.com/tofutools/tclaude/internal/backend/migration/source/v228"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
)

func (t *translator) translateSandboxProfiles(batch *app.ImportBatch) {
	rows := append([]sourcev228.Row(nil), t.inspection.Snapshot.Rows["sandbox_profiles"]...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
	byName := map[string]sourcev228.Row{}
	duplicate := map[string]bool{}
	for _, row := range rows {
		name := sourcev228.String(row.Values["name"])
		if _, ok := byName[name]; ok {
			duplicate[name] = true
		}
		byName[name] = row
	}
	results := map[string]app.SandboxProfileResult{}
	heights := map[string]int{}
	reader := sandboxTranslationReader{}
	visiting := map[string]bool{}
	var resolve func(string) (app.SandboxProfileResult, error)
	resolve = func(name string) (app.SandboxProfileResult, error) {
		if result, ok := results[name]; ok {
			return result, nil
		}
		row, ok := byName[name]
		if !ok || duplicate[name] || visiting[name] || len(visiting) > 16 {
			return app.SandboxProfileResult{}, fmt.Errorf("missing, ambiguous, cyclic or excessively deep include")
		}
		visiting[name] = true
		defer delete(visiting, name)
		if name == "" || name != strings.TrimSpace(name) || len(name) > 200 || !utf8.ValidString(name) || strings.ContainsRune(name, 0) {
			return app.SandboxProfileResult{}, fmt.Errorf("invalid sandbox profile name")
		}
		policy, includes, err := legacySandboxPolicy(row)
		if err != nil {
			return app.SandboxProfileResult{}, err
		}
		height := 0
		for _, include := range includes {
			child, err := resolve(include)
			if err != nil {
				return app.SandboxProfileResult{}, err
			}
			policy.Includes = append(policy.Includes, child.Revision.Ref)
			if heights[include]+1 > height {
				height = heights[include] + 1
			}
		}
		if height > 16 {
			return app.SandboxProfileResult{}, fmt.Errorf("include depth exceeds target bound")
		}
		// Persist the same canonical empty-value representation that public reads
		// return; the exact original []/{} spellings stay in retained source evidence.
		encoded, err := json.Marshal(policy)
		if err != nil {
			return app.SandboxProfileResult{}, err
		}
		var canonical model.SandboxPolicy
		if err = json.Unmarshal(encoded, &canonical); err != nil {
			return app.SandboxProfileResult{}, err
		}
		policy = canonical
		hash, err := sandboxpolicy.ContentHash(policy)
		if err != nil {
			return app.SandboxProfileResult{}, err
		}
		id := model.SandboxProfileID(t.id("sandbox_profiles", sourcev228.String(row.Values["id"])))
		if err := id.Validate(); err != nil {
			return app.SandboxProfileResult{}, err
		}
		revisionID := model.SandboxProfileRevisionID(stableImportID("sbr", t.inspection.Source.DatabaseHash, "sandbox_profiles\x00"+row.Key+"\x00"+hash))
		at := timeValue(row.Values["created_at"])
		result := app.SandboxProfileResult{
			Profile:  model.SandboxProfile{ID: id, Name: name, HeadRevisionID: revisionID, Archived: true, Imported: true, Revision: 1, CreatedAt: at, UpdatedAt: firstTime(timeValue(row.Values["updated_at"]), at)},
			Revision: model.SandboxProfileRevision{Ref: model.SandboxProfileRef{ProfileID: id, RevisionID: revisionID, ContentHash: hash}, Number: 1, Policy: policy, CreatedAt: at},
		}
		reader[result.Revision.Ref] = policy
		if _, err := sandboxpolicy.Resolve(context.Background(), result.Revision.Ref, reader); err != nil {
			delete(reader, result.Revision.Ref)
			return app.SandboxProfileResult{}, err
		}
		results[name] = result
		heights[name] = height
		return result, nil
	}
	for _, row := range rows {
		name := sourcev228.String(row.Values["name"])
		result, err := resolve(name)
		if err != nil {
			t.launchMetadataDiagnostic(batch, "sandbox_profiles", row.Key, "sandbox_profile_retained_unmapped", "legacy policy or its include closure cannot be represented exactly under current target constraints; the complete source record remains retained for explicit review")
			continue
		}
		batch.SandboxProfiles = append(batch.SandboxProfiles, result)
		for i := range batch.SourceRecords {
			record := &batch.SourceRecords[i]
			if record.SourceTable == "sandbox_profiles" && record.SourceKey == row.Key {
				record.Conversion = string(ConversionReady)
				record.ReasonCode = "sandbox_profile_archived_without_activation"
			}
		}
	}
}

type sandboxTranslationReader map[model.SandboxProfileRef]model.SandboxPolicy

func (r sandboxTranslationReader) ReadSandboxRevision(_ context.Context, ref model.SandboxProfileRef) (model.SandboxPolicy, error) {
	policy, ok := r[ref]
	if !ok {
		return model.SandboxPolicy{}, app.ErrNotFound
	}
	return policy, nil
}
