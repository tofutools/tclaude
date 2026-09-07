package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (s *Store) AppendAutomationFacts(ctx context.Context, source string, facts []model.NormalizedFact) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, fact := range facts {
		fact.Source = source
		if err = insertAutomationFactTx(ctx, tx, fact); err != nil {
			return err
		}
	}
	if len(facts) > 0 {
		if err = bumpTx(ctx, tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) AutomationFactsAfter(ctx context.Context, source string, resource model.AutomationFactResource, after uint64, limit uint32) ([]model.NormalizedFact, error) {
	if limit == 0 || limit > 256 {
		return nil, app.ErrInvalid
	}
	resourceJSON, _ := json.Marshal(resource)
	rows, err := s.db.QueryContext(ctx, `SELECT sequence,event_id,kind,value,occurred_at,observed_at,parent_occurrence_id,causal_depth FROM automation_product_facts WHERE source_id=? AND resource_json=? AND sequence>? ORDER BY sequence LIMIT ?`, source, resourceJSON, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.NormalizedFact
	for rows.Next() {
		var fact model.NormalizedFact
		var occurred, observed int64
		fact.Source, fact.Resource = source, resource
		if err = rows.Scan(&fact.Sequence, &fact.EventID, &fact.Kind, &fact.Value, &occurred, &observed, &fact.ParentOccurrenceID, &fact.CausalDepth); err != nil {
			return nil, err
		}
		fact.OccurredAt, fact.ObservedAt = fromNanos(occurred), fromNanos(observed)
		result = append(result, fact)
	}
	return result, rows.Err()
}

func insertAutomationFactTx(ctx context.Context, tx *sql.Tx, fact model.NormalizedFact) error {
	resource, _ := json.Marshal(fact.Resource)
	_, err := tx.ExecContext(ctx, `INSERT INTO automation_product_facts(source_id,event_id,kind,value,resource_json,occurred_at,observed_at,parent_occurrence_id,causal_depth) VALUES(?,?,?,?,?,?,?,?,?)`, fact.Source, fact.EventID, fact.Kind, fact.Value, resource, nanos(fact.OccurredAt), nanos(fact.ObservedAt), fact.ParentOccurrenceID, fact.CausalDepth)
	if err == nil {
		return nil
	}
	var stored model.NormalizedFact
	var storedResource []byte
	var occurred, observed int64
	readErr := tx.QueryRowContext(ctx, `SELECT kind,value,resource_json,occurred_at,observed_at,parent_occurrence_id,causal_depth FROM automation_product_facts WHERE source_id=? AND event_id=?`, fact.Source, fact.EventID).Scan(&stored.Kind, &stored.Value, &storedResource, &occurred, &observed, &stored.ParentOccurrenceID, &stored.CausalDepth)
	if readErr != nil {
		return classify(err)
	}
	stored.Source, stored.EventID = fact.Source, fact.EventID
	stored.OccurredAt, stored.ObservedAt = fromNanos(occurred), fromNanos(observed)
	if decodeErr := json.Unmarshal(storedResource, &stored.Resource); decodeErr != nil {
		return decodeErr
	}
	fact.Sequence, stored.Sequence = 0, 0
	if !reflect.DeepEqual(stored, fact) {
		return app.ErrConflict
	}
	return nil
}

func automationCausalityTx(ctx context.Context, tx *sql.Tx, principal model.Principal) (model.OccurrenceID, uint32, error) {
	if principal.AutomationRun == "" {
		return "", 0, nil
	}
	id := model.OccurrenceID(principal.AutomationRun)
	var depth uint32
	err := tx.QueryRowContext(ctx, `SELECT causal_depth FROM automation_occurrences WHERE id=?`, id).Scan(&depth)
	if errors.Is(err, sql.ErrNoRows) {
		// Some non-occurrence automation effects (notably synchronous native
		// guidance) historically use AutomationRun as a correlation key. They
		// are roots, not fabricated occurrence ancestry.
		return "", 0, nil
	}
	return id, depth + 1, err
}

func insertTerminalOperationFactsTx(ctx context.Context, tx *sql.Tx, in app.OperationCompletion) error {
	if in.OperationState != model.OperationSucceeded && in.OperationState != model.OperationFailed {
		return nil
	}
	op, err := operationTx(ctx, tx, in.OperationID)
	if err != nil {
		return err
	}
	parent, depth, err := automationCausalityTx(ctx, tx, op.Principal)
	if err != nil {
		return err
	}
	kind, value := model.FactOperationSucceeded, "succeeded"
	if in.OperationState == model.OperationFailed {
		kind, value = model.FactOperationFailed, "failed"
	}
	fact := model.NormalizedFact{Source: model.AutomationSourceApplication, EventID: "operation:" + string(op.ID) + ":" + string(in.OperationState), Kind: kind, Value: value, Resource: model.AutomationFactResource{Kind: model.FactResourceOperation, ID: string(op.ID)}, OccurredAt: in.At, ObservedAt: in.At, ParentOccurrenceID: parent, CausalDepth: depth}
	if err = insertAutomationFactTx(ctx, tx, fact); err != nil {
		return err
	}
	if op.Kind != model.OperationSendMessage {
		return nil
	}
	var messageID model.MessageID
	if err = tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE operation_id=?`, op.ID).Scan(&messageID); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	messageKind, messageValue := model.FactMessageDelivered, "delivered"
	if in.OperationState == model.OperationFailed {
		messageKind, messageValue = model.FactMessageDenied, "denied"
	}
	return insertAutomationFactTx(ctx, tx, model.NormalizedFact{Source: model.AutomationSourceApplication, EventID: "message:" + string(messageID) + ":" + messageValue, Kind: messageKind, Value: messageValue, Resource: model.AutomationFactResource{Kind: model.FactResourceMessage, ID: string(messageID)}, OccurredAt: in.At, ObservedAt: in.At, ParentOccurrenceID: parent, CausalDepth: depth})
}

func insertTerminalWorkFactTx(ctx context.Context, tx *sql.Tx, runID model.WorkRunID, state model.WorkRunState, at time.Time) error {
	var kind, value string
	switch state {
	case model.WorkRunSucceeded:
		kind, value = model.FactWorkSucceeded, "succeeded"
	case model.WorkRunFailed:
		kind, value = model.FactWorkFailed, "failed"
	case model.WorkRunCancelled:
		kind, value = model.FactWorkCancelled, "cancelled"
	default:
		return nil
	}
	var requesterJSON []byte
	if err := tx.QueryRowContext(ctx, `SELECT requester_json FROM work_runs WHERE id=?`, runID).Scan(&requesterJSON); err != nil {
		return classify(err)
	}
	var requester model.Principal
	if err := json.Unmarshal(requesterJSON, &requester); err != nil {
		return err
	}
	parent, depth, err := automationCausalityTx(ctx, tx, requester)
	if err != nil {
		return err
	}
	return insertAutomationFactTx(ctx, tx, model.NormalizedFact{Source: model.AutomationSourceApplication, EventID: "work:" + string(runID) + ":" + value, Kind: kind, Value: value, Resource: model.AutomationFactResource{Kind: model.FactResourceWork, ID: string(runID)}, OccurredAt: at, ObservedAt: at, ParentOccurrenceID: parent, CausalDepth: depth})
}
