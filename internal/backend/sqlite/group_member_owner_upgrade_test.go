package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestExistingGroupOwnerReceivesMemberCreationOnce(t *testing.T) {
	for _, old := range [][]model.Action{nil, {model.ActionReadStatus, model.ActionManageMembership}} {
		t.Run(fmt.Sprintf("prior_%d_actions", len(old)), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "state.sqlite")
			store, err := Open(path)
			require.NoError(t, err)
			raw, err := json.Marshal(old)
			require.NoError(t, err)
			_, err = store.db.ExecContext(ctx, `UPDATE roles SET actions_json=?,revision=9 WHERE id='group_owner'`, raw)
			require.NoError(t, err)
			require.NoError(t, store.Close())
			store, err = Open(path)
			require.NoError(t, err)
			role, err := roleByID(ctx, store.db, model.GroupOwnerRole)
			require.NoError(t, err)
			require.Equal(t, model.Revision(10), role.Revision)
			require.Equal(t, append(old, model.ActionCreateGroupMember), role.Actions)
			require.NoError(t, store.Close())
			store, err = Open(path)
			require.NoError(t, err)
			defer store.Close()
			again, err := roleByID(ctx, store.db, model.GroupOwnerRole)
			require.NoError(t, err)
			require.Equal(t, role, again)
		})
	}
}
