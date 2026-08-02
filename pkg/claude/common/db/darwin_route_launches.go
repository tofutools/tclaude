package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	DarwinRouteLaunchPending = "pending"
	DarwinRouteLaunchActive  = "active"
	DarwinRouteLaunchClosed  = "closed"
	maxDarwinRouteSlots      = 16
)

type DarwinRouteLaunch struct {
	AgentID          string
	ConvID           string
	LaunchGeneration string
	Slots            []int
	State            string
	CreatedAt        time.Time
	ClosedAt         time.Time
}

func validateDarwinRouteLaunchIdentity(agentID, convID, generation string) error {
	if strings.TrimSpace(agentID) == "" || strings.TrimSpace(convID) == "" || strings.TrimSpace(generation) == "" {
		return errors.New("darwin route launch requires agent, conversation, and generation")
	}
	return nil
}

func encodeDarwinRouteLaunchSlots(slots []int) (string, error) {
	if len(slots) == 0 || len(slots) > maxDarwinRouteSlots {
		return "", errors.New("darwin route launch requires 1–16 slots")
	}
	seen := make(map[int]struct{}, len(slots))
	values := make([]string, len(slots))
	for i, slot := range slots {
		if slot < 1 || slot > 65535 {
			return "", fmt.Errorf("darwin route launch slot %d is outside TCP range", slot)
		}
		if _, ok := seen[slot]; ok {
			return "", fmt.Errorf("darwin route launch slot %d is duplicated", slot)
		}
		seen[slot] = struct{}{}
		values[i] = strconv.Itoa(slot)
	}
	return strings.Join(values, ","), nil
}

func decodeDarwinRouteLaunchSlots(raw string) ([]int, error) {
	parts := strings.Split(raw, ",")
	slots := make([]int, len(parts))
	for i, part := range parts {
		value, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("decode Darwin route launch slots: %w", err)
		}
		slots[i] = value
	}
	if _, err := encodeDarwinRouteLaunchSlots(slots); err != nil {
		return nil, err
	}
	return slots, nil
}

func RegisterDarwinRouteLaunch(agentID, convID, generation string, slots []int) error {
	if err := validateDarwinRouteLaunchIdentity(agentID, convID, generation); err != nil {
		return err
	}
	encoded, err := encodeDarwinRouteLaunchSlots(slots)
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
	createdAt := dbTime(time.Now())
	if _, err := tx.Exec(`INSERT INTO darwin_route_launches
		(agent_id, conv_id, launch_generation, slots, state, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, agentID, convID, generation, encoded,
		DarwinRouteLaunchPending, createdAt); err != nil {
		return err
	}
	for _, slot := range slots {
		if _, err := tx.Exec(`INSERT INTO darwin_route_slot_claims
			(slot, agent_id, conv_id, launch_generation, state, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`, slot, agentID, convID, generation,
			DarwinRouteLaunchPending, createdAt); err != nil {
			return fmt.Errorf("claim Darwin route slot %d: %w", slot, err)
		}
	}
	return tx.Commit()
}

func ActivateDarwinRouteLaunch(agentID, convID, generation string) error {
	if err := validateDarwinRouteLaunchIdentity(agentID, convID, generation); err != nil {
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
	result, err := tx.Exec(`UPDATE darwin_route_launches SET state = ?
		WHERE agent_id = ? AND conv_id = ? AND launch_generation = ? AND state = ?`,
		DarwinRouteLaunchActive, agentID, convID, generation, DarwinRouteLaunchPending)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("darwin route launch is missing or no longer pending")
	}
	result, err = tx.Exec(`UPDATE darwin_route_slot_claims SET state = ?
		WHERE agent_id = ? AND conv_id = ? AND launch_generation = ? AND state = ?`,
		DarwinRouteLaunchActive, agentID, convID, generation, DarwinRouteLaunchPending)
	if err != nil {
		return err
	}
	claimCount, err := result.RowsAffected()
	if err != nil {
		return err
	}
	launch, err := GetDarwinRouteLaunchTx(tx, agentID, convID, generation)
	if err != nil {
		return err
	}
	if claimCount != int64(len(launch.Slots)) {
		return errors.New("darwin route launch slot claims are incomplete")
	}
	return tx.Commit()
}

func GetDarwinRouteLaunchTx(tx *sql.Tx, agentID, convID, generation string) (*DarwinRouteLaunch, error) {
	return getDarwinRouteLaunchQuery(darwinRouteLaunchQuery(tx.QueryRow), agentID, convID, generation)
}

type darwinRouteLaunchQuery func(query string, args ...any) *sql.Row

func (query darwinRouteLaunchQuery) QueryRow(statement string, args ...any) *sql.Row {
	return query(statement, args...)
}

type darwinRouteLaunchQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func getDarwinRouteLaunchQuery(queryer darwinRouteLaunchQuerier, agentID, convID, generation string) (*DarwinRouteLaunch, error) {
	var launch DarwinRouteLaunch
	var rawSlots string
	var createdAt, closedAt dbTimestamp
	err := queryer.QueryRow(`SELECT agent_id, conv_id, launch_generation, slots, state, created_at, closed_at
		FROM darwin_route_launches WHERE agent_id = ? AND conv_id = ? AND launch_generation = ?`,
		agentID, convID, generation).Scan(&launch.AgentID, &launch.ConvID,
		&launch.LaunchGeneration, &rawSlots, &launch.State, &createdAt, &closedAt)
	if err != nil {
		return nil, err
	}
	launch.Slots, err = decodeDarwinRouteLaunchSlots(rawSlots)
	if err != nil {
		return nil, err
	}
	launch.CreatedAt = createdAt.Time()
	launch.ClosedAt = closedAt.Time()
	return &launch, nil
}

func GetDarwinRouteLaunch(agentID, convID, generation string) (*DarwinRouteLaunch, error) {
	if err := validateDarwinRouteLaunchIdentity(agentID, convID, generation); err != nil {
		return nil, err
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return getDarwinRouteLaunchQuery(darwinRouteLaunchQuery(d.QueryRow), agentID, convID, generation)
}

func DeleteDarwinRouteLaunch(agentID, convID, generation string) error {
	if err := validateDarwinRouteLaunchIdentity(agentID, convID, generation); err != nil {
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
	if _, err := tx.Exec(`DELETE FROM darwin_route_slot_claims
		WHERE agent_id = ? AND conv_id = ? AND launch_generation = ?`, agentID, convID, generation); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM darwin_route_launches
		WHERE agent_id = ? AND conv_id = ? AND launch_generation = ?`, agentID, convID, generation); err != nil {
		return err
	}
	return tx.Commit()
}

func MarkDarwinRouteLaunchClosedTx(tx *sql.Tx, agentID, convID, generation string, closedAt time.Time) error {
	if strings.TrimSpace(agentID) == "" || strings.TrimSpace(convID) == "" || strings.TrimSpace(generation) == "" {
		return nil
	}
	if err := validateDarwinRouteLaunchIdentity(agentID, convID, generation); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE darwin_route_launches SET state = ?, closed_at = ?
		WHERE agent_id = ? AND conv_id = ? AND launch_generation = ? AND state IN (?, ?)`,
		DarwinRouteLaunchClosed, dbTime(closedAt), agentID, convID, generation,
		DarwinRouteLaunchPending, DarwinRouteLaunchActive); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM darwin_route_slot_claims
		WHERE agent_id = ? AND conv_id = ? AND launch_generation = ?`, agentID, convID, generation)
	return err
}
