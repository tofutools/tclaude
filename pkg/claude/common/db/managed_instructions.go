package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const ManagedInstructionItemLimit = 1 << 20
const ManagedInstructionAgentLimit = 16 << 20

// ManagedInstruction's payload is a validated skill bundle or a UTF-8 rule.
// Blobs are content-addressed inside the private SQLite store, never harness HOME.
type ManagedInstruction struct {
	Scope   string `json:"scope"`
	GroupID int64  `json:"group_id,omitempty"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Digest  string `json:"digest"`
	Content []byte `json:"content,omitempty"`
}

func ManagedInstructionScope(groupID int64) string {
	if groupID == 0 {
		return "user"
	}
	return fmt.Sprintf("group:%d", groupID)
}
func validManagedInstruction(groupID int64, kind, name string) bool {
	return groupID >= 0 && (kind == "skill" || kind == "rule") && name != "" && len(name) <= 128 && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00") && !strings.HasPrefix(name, ".")
}

// PutManagedInstruction refuses accidental replacement; overwrite is a human
// opt-in at the API. Validation of skill files/rule text belongs to the importer.
func PutManagedInstruction(groupID int64, kind, name string, content []byte, overwrite bool) (string, error) {
	if !validManagedInstruction(groupID, kind, name) || len(content) == 0 || len(content) > ManagedInstructionItemLimit {
		return "", errors.New("invalid managed instruction or item exceeds 1 MiB")
	}
	digest := sha256.Sum256(content)
	key := hex.EncodeToString(digest[:])
	d, err := Open()
	if err != nil {
		return "", err
	}
	tx, err := d.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`INSERT OR IGNORE INTO managed_instruction_blobs(digest,content) VALUES(?,?)`, key, content); err != nil {
		return "", err
	}
	var group any
	if groupID != 0 {
		group = groupID
	}
	query := `INSERT INTO managed_instructions(scope,group_id,kind,name,digest) VALUES(?,?,?,?,?)`
	if overwrite {
		query += ` ON CONFLICT(scope,kind,name) DO UPDATE SET digest=excluded.digest`
	}
	if _, err = tx.Exec(query, ManagedInstructionScope(groupID), group, kind, name, key); err != nil {
		return "", fmt.Errorf("store managed instruction (existing names require explicit overwrite): %w", err)
	}
	if _, err = tx.Exec(`DELETE FROM managed_instruction_blobs WHERE digest NOT IN (SELECT digest FROM managed_instructions)`); err != nil {
		return "", err
	}
	return key, tx.Commit()
}
func ListManagedInstructions(groupID int64, kind string) ([]ManagedInstruction, error) {
	if groupID < 0 || (kind != "skill" && kind != "rule") {
		return nil, errors.New("invalid managed instruction scope or kind")
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT i.scope,coalesce(i.group_id,0),i.kind,i.name,i.digest,b.content FROM managed_instructions i JOIN managed_instruction_blobs b ON b.digest=i.digest WHERE scope=? AND kind=? ORDER BY name`, ManagedInstructionScope(groupID), kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ManagedInstruction{}
	for rows.Next() {
		var r ManagedInstruction
		if err = rows.Scan(&r.Scope, &r.GroupID, &r.Kind, &r.Name, &r.Digest, &r.Content); err != nil {
			return nil, err
		}
		sum := sha256.Sum256(r.Content)
		if hex.EncodeToString(sum[:]) != r.Digest {
			return nil, errors.New("managed instruction digest mismatch")
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func DeleteManagedInstruction(groupID int64, kind, name string) (bool, error) {
	if !validManagedInstruction(groupID, kind, name) {
		return false, errors.New("invalid managed instruction")
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`DELETE FROM managed_instructions WHERE scope=? AND kind=? AND name=?`, ManagedInstructionScope(groupID), kind, name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	// Frozen launch snapshots carry their own content; only live library heads
	// need the shared content-addressed blob after an item is removed.
	if _, err = tx.Exec(`DELETE FROM managed_instruction_blobs WHERE digest NOT IN (SELECT digest FROM managed_instructions)`); err != nil {
		return false, err
	}
	return n != 0, tx.Commit()
}

type ManagedInstructionLaunch struct {
	AgentID   string
	Body      string
	Resources string
	Root      string
}

func SaveManagedInstructionLaunch(p ManagedInstructionLaunch) error {
	if p.AgentID == "" {
		return errors.New("managed instruction launch requires agent ID")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO managed_instruction_launches(agent_id,body,resources,root) VALUES(?,?,?,?) ON CONFLICT(agent_id) DO UPDATE SET body=excluded.body,resources=excluded.resources,root=excluded.root`, p.AgentID, p.Body, p.Resources, p.Root)
	return err
}
func GetManagedInstructionLaunch(agentID string) (*ManagedInstructionLaunch, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var p ManagedInstructionLaunch
	err = d.QueryRow(`SELECT agent_id,body,resources,root FROM managed_instruction_launches WHERE agent_id=?`, agentID).Scan(&p.AgentID, &p.Body, &p.Resources, &p.Root)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func GetManagedInstructionLaunchByRoot(root string) (*ManagedInstructionLaunch, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var p ManagedInstructionLaunch
	err = d.QueryRow(`SELECT agent_id,body,resources,root FROM managed_instruction_launches WHERE root=? AND root<>'' LIMIT 1`, root).Scan(&p.AgentID, &p.Body, &p.Resources, &p.Root)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}
func DeleteManagedInstructionLaunch(agentID string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM managed_instruction_launches WHERE agent_id=?`, agentID)
	return err
}

// RetireManagedInstructionLaunch drops only a daemon-generated runtime directory.
// Launch snapshots embed their bytes, so library blobs can be independently GC'd.
func RetireManagedInstructionLaunch(agentID string) error {
	launch, err := GetManagedInstructionLaunch(agentID)
	if err != nil || launch == nil {
		return err
	}
	if launch.Root != "" {
		if !filepath.IsAbs(launch.Root) || !strings.HasPrefix(filepath.Base(launch.Root), "tclaude-managed-") {
			return errors.New("refusing to remove a non-managed launch directory")
		}
		if err = os.RemoveAll(launch.Root); err != nil {
			return err
		}
	}
	return DeleteManagedInstructionLaunch(agentID)
}
