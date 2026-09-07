package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
)

type importedSandboxReader map[model.SandboxProfileRef]model.SandboxPolicy

func (r importedSandboxReader) ReadSandboxRevision(_ context.Context, ref model.SandboxProfileRef) (model.SandboxPolicy, error) {
	value, ok := r[ref]
	if !ok {
		return model.SandboxPolicy{}, app.ErrNotFound
	}
	return value, nil
}

func applyImportedSandboxProfiles(ctx context.Context, tx *sql.Tx, profiles []app.SandboxProfileResult) error {
	reader := importedSandboxReader{}
	for _, result := range profiles {
		profile, revision := result.Profile, result.Revision
		if profile.ID.Validate() != nil || revision.Ref.ProfileID != profile.ID || profile.HeadRevisionID != revision.Ref.RevisionID || !profile.Archived || !profile.Imported || profile.Revision != 1 || revision.Number != 1 || result.Repeated || !reflect.DeepEqual(revision.Author, model.Principal{}) || revision.RequestID != "" {
			return app.ErrInvalid
		}
		if profile.Name == "" || profile.Name != strings.TrimSpace(profile.Name) || len(profile.Name) > 200 || !utf8.ValidString(profile.Name) || strings.ContainsRune(profile.Name, 0) {
			return app.ErrInvalid
		}
		if _, exists := reader[revision.Ref]; exists {
			return app.ErrInvalid
		}
		reader[revision.Ref] = revision.Policy
	}
	for _, result := range profiles {
		if _, err := sandboxpolicy.Resolve(ctx, result.Revision.Ref, reader); err != nil {
			return fmt.Errorf("invalid imported sandbox closure: %w", err)
		}
	}
	for _, result := range profiles {
		profile, revision := result.Profile, result.Revision
		document, err := json.Marshal(profile)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO sandbox_profiles(id,name,head_revision_id,archived,revision,document) VALUES(?,?,?,?,?,?)`, profile.ID, profile.Name, profile.HeadRevisionID, true, profile.Revision, document); err != nil {
			return err
		}
		document, err = json.Marshal(revision)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO sandbox_profile_revisions(id,profile_id,number,content_hash,document) VALUES(?,?,?,?,?)`, revision.Ref.RevisionID, profile.ID, revision.Number, revision.Ref.ContentHash, document); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) verifyImportedSandboxProfiles(ctx context.Context, profiles []app.SandboxProfileResult) error {
	for _, expected := range profiles {
		actual, err := s.SandboxProfile(ctx, expected.Profile.ID)
		if err != nil || !reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("verify imported sandbox profile %s: %v", expected.Profile.ID, err)
		}
		var matches int
		err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM sandbox_profiles p JOIN sandbox_profile_revisions r ON r.id=p.head_revision_id WHERE p.id=? AND p.name=? AND p.head_revision_id=? AND p.archived=1 AND p.revision=1 AND r.profile_id=p.id AND r.number=1 AND r.content_hash=?`, expected.Profile.ID, expected.Profile.Name, expected.Revision.Ref.RevisionID, expected.Revision.Ref.ContentHash).Scan(&matches)
		if err != nil || matches != 1 {
			return fmt.Errorf("verify imported sandbox profile %s index fields: %v", expected.Profile.ID, err)
		}
	}
	return nil
}
