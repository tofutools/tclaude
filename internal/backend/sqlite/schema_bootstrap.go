package sqlite

import "context"

// executeSchema commits the base declarations together instead of forcing a
// separate durable commit for each CREATE. Specialized data migrations retain
// their own transactions and foreign-key handling after this batch finishes.
func (s *Store) executeSchema(ctx context.Context, scripts ...string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, script := range scripts {
		if _, err := tx.ExecContext(ctx, script); err != nil {
			return err
		}
	}
	return tx.Commit()
}
