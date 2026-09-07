package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"reflect"
)

func applyImportedGroupConfigurations(ctx context.Context, tx *sql.Tx, defaults []model.GroupConfiguration) error {
	for _, in := range defaults {
		if in.GroupID.Validate() != nil || in.Profile == nil || in.Revision != 1 {
			return app.ErrInvalid
		}
		var data []byte
		if err := tx.QueryRowContext(ctx, `SELECT record FROM configuration_profile_revisions WHERE profile_id=? AND revision_id=?`, in.Profile.ProfileID, in.Profile.RevisionID).Scan(&data); err != nil {
			return classify(err)
		}
		var revision model.ConfigurationProfileRevision
		if err := json.Unmarshal(data, &revision); err != nil {
			return err
		}
		if revision.Ref != *in.Profile {
			return app.ErrConflict
		}
		// Archived selections are retained for inspection, not made eligible for
		// creation. The ordinary fresh-member path still enforces profile lifecycle.
		if _, err := tx.ExecContext(ctx, `INSERT INTO group_configurations(group_id,profile_id,revision_id,content_hash,revision,updated_at) VALUES(?,?,?,?,?,?)`, in.GroupID, in.Profile.ProfileID, in.Profile.RevisionID, in.Profile.ContentHash, in.Revision, importNanos(in.UpdatedAt)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) verifyImportedGroupConfigurations(ctx context.Context, defaults []model.GroupConfiguration) error {
	for _, expected := range defaults {
		actual, err := s.GroupConfiguration(ctx, expected.GroupID)
		if err != nil || !reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("verify imported group configuration %s: %w", expected.GroupID, err)
		}
	}
	return nil
}
