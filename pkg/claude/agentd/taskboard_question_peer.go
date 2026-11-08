package agentd

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Questions across nodes (tk_976fe76c): an agent asks a linked node's
// operator (`taskboard ask-operator --peer`), and the answer comes back to
// it as federation mail.
//
//   - The asking agent's node records the question when it proxies a
//     successful create (recordPeerQuestion): (peer, task, agent), bounded
//     by db.FederationPeerQuestionTTL.
//   - The answering node knows the asker by stable ids (the question's
//     AskedVia) and, on answer, queues ordinary mail from its operator to
//     that agent, tagged with the task id (mailTaskboardAnswerToPeer).
//   - The asker's node lets that mail in only if it matches a recorded
//     question from that peer for that agent, once
//     (taskboardAnswerMailAuthorized). Any other mail is refused as before:
//     the record is a reply-to in spirit, and widens nothing.

// parsePeerAgentPrincipal splits "peer:<instance>/agent:<id>"
// (peerPrincipal); not ok for a peer's operator or a local actor.
func parsePeerAgentPrincipal(p string) (instance, agentID string, ok bool) {
	rest, ok := strings.CutPrefix(p, "peer:")
	if !ok {
		return "", "", false
	}
	instance, agentID, ok = strings.Cut(rest, "/agent:")
	if !ok || instance == "" || strings.Contains(instance, "/") || !proto.ValidAgentRef(agentID) {
		return "", "", false
	}
	return instance, agentID, true
}

// taskboardPeerAskerPrincipal is a linked node's asking agent by stable ids,
// "" for any other caller.
func taskboardPeerAskerPrincipal(r *http.Request) string {
	p := taskboardCommentAuthor(r)
	if _, _, ok := parsePeerAgentPrincipal(p); ok {
		return p
	}
	return ""
}

// mailTaskboardAnswerToPeer queues the answer as federation mail to the
// linked node's agent that asked. Delivery is the outbox's: retried until
// acknowledged, and refused there unless that node recorded the question.
func mailTaskboardAnswerToPeer(t *db.GroupTask, q *db.GroupTaskQuestion, subject, body string) {
	instance, agentID, ok := parsePeerAgentPrincipal(q.AskedVia)
	if !ok {
		return
	}
	peer, err := db.GetFederationPeer(instance)
	if err != nil || peer == nil {
		slog.Info("taskboard: the asking node is not a trusted peer; answer not mailed", "task", t.ID, "peer", instance)
		return
	}
	if _, err := queueFederatedEnvelope(fedOutgoing{
		peer: peer, kind: proto.KindMail, toAgent: agentID, toLabel: agentID + "@" + peerDisplay(peer),
		subject: subject, preview: body, ttl: fedMailTTL,
		payload: proto.MailPayload{Subject: subject, Body: body, TaskboardAnswer: t.ID},
	}); err != nil {
		slog.Warn("taskboard: could not queue the answer for a peer's agent", "task", t.ID, "peer", instance, "error", err)
	}
}

// taskboardAnswerMailAuthorized lets in mail that answers a question this
// node's agent asked peer's operator: the mail names the task, and a live
// record matches (peer, task, recipient). The match is consumed.
func taskboardAnswerMailAuthorized(peer string, env *proto.Envelope, mp proto.MailPayload) bool {
	if mp.TaskboardAnswer == "" || env.Kind != proto.KindMail || !proto.ValidAgentRef(env.To.Agent) {
		return false
	}
	ok, err := db.ConsumeFederationPeerQuestion(peer, mp.TaskboardAnswer, env.To.Agent, time.Now())
	if err != nil {
		slog.Warn("federation: reading recorded peer questions failed", "error", err)
		return false
	}
	return ok
}

// peerQuestionCreateRoute is the agent peer-view route that creates a task
// on a linked node's board.
const peerQuestionCreateRoute = "POST /v1/groups/{name}/taskboard/tasks"

// peerCreateAsksQuestion reports whether a task-create body (JSON, or the
// multipart upload with a "task" part) carries a question.
func peerCreateAsksQuestion(contentType string, body []byte) bool {
	task := body
	if mt, params, err := mime.ParseMediaType(contentType); err == nil && strings.HasPrefix(mt, "multipart/") {
		task = nil
		mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				return false
			}
			if part.FormName() == "task" {
				task, _ = io.ReadAll(io.LimitReader(part, 1<<20))
				break
			}
		}
	}
	var in struct {
		Question json.RawMessage `json:"question"`
	}
	return json.Unmarshal(task, &in) == nil && len(in.Question) > 0 && string(in.Question) != "null"
}

// peerQuestionCapture keeps a copy of the proxied create's answer, to read
// the new task's id.
type peerQuestionCapture struct {
	http.ResponseWriter
	body bytes.Buffer
}

func (c *peerQuestionCapture) Write(b []byte) (int, error) {
	if c.body.Len() < 1<<20 {
		c.body.Write(b)
	}
	return c.ResponseWriter.Write(b)
}

// recordPeerQuestion records the question a successful create on peer
// asked for agentID.
func recordPeerQuestion(peer, agentID string, resp []byte) {
	var out struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(resp, &out) != nil || !strings.HasPrefix(out.ID, "tk_") || len(out.ID) > 64 {
		return
	}
	now := time.Now()
	if err := db.RecordFederationPeerQuestion(peer, out.ID, agentID, now, now.Add(db.FederationPeerQuestionTTL)); err != nil {
		slog.Warn("federation: could not record a question asked on a peer", "peer", peer, "task", out.ID, "error", err)
	}
}
