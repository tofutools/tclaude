package agentd

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

type messagePrincipalKind uint8

const (
	messagePrincipalAgent messagePrincipalKind = iota + 1
	messagePrincipalOperator
)

type messagePrincipal struct {
	kind messagePrincipalKind
	conv string
}

type messageTarget struct {
	selector   string
	agentID    string
	generation string
}

type messageContent struct {
	subject string
	body    string
	render  func(groupName string) (subject, body string)
}

type messageCauseKind uint8

const (
	messageCauseDirect messageCauseKind = iota + 1
	messageCauseTrigger
)

// messageCause is daemon-owned policy, not request-shaped input. In
// particular, clients cannot opt themselves out of regular-send capacity.
type messageCause struct {
	kind        messageCauseKind
	groupID     int64
	groupName   string
	firingID    int64
	actionIndex int
}

type messageAudience struct {
	agentID      string
	conversation string
	routeConv    string
	originalTo   string
	pinned       bool
	groupID      int64
	groupName    string
}

type messageNotificationState string

const messageNotificationArmed messageNotificationState = "armed"

type acceptedMessage struct {
	messageIDs       []int64
	resolvedAudience messageAudience
	notification     messageNotificationState
	pending          int
	duplicate        bool
	outcomeCommitted bool
}

type messageRefusal struct {
	code             string
	detail           string
	retryable        bool
	pending          int
	limit            int
	candidates       []*agent.Resolved
	outcomeCommitted bool
}

func (r *messageRefusal) Error() string { return r.detail }

func acceptMessage(principal messagePrincipal, target messageTarget, content messageContent, cause messageCause) (*acceptedMessage, *messageRefusal) {
	if cause.kind == messageCauseTrigger {
		prior, err := db.GetTriggerActionOutcome(cause.firingID, cause.actionIndex)
		if err != nil {
			return nil, &messageRefusal{code: "queue_failed", detail: err.Error()}
		}
		if prior != nil {
			if prior.ActionType != db.TriggerActionMessage {
				return nil, &messageRefusal{code: "queue_failed", detail: fmt.Sprintf("trigger action identity conflict at firing %d action %d: stored %q, requested %q", cause.firingID, cause.actionIndex, prior.ActionType, db.TriggerActionMessage), outcomeCommitted: true}
			}
			if prior.Outcome != "queued" || prior.MessageID == 0 {
				return nil, &messageRefusal{code: prior.Outcome, detail: prior.Detail, outcomeCommitted: true}
			}
			m, err := db.GetAgentMessage(prior.MessageID)
			if err != nil || m == nil {
				if err == nil {
					err = fmt.Errorf("accepted trigger message %d is missing", prior.MessageID)
				}
				return nil, &messageRefusal{code: "queue_failed", detail: err.Error(), outcomeCommitted: true}
			}
			audience := messageAudience{conversation: m.ToConv, routeConv: m.ToConv, originalTo: m.OriginalToConv, pinned: m.PinGen, groupID: m.GroupID}
			audience.agentID, _ = db.AgentIDForConv(m.ToConv)
			enqueueDeliveryForConv(m.ToConv)
			return &acceptedMessage{
				messageIDs: []int64{prior.MessageID}, resolvedAudience: audience,
				notification: messageNotificationArmed, duplicate: true, outcomeCommitted: true,
			}, nil
		}
	}
	audience, refusal := resolveMessageAudience(target)
	if refusal != nil {
		return nil, refusal
	}
	if principal.kind == messagePrincipalAgent {
		if strings.TrimSpace(principal.conv) == "" {
			return nil, &messageRefusal{code: "auth", detail: "message sender is unavailable"}
		}
		if principal.conv == audience.conversation {
			return nil, &messageRefusal{code: "invalid_arg", detail: "cannot message self"}
		}
		via, _, err := db.CanSenderReachTarget(principal.conv, audience.routeConv)
		if err != nil {
			return nil, &messageRefusal{code: "io", detail: err.Error()}
		}
		if via != nil {
			if via.IsArchived() {
				return nil, &messageRefusal{code: "archived", detail: errGroupArchived.Error()}
			}
			audience.groupID, audience.groupName = via.ID, via.Name
		} else if !holdsPermission(principal.conv, PermMessageDirect) {
			code := "auth"
			if cause.kind == messageCauseTrigger {
				code = "permission_denied"
			}
			return nil, &messageRefusal{code: code, detail: fmt.Sprintf("no shared group with %s and you do not own a group containing it; messaging an agent outside your group requires the %q permission (ask the human to grant it, or get a time-bounded grant via `tclaude agent sudo`)", short8(audience.conversation), PermMessageDirect)}
		} else {
			audience.groupID, audience.groupName = 0, ""
		}
	} else if principal.kind != messagePrincipalOperator {
		return nil, &messageRefusal{code: "auth", detail: "invalid message principal"}
	}

	// A trigger's explicitly selected group is attribution for an operator
	// action. Agent-owned triggers still use the live reachability route above.
	if principal.kind == messagePrincipalOperator && cause.groupID > 0 {
		audience.groupID, audience.groupName = cause.groupID, cause.groupName
	}
	if content.render != nil {
		content.subject, content.body = content.render(audience.groupName)
	}
	m := &db.AgentMessage{
		GroupID: audience.groupID, FromConv: principal.conv, ToConv: audience.conversation,
		OriginalToConv: audience.originalTo, Subject: content.subject, Body: content.body,
		ToRecipients: []string{audience.conversation}, PinGen: audience.pinned,
		OperatorAuthored: principal.kind == messagePrincipalOperator,
	}

	accepted := &acceptedMessage{resolvedAudience: audience}
	switch cause.kind {
	case messageCauseDirect:
		id, pending, err := db.InsertAgentMessageBounded(m, regularAgentMessageQueueLimit)
		if err != nil {
			var full *db.AgentMessageQueueFullError
			if errors.As(err, &full) {
				return nil, &messageRefusal{code: "queue_full", detail: queueFullHint(full.Pending, full.Limit), retryable: true, pending: full.Pending, limit: full.Limit}
			}
			return nil, &messageRefusal{code: "io", detail: err.Error()}
		}
		accepted.messageIDs, accepted.pending = []int64{id}, pending
	case messageCauseTrigger:
		desired := db.TriggerActionOutcome{
			FiringID: cause.firingID, ActionIndex: cause.actionIndex,
			ActionType: db.TriggerActionMessage, Outcome: "queued", CreatedAt: time.Now().UTC(),
		}
		stored, inserted, err := db.InsertTriggerMessageOutcome(m, desired)
		if err != nil {
			return nil, &messageRefusal{code: "queue_failed", detail: err.Error()}
		}
		accepted.outcomeCommitted = true
		accepted.duplicate = !inserted
		if stored.Outcome != "queued" || stored.MessageID == 0 {
			return nil, &messageRefusal{code: stored.Outcome, detail: stored.Detail, outcomeCommitted: true}
		}
		accepted.messageIDs = []int64{stored.MessageID}
	default:
		return nil, &messageRefusal{code: "invalid_arg", detail: "invalid message cause"}
	}

	// Both persistence variants commit before returning. Only now may the
	// best-effort delivery worker observe and nudge the accepted row.
	enqueueDeliveryForConv(audience.conversation)
	accepted.notification = messageNotificationArmed
	return accepted, nil
}

func resolveMessageAudience(target messageTarget) (messageAudience, *messageRefusal) {
	var audience messageAudience
	if target.agentID != "" {
		audience.agentID = strings.TrimSpace(target.agentID)
		conv, err := db.CurrentConvForAgent(audience.agentID)
		if err != nil || conv == "" {
			return audience, &messageRefusal{code: "target_invalid", detail: "selected agent has no current conversation"}
		}
		audience.conversation = conv
		audience.routeConv = conv
		return audience, nil
	}
	resolved, matches, err := agent.ResolveSelector(target.selector)
	if errors.Is(err, agent.ErrAmbiguous) {
		return audience, &messageRefusal{code: "ambiguous", detail: "target matches multiple conversations", candidates: matches}
	}
	if err != nil {
		return audience, &messageRefusal{code: "not_found", detail: err.Error()}
	}
	audience.agentID = resolved.AgentID
	head := resolved.ConvID
	audience.conversation = head
	audience.routeConv = head
	raw := strings.TrimSpace(target.selector)
	if raw != "" && raw != head && db.ResolveLatestConv(raw) == head {
		audience.originalTo = raw
	}
	if gen := strings.TrimSpace(target.generation); gen != "" {
		targetAgent := resolved.AgentID
		if targetAgent == "" {
			targetAgent, _ = db.AgentIDForConv(head)
		}
		genAgent, _ := db.AgentIDForConv(gen)
		if targetAgent == "" || genAgent == "" || genAgent != targetAgent {
			return audience, &messageRefusal{code: "invalid_arg", detail: fmt.Sprintf("gen %q is not a generation of the target agent", gen)}
		}
		audience.conversation, audience.originalTo, audience.pinned = gen, "", true
	}
	return audience, nil
}
