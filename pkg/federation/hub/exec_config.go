package hub

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const execThreat = "Scripts run as the non-root hub service user and can disrupt the whole fleet's connectivity. Pinned node identity keys still prevent reading or forging end-to-end node content."

func (s *Store) scriptSwitch(override *bool) (bool, string, error) {
	if override != nil {
		return *override, "flag", nil
	}
	path := filepath.Join(filepath.Dir(s.path), "hub-config.json")
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, "config", nil
	}
	if err != nil {
		return false, "config", err
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return false, "config", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 16<<10 {
		return false, "config", errors.New("hub-config.json must be a regular private 0600 file under16KiB")
	}
	var cfg struct {
		AcceptRemoteScripts bool `json:"accept_remote_scripts"`
	}
	decoder := json.NewDecoder(io.LimitReader(file, 16<<10))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&cfg); err != nil {
		return false, "config", err
	}
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		return false, "config", errors.New("expected one hub config object")
	}
	return cfg.AcceptRemoteScripts, "config", nil
}

// GrantExec is deliberately host-only; no signed RPC calls it. Initial elevated
// authority requires access to the hub user's shell, not bootstrap/admin status.
func (s *Store) GrantExec(instance string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`UPDATE hub_admins SET created_at=created_at WHERE 0`); err != nil {
		return err
	}
	var count int
	if err = tx.QueryRow(`SELECT count(*) FROM hub_admins a JOIN instances i ON i.instance_id=a.instance_id WHERE a.instance_id=? AND i.revoked=0 AND NOT EXISTS(SELECT 1 FROM identity_rotations r WHERE r.old_instance=i.instance_id AND r.state IN ('accepted','conflict','revoked','recovered'))`, instance).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return adminErr(409, "instance", "hub.exec needs an active existing hub admin")
	}
	if _, err = tx.Exec(`INSERT OR IGNORE INTO hub_admin_capabilities VALUES(?,'hub.exec')`, instance); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) hasAdminCapability(instance, capability string) bool {
	var count int
	query := `SELECT count(*) FROM hub_admins a JOIN instances i ON i.instance_id=a.instance_id JOIN hub_admin_capabilities c ON c.instance_id=a.instance_id WHERE a.instance_id=? AND i.revoked=0 AND NOT EXISTS(SELECT 1 FROM identity_rotations r WHERE r.old_instance=i.instance_id AND r.state IN ('accepted','conflict','revoked','recovered'))`
	args := []any{instance}
	if capability != "" {
		query += ` AND c.capability=?`
		args = append(args, capability)
	}
	return s.db.QueryRow(query, args...).Scan(&count) == nil && count > 0
}
