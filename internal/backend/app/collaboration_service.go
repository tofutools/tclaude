package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tofutools/tclaude/internal/backend/model"
)

const (
	maxMessageSubjectBytes    = 512
	maxMessageBodyBytes       = 1 << 20
	maxMessageRecipients      = 128
	maxMessageAttachments     = 4
	maxAttachmentBytes        = 5 << 20
	maxMessageAttachmentBytes = 10 << 20
	attachmentClaimLifetime   = 30 * time.Minute
)

func validateAgentMetadata(taskReference string, preferences model.AgentNotificationPreferences) error {
	if len(taskReference) > 2048 || !utf8.ValidString(taskReference) {
		return fail(ErrInvalid, "task reference must be valid UTF-8 and at most 2048 bytes")
	}
	switch preferences.DirectMessage {
	case "", model.NotificationNone, model.NotificationIfAvailable:
		return nil
	default:
		return fail(ErrInvalid, "invalid direct-message notification preference")
	}
}

func (s *Service) RetireAgent(ctx context.Context, req RetireAgentRequest) (AgentResult, error) {
	if err := requireOperator(req.Context); err != nil {
		return AgentResult{}, err
	}
	if req.ExpectedRevision == 0 {
		return AgentResult{}, fail(ErrInvalid, "expected revision is required")
	}
	if strings.TrimSpace(req.Reason) == "" || len(req.Reason) > 1024 {
		return AgentResult{}, fail(ErrInvalid, "bounded retirement reason is required")
	}
	agent, err := s.store.RetireAgent(ctx, req.ID, req.ExpectedRevision, req.Context, req.Reason, s.now().UTC())
	return AgentResult{Agent: agent}, err
}

func (s *Service) ReactivateAgent(ctx context.Context, req ReactivateAgentRequest) (AgentResult, error) {
	if err := requireOperator(req.Context); err != nil {
		return AgentResult{}, err
	}
	if req.ExpectedRevision == 0 {
		return AgentResult{}, fail(ErrInvalid, "expected revision is required")
	}
	agent, err := s.store.ReactivateAgent(ctx, req.ID, req.ExpectedRevision, req.Context, s.now().UTC())
	return AgentResult{Agent: agent}, err
}

func (s *Service) CreateAttachmentClaim(ctx context.Context, req CreateAttachmentClaimRequest) (AttachmentClaimResult, error) {
	if err := s.validateMessageSender(ctx, req.Principal); err != nil {
		return AttachmentClaimResult{}, err
	}
	attachment, err := s.prepareAttachment(req.Filename, req.MediaType, req.Content)
	if err != nil {
		return AttachmentClaimResult{}, err
	}
	now := s.now().UTC()
	if _, err := s.store.PurgeExpiredAttachmentClaims(ctx, now); err != nil {
		return AttachmentClaimResult{}, err
	}
	attachment.ID = model.AttachmentID(s.newID("att_"))
	claim := model.AttachmentClaim{ID: model.AttachmentClaimID(s.newID("acl_")), Attachment: attachment, Owner: req.Principal, CreatedAt: now, ExpiresAt: now.Add(attachmentClaimLifetime)}
	claim.Attachment.CreatedAt = now
	if err := s.store.CreateAttachmentClaim(ctx, claim, append([]byte(nil), req.Content...)); err != nil {
		return AttachmentClaimResult{}, err
	}
	return AttachmentClaimResult{Claim: claim}, nil
}

func (s *Service) SendMessage(ctx context.Context, req SendMessageRequest) (MessageResult, error) {
	if err := validateEffectContext(req.RequestContext); err != nil {
		return MessageResult{}, err
	}
	if len(req.Subject) == 0 {
		req.Subject = "Message"
	}
	if len(req.To.AgentIDs) == 0 && req.To.GroupID == "" && req.To.RoleID == "" && !req.To.Operator && len(req.RecipientAgentIDs) != 0 {
		req.To.AgentIDs = append([]model.AgentID(nil), req.RecipientAgentIDs...)
	}
	requestDigest, err := authoredMessageRequestDigest(req)
	if err != nil {
		return MessageResult{}, err
	}
	if repeated, ok, err := s.store.MessageByRequest(ctx, req.Principal, req.RequestID, requestDigest); err != nil {
		return MessageResult{}, err
	} else if ok {
		return MessageResult{Message: repeated.Message}, nil
	}
	if err := s.validateMessageSender(ctx, req.Principal); err != nil {
		return MessageResult{}, err
	}
	if len(req.Subject) > maxMessageSubjectBytes || !utf8.ValidString(req.Subject) {
		return MessageResult{}, fail(ErrInvalid, "message subject must be valid UTF-8 and at most %d bytes", maxMessageSubjectBytes)
	}
	if len(req.Body) > maxMessageBodyBytes || !utf8.ValidString(req.Body) {
		return MessageResult{}, fail(ErrInvalid, "message body must be valid UTF-8 and at most %d bytes", maxMessageBodyBytes)
	}
	if len(req.Attachments) > maxMessageAttachments {
		return MessageResult{}, fail(ErrInvalid, "message has more than %d attachments", maxMessageAttachments)
	}

	recipients, authority, err := s.resolveRecipients(ctx, req.Principal, req.To, req.CC)
	if err != nil {
		return MessageResult{}, err
	}
	if len(recipients) == 0 {
		return MessageResult{}, fail(ErrInvalid, "at least one concrete recipient is required")
	}
	if len(recipients) > maxMessageRecipients {
		return MessageResult{}, fail(ErrInvalid, "message resolves to more than %d recipients", maxMessageRecipients)
	}

	prepared := make([]MessageAttachmentAdmission, 0, len(req.Attachments))
	var total int64
	for _, input := range req.Attachments {
		if input.Claim != nil {
			if len(input.Content) != 0 || input.Filename != "" || input.MediaType != "" {
				return MessageResult{}, fail(ErrInvalid, "attachment must contain bytes or one claim, not both")
			}
			ref := input.Claim
			if err := validateAttachmentDescriptor(ref.Filename, ref.MediaType, ref.Size, ref.SHA256); err != nil || ref.ClaimID.Validate() != nil || ref.AttachmentID.Validate() != nil {
				return MessageResult{}, fail(ErrInvalid, "invalid attachment claim reference")
			}
			prepared = append(prepared, MessageAttachmentAdmission{Attachment: model.Attachment{ID: ref.AttachmentID, Filename: ref.Filename, MediaType: ref.MediaType, Size: ref.Size, SHA256: ref.SHA256}, ClaimID: ref.ClaimID})
			total += ref.Size
			continue
		}
		attachment, err := s.prepareAttachment(input.Filename, input.MediaType, input.Content)
		if err != nil {
			return MessageResult{}, err
		}
		prepared = append(prepared, MessageAttachmentAdmission{Attachment: attachment, Content: append([]byte(nil), input.Content...)})
		total += attachment.Size
	}
	if total > maxMessageAttachmentBytes {
		return MessageResult{}, fail(ErrInvalid, "message attachment content exceeds %d bytes", maxMessageAttachmentBytes)
	}
	if strings.TrimSpace(req.Body) == "" && len(prepared) == 0 {
		return MessageResult{}, fail(ErrInvalid, "message body or attachment is required")
	}

	now := s.now().UTC()
	var messageConversation model.ConversationID
	if req.Principal.Kind == model.PrincipalExecution {
		execution, err := s.store.Execution(ctx, req.Principal.ExecutionID)
		if err != nil {
			return MessageResult{}, err
		}
		messageConversation = execution.ConversationID
	} else {
		agentID := req.Principal.AgentID
		if req.Principal.Kind == model.PrincipalAutomation && req.Principal.Authority.Kind == model.AuthorityAgent {
			agentID = req.Principal.Authority.AgentID
		}
		if agentID != "" {
			if association, err := s.store.CurrentConversation(ctx, agentID); err == nil {
				messageConversation = association.ConversationID
			}
		}
	}
	for i := range prepared {
		if prepared[i].Attachment.ID == "" {
			prepared[i].Attachment.ID = model.AttachmentID(s.newID("att_"))
		}
		prepared[i].Attachment.CreatedAt = now
	}
	message := model.Message{ID: model.MessageID(s.newID("msg_")), Sender: req.Principal, SenderConversationID: messageConversation, Subject: req.Subject, Body: req.Body, ParentMessageID: req.ParentMessageID, Recipients: recipients, CreatedAt: now}
	for _, item := range prepared {
		message.Attachments = append(message.Attachments, item.Attachment)
	}
	result, err := s.store.CreateMessage(ctx, MessageAdmission{Message: message, RequestID: req.RequestID, OperationID: model.OperationID(s.newID("op_")), RequestDigest: requestDigest, Authority: authority, Attachments: prepared})
	if err != nil {
		return MessageResult{}, err
	}
	return MessageResult{Message: result.Message}, nil
}

func (s *Service) MarkMessageRead(ctx context.Context, req MarkMessageReadRequest) (MessageResult, error) {
	addressKind, agentID := model.MessageAddressAgent, req.AgentID
	if req.Principal.Kind == model.PrincipalOperator {
		if req.Operator {
			addressKind, agentID = model.MessageAddressOperator, ""
		}
	} else {
		agentID = req.Principal.AgentID
		if req.Principal.Kind == model.PrincipalAutomation && req.Principal.Authority.Kind == model.AuthorityAgent {
			agentID = req.Principal.Authority.AgentID
		}
		if agentID == "" {
			return MessageResult{}, fail(ErrUnsupported, "standalone execution has no agent inbox")
		}
	}
	resource := model.ResourceSelector{Kind: model.ResourceAgent, AgentID: agentID}
	if addressKind == model.MessageAddressOperator {
		resource = model.ResourceSelector{Kind: model.ResourceOperator}
	}
	authority := model.AuthorityRequest{Principal: req.Principal, Action: model.ActionMarkInboxRead, Resource: resource}
	message, err := s.store.MarkMessageRead(ctx, req.MessageID, addressKind, agentID, authority, s.now().UTC())
	if req.Principal.Kind != model.PrincipalOperator {
		message = messageForRecipient(message, agentID)
	}
	return MessageResult{Message: message}, err
}

func (s *Service) ReadAttachment(ctx context.Context, req ReadAttachmentRequest) (AttachmentContentResult, error) {
	attachment, content, err := s.store.AttachmentContent(ctx, req.AttachmentID, req.Principal, s.now().UTC())
	return AttachmentContentResult{Attachment: attachment, Content: content}, err
}

func (s *Service) RecordMessageNotification(ctx context.Context, update MessageNotificationUpdate) (MessageResult, error) {
	switch update.Outcome {
	case model.NotificationDelivered, model.NotificationUnavailable, model.NotificationFailed:
	default:
		return MessageResult{}, fail(ErrInvalid, "notification completion outcome is required")
	}
	if len(update.Detail) > 1024 || !utf8.ValidString(update.Detail) {
		return MessageResult{}, fail(ErrInvalid, "notification detail must be valid UTF-8 and at most 1024 bytes")
	}
	if update.At.IsZero() {
		update.At = s.now().UTC()
	}
	message, err := s.store.RecordMessageNotification(ctx, update.RecipientID, update.Outcome, update.Detail, update.At.UTC())
	return MessageResult{Message: message}, err
}

func (s *Service) validateMessageSender(ctx context.Context, principal model.Principal) error {
	switch principal.Kind {
	case model.PrincipalOperator:
		return nil
	case model.PrincipalAgent:
		agent, err := s.store.Agent(ctx, principal.AgentID)
		if err != nil || agent.Lifecycle != model.AgentActive {
			return fail(ErrUnauthorized, "sender is not an active admitted agent")
		}
		return nil
	case model.PrincipalExecution:
		if principal.AgentID == "" {
			return fail(ErrUnsupported, "standalone execution has no agent sender")
		}
		return s.requireAuthority(ctx, model.AuthorityRequest{Principal: principal, Action: model.ActionReadIdentity, Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: principal.AgentID}}, s.now().UTC())
	case model.PrincipalAutomation:
		if principal.Authority.Kind == model.AuthorityAgent {
			agent, err := s.store.Agent(ctx, principal.Authority.AgentID)
			if err != nil || agent.Lifecycle != model.AgentActive {
				return fail(ErrUnauthorized, "automation sender agent is not active")
			}
		}
		return nil
	default:
		return fail(ErrUnauthorized, "unsupported principal")
	}
}

func (s *Service) resolveRecipients(ctx context.Context, sender model.Principal, to, cc model.MessageAudience) ([]model.MessageRecipient, []model.AuthorityRequest, error) {
	targets := make(map[string]model.MessageRecipient)
	authority := make(map[string]model.AuthorityRequest)
	for _, authored := range []struct {
		audience model.MessageAudience
		kind     model.MessageAudienceKind
	}{{to, model.MessageAudienceTo}, {cc, model.MessageAudienceCC}} {
		for _, direct := range authored.audience.AgentIDs {
			agent, err := s.store.Agent(ctx, direct)
			if err != nil || agent.Lifecycle != model.AgentActive {
				return nil, nil, fail(ErrInvalid, "recipient agent %s is not active", direct)
			}
		}
		ids, err := s.store.ResolveMessageAudience(ctx, authored.audience)
		if err != nil {
			return nil, nil, err
		}
		for _, id := range ids {
			key := "agent:" + string(id)
			if existing, ok := targets[key]; ok && existing.Audience == model.MessageAudienceTo {
				continue
			}
			intent := model.NotificationNone
			if agent, err := s.store.Agent(ctx, id); err == nil {
				intent = agent.Notifications.DirectMessage
			}
			outcome := model.NotificationNotRequested
			if intent == model.NotificationIfAvailable {
				outcome = model.NotificationPending
			}
			targets[key] = model.MessageRecipient{ID: model.RecipientID(s.newID("rcp_")), AddressKind: model.MessageAddressAgent, AgentID: id, Audience: authored.kind, NotificationIntent: intent, NotificationOutcome: outcome}
			authority[key] = model.AuthorityRequest{Principal: sender, Action: model.ActionSendMessage, Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: id}}
		}
		if authored.audience.Operator {
			key := "operator"
			if existing, ok := targets[key]; !ok || existing.Audience != model.MessageAudienceTo {
				targets[key] = model.MessageRecipient{ID: model.RecipientID(s.newID("rcp_")), AddressKind: model.MessageAddressOperator, Audience: authored.kind, NotificationIntent: model.NotificationIfAvailable, NotificationOutcome: model.NotificationPending}
				authority[key] = model.AuthorityRequest{Principal: sender, Action: model.ActionSendMessage, Resource: model.ResourceSelector{Kind: model.ResourceOperator}}
			}
		}
	}
	keys := make([]string, 0, len(targets))
	for key := range targets {
		keys = append(keys, key)
	}
	slicesSort(keys)
	recipients := make([]model.MessageRecipient, 0, len(keys))
	requests := make([]model.AuthorityRequest, 0, len(keys))
	for _, key := range keys {
		recipients = append(recipients, targets[key])
		requests = append(requests, authority[key])
	}
	return recipients, requests, nil
}

func (s *Service) prepareAttachment(filename, mediaType string, content []byte) (model.Attachment, error) {
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	digest := sha256.Sum256(content)
	attachment := model.Attachment{Filename: filename, MediaType: mediaType, Size: int64(len(content)), SHA256: hex.EncodeToString(digest[:])}
	if err := validateAttachmentDescriptor(filename, mediaType, attachment.Size, attachment.SHA256); err != nil {
		return model.Attachment{}, err
	}
	return attachment, nil
}

func validateAttachmentDescriptor(filename, mediaType string, size int64, digest string) error {
	if strings.TrimSpace(filename) == "" || len(filename) > 255 || !utf8.ValidString(filename) || strings.ContainsAny(filename, "/\\\x00\r\n") {
		return fail(ErrInvalid, "attachment filename must be a plain valid UTF-8 name of at most 255 bytes")
	}
	if strings.TrimSpace(mediaType) == "" || len(mediaType) > 127 || !utf8.ValidString(mediaType) || strings.ContainsAny(mediaType, "\x00\r\n") {
		return fail(ErrInvalid, "attachment media type is invalid")
	}
	if size < 0 || size > maxAttachmentBytes {
		return fail(ErrInvalid, "attachment size exceeds %d bytes", maxAttachmentBytes)
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size {
		return fail(ErrInvalid, "attachment SHA-256 is invalid")
	}
	return nil
}

func authoredMessageRequestDigest(req SendMessageRequest) (string, error) {
	type digestAttachment struct {
		Filename  string
		MediaType string
		Size      int64
		SHA256    string
	}
	payload := struct {
		SenderScope string
		Subject     string
		Body        string
		Parent      model.MessageID
		To          model.MessageAudience
		CC          model.MessageAudience
		Attachments []digestAttachment
	}{SenderScope: requestScopeForDigest(req.Principal), Subject: req.Subject, Body: req.Body, Parent: req.ParentMessageID, To: req.To, CC: req.CC}
	payload.To.AgentIDs = append([]model.AgentID(nil), req.To.AgentIDs...)
	payload.CC.AgentIDs = append([]model.AgentID(nil), req.CC.AgentIDs...)
	sortAgentIDs(payload.To.AgentIDs)
	sortAgentIDs(payload.CC.AgentIDs)
	for _, input := range req.Attachments {
		if input.Claim != nil {
			payload.Attachments = append(payload.Attachments, digestAttachment{input.Claim.Filename, input.Claim.MediaType, input.Claim.Size, input.Claim.SHA256})
			continue
		}
		mediaType := input.MediaType
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		digest := sha256.Sum256(input.Content)
		payload.Attachments = append(payload.Attachments, digestAttachment{input.Filename, mediaType, int64(len(input.Content)), hex.EncodeToString(digest[:])})
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func sortAgentIDs(values []model.AgentID) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func requestScopeForDigest(principal model.Principal) string {
	switch principal.Kind {
	case model.PrincipalExecution:
		return "execution:" + string(principal.ExecutionID)
	case model.PrincipalAutomation:
		return "automation:" + principal.AutomationRun
	case model.PrincipalAgent:
		return "agent:" + string(principal.AgentID)
	default:
		return "operator"
	}
}

func slicesSort(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
