// Package hubupdate connects a serving hub child to its host-owned guardian.
// The external authorization path remains the signed hub-admin connection;
// this private Unix socket is accessible only to the hub service user.
package hubupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/tofutools/tclaude/pkg/selfupdate"
	"io"
	"net"
	"net/http"
	"time"
)

type Client struct {
	Socket string
	Token  string
}
type request struct {
	Actor     string          `json:"actor"`
	Operation string          `json:"operation"`
	Body      json.RawMessage `json:"body"`
}
type response struct {
	Status int             `json:"status"`
	Code   string          `json:"code,omitempty"`
	Error  string          `json:"error,omitempty"`
	Body   json.RawMessage `json:"body,omitempty"`
}
type Error struct {
	Status        int
	Code, Message string
}

func (e *Error) Error() string { return e.Message }
func (c Client) Call(ctx context.Context, actor, op string, payload any) (any, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(request{Actor: actor, Operation: op, Body: body})
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", c.Socket)
	}}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, "POST", "http://guardian/control", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Hub-Guardian-Token", c.Token)
	res, err := (&http.Client{Transport: transport, Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("hub guardian unavailable")
	}
	defer func() { _ = res.Body.Close() }()
	raw, err = io.ReadAll(io.LimitReader(res.Body, 256<<10+1))
	if err != nil || len(raw) > 256<<10 {
		return nil, fmt.Errorf("invalid guardian response")
	}
	var result response
	if json.Unmarshal(raw, &result) != nil {
		return nil, fmt.Errorf("invalid guardian response")
	}
	if result.Status != 200 {
		return nil, &Error{Status: result.Status, Code: result.Code, Message: result.Error}
	}
	var out any
	if json.Unmarshal(result.Body, &out) != nil {
		return nil, fmt.Errorf("invalid guardian body")
	}
	return out, nil
}
func Handler(service *selfupdate.Service, supervisor string, authorize func(string) bool, audits ...func(selfupdate.Job) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := func(status int, code, message string, body any) {
			raw, _ := json.Marshal(body)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response{Status: status, Code: code, Error: message, Body: raw})
		}
		if r.Method != "POST" || r.URL.Path != "/control" {
			reply(404, "operation", "unknown guardian operation", nil)
			return
		}
		var p request
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&p) != nil {
			reply(400, "request", "invalid guardian request", nil)
			return
		}
		if !authorize(p.Actor) {
			reply(403, "hub_update_required", "hub.update required", nil)
			return
		}
		audit := func(j *selfupdate.Job) error {
			if j != nil && j.State != "running" && j.State != "restarting" && len(audits) > 0 {
				return audits[0](*j)
			}
			return nil
		}
		if err := audit(service.Pending()); err != nil {
			reply(503, "audit", "hub update outcome audit unavailable", nil)
			return
		}
		switch p.Operation {
		case "update.status":
			reply(200, "", "", StatusJSON(service.Status(), supervisor))
		case "update.job":
			var body struct {
				JobID string `json:"job_id"`
			}
			if json.Unmarshal(p.Body, &body) != nil {
				reply(400, "job", "invalid job", nil)
				return
			}
			job, err := service.Job(body.JobID)
			if err != nil {
				reply(404, "job", "no such update job", nil)
				return
			}
			if err := audit(&job); err != nil {
				reply(503, "audit", "hub update outcome audit unavailable", nil)
				return
			}
			reply(200, "", "", JobJSON(job))
		case "update.start":
			var body selfupdate.Request
			d := json.NewDecoder(bytes.NewReader(p.Body))
			d.DisallowUnknownFields()
			if d.Decode(&body) != nil {
				reply(400, "request", "invalid update request", nil)
				return
			}
			if err := body.Validate(); err != nil {
				reply(400, "request", err.Error(), nil)
				return
			}
			if body.Action != "check" && supervisor == "" {
				reply(409, "not_supervised", "host must run serve --supervised under systemd or launchd with a restart policy", nil)
				return
			}
			job, err := service.Start(body, p.Actor, func() bool { return authorize(p.Actor) })
			if err != nil {
				if err == selfupdate.ErrBusy {
					reply(409, "update_busy", err.Error(), nil)
				} else {
					reply(400, "update", err.Error(), nil)
				}
				return
			}
			reply(200, "", "", JobJSON(job))
		default:
			reply(404, "operation", "unknown update operation", nil)
		}
	})
}
func JobJSON(j selfupdate.Job) map[string]any {
	raw, _ := json.Marshal(j)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	out["from_version"] = j.CurrentVersion
	if j.State == "succeeded" {
		out["state"] = "completed"
	}
	return out
}
func StatusJSON(s selfupdate.Status, supervisor string) map[string]any {
	raw, _ := json.Marshal(s)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	if supervisor == "" {
		out["supervisor"] = nil
		out["blocked"] = map[string]string{"code": "not_supervised", "message": "host must run serve --supervised under a verified systemd/launchd restart policy"}
	} else {
		out["supervisor"] = supervisor
	}
	if s.Job != nil {
		out["job"] = JobJSON(*s.Job)
	}
	return out
}
