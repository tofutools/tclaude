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
	_, err = d.Exec(`INSERT INTO darwin_route_launches
		(agent_id, conv_id, launch_generation, slots, state, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, agentID, convID, generation, encoded,
		DarwinRouteLaunchPending, dbTime(time.Now()))
	return err
}

func ActivateDarwinRouteLaunch(agentID, convID, generation string) error {
	if err := validateDarwinRouteLaunchIdentity(agentID, convID, generation); err != nil {
		return err
	}
	d, err := Open()
	if err != nil {
		return err
	}
	result, err := d.Exec(`UPDATE darwin_route_launches SET state = ?
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
	return nil
}

func GetDarwinRouteLaunch(agentID, convID, generation string) (*DarwinRouteLaunch, error) {
	if err := validateDarwinRouteLaunchIdentity(agentID, convID, generation); err != nil {
		return nil, err
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var launch DarwinRouteLaunch
	var rawSlots string
	var createdAt, closedAt dbTimestamp
	err = d.QueryRow(`SELECT agent_id, conv_id, launch_generation, slots, state, created_at, closed_at
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

func DeleteDarwinRouteLaunch(agentID, convID, generation string) error {
	if err := validateDarwinRouteLaunchIdentity(agentID, convID, generation); err != nil {
		return err
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM darwin_route_launches
		WHERE agent_id = ? AND conv_id = ? AND launch_generation = ?`, agentID, convID, generation)
	return err
}

func MarkDarwinRouteLaunchClosedTx(tx *sql.Tx, agentID, convID, generation string, closedAt time.Time) error {
	if strings.TrimSpace(agentID) == "" || strings.TrimSpace(convID) == "" || strings.TrimSpace(generation) == "" {
		return nil
	}
	if err := validateDarwinRouteLaunchIdentity(agentID, convID, generation); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE darwin_route_launches SET state = ?, closed_at = ?
		WHERE agent_id = ? AND conv_id = ? AND launch_generation = ? AND state IN (?, ?)`,
		DarwinRouteLaunchClosed, dbTime(closedAt), agentID, convID, generation,
		DarwinRouteLaunchPending, DarwinRouteLaunchActive)
	return err
}
