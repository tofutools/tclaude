package agentd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
)

type fedModelAnswer struct {
	peer, request string
	payload       proto.ModelAnswerPayload
}
type fedModelState struct {
	waiters  map[string]chan fedModelAnswer
	incoming map[string]string
	rates    map[string][]time.Time
}

func (rt *fedRuntime) modelsLocked() *fedModelState {
	if rt.models == nil {
		rt.models = &fedModelState{waiters: map[string]chan fedModelAnswer{}, incoming: map[string]string{}, rates: map[string][]time.Time{}}
	}
	return rt.models
}
func (rt *fedRuntime) handleModelAnswer(peer *db.FederationPeer, env *proto.Envelope) {
	var p proto.ModelAnswerPayload
	if env.DecodePayload(&p) != nil || !proto.ValidStreamID(p.Stream) {
		return
	}
	rt.modelsMu.Lock()
	ch := rt.modelsLocked().waiters[p.Stream]
	rt.modelsMu.Unlock()
	if ch != nil {
		select {
		case ch <- fedModelAnswer{peer.InstanceID, env.InReplyTo, p}:
		default:
		}
	}
}
func (rt *fedRuntime) openModelStream(ctx context.Context, peer *db.FederationPeer, session, name string) (*routebroker.FlowStream, error) {
	kp, err := stream.NewKeyPair()
	if err != nil {
		return nil, err
	}
	p := proto.ModelOpenPayload{Version: 1, Stream: proto.NewEnvelopeID(), Proxy: name, Session: session, Key: kp.Pub}
	ch := make(chan fedModelAnswer, 1)
	rt.modelsMu.Lock()
	st := rt.modelsLocked()
	if len(st.waiters) >= 128 {
		rt.modelsMu.Unlock()
		return nil, errors.New("model stream limit")
	}
	st.waiters[p.Stream] = ch
	rt.modelsMu.Unlock()
	defer func() { rt.modelsMu.Lock(); delete(rt.modelsLocked().waiters, p.Stream); rt.modelsMu.Unlock() }()
	env, err := proto.NewEnvelope(rt.id, proto.KindModelOpen, proto.Endpoint{Name: rt.name}, proto.Endpoint{Instance: peer.InstanceID}, time.Minute, p)
	if err != nil {
		return nil, err
	}
	sealed, err := proto.Seal(rt.id, env, peer.PubKey)
	if err != nil {
		return nil, err
	}
	opening, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	result, err := rt.cl.Send(opening, peer.InstanceID, sealed)
	if err != nil {
		return nil, err
	}
	if result.Status != proto.SendDelivered {
		return nil, errors.New("model gateway peer is offline")
	}
	for {
		select {
		case <-rt.ctx.Done():
			return nil, rt.ctx.Err()
		case <-opening.Done():
			return nil, opening.Err()
		case a := <-ch:
			if a.peer != peer.InstanceID || a.request != env.ID {
				continue
			}
			if !a.payload.OK {
				return nil, errors.New("model gateway refused")
			}
			raw, err := rt.joinStream(opening, peer.InstanceID, p.Stream, kp, a.payload.Key, true)
			if err != nil {
				return nil, err
			}
			flow := routebroker.NewFlowStream(raw)
			// Runtime cancellation closes active data streams as well as pending opens.
			stop := context.AfterFunc(rt.ctx, func() { _ = flow.Close() })
			context.AfterFunc(ctx, func() { stop(); _ = flow.Close() })
			return flow, nil
		}
	}
}
func (rt *fedRuntime) acceptModelOpen(peer *db.FederationPeer, env *proto.Envelope) {
	var p proto.ModelOpenPayload
	if env.From.Agent != "" || env.To.Agent != "" || env.DecodePayload(&p) != nil || p.Version != 1 || !proto.ValidStreamID(p.Stream) || len(p.Key) != 32 || !validModelProxyName(p.Proxy) || p.Session == "" || len(p.Session) > 128 {
		return
	}
	if fresh, err := db.MarkFederationEnvelopeSeen(peer.InstanceID, "modelopen:"+env.ID, time.Now().Add(2*time.Minute)); err != nil || !fresh {
		return
	}
	answer := func(ok bool, key []byte, reason string) {
		rt.sendControl(peer.InstanceID, proto.KindModelAnswer, env.ID, proto.ModelAnswerPayload{Stream: p.Stream, OK: ok, Key: key, Reason: reason})
	}
	if !fedPeerModelAllows(peer.InstanceID, p.Proxy) {
		answer(false, nil, "models.proxy is not granted for this named gateway")
		return
	}
	instance, err := modelProxyPolicy(p.Proxy)
	if err != nil {
		answer(false, nil, err.Error())
		return
	}
	rt.modelsMu.Lock()
	st := rt.modelsLocked()
	count := 0
	for _, name := range st.incoming {
		if name == p.Proxy {
			count++
		}
	}
	if len(st.incoming) >= 128 || count >= instance.ModelPolicy.MaxConcurrent || st.incoming[p.Stream] != "" || !allowPerMinute(st.rates, peer.InstanceID+"/"+p.Proxy, instance.ModelPolicy.RequestsPerMinute) {
		rt.modelsMu.Unlock()
		answer(false, nil, "model gateway concurrency or request-rate limit")
		return
	}
	st.incoming[p.Stream] = p.Proxy
	rt.modelsMu.Unlock()
	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		defer func() { rt.modelsMu.Lock(); delete(rt.modelsLocked().incoming, p.Stream); rt.modelsMu.Unlock() }()
		kp, err := stream.NewKeyPair()
		if err != nil {
			answer(false, nil, "model stream unavailable")
			return
		}
		duration := instance.ModelPolicy.MaxDurationSeconds
		if duration == 0 {
			duration = 1800
		}
		ctx, cancel := context.WithTimeout(rt.ctx, time.Duration(duration)*time.Second)
		defer cancel()
		answer(true, kp.Pub, "")
		raw, err := rt.joinStream(ctx, peer.InstanceID, p.Stream, kp, p.Key, false)
		if err != nil {
			return
		}
		flow := routebroker.NewFlowStream(raw)
		defer func() { _ = flow.Close() }()
		stop := context.AfterFunc(ctx, func() { _ = flow.Close() })
		defer stop()
		done := make(chan struct{})
		defer close(done)
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-flow.Done():
					cancel()
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
					if !fedPeerModelAllows(peer.InstanceID, p.Proxy) {
						cancel()
						return
					}
					if _, err := modelProxyPolicy(p.Proxy); err != nil {
						cancel()
						return
					}
				}
			}
		}()
		req, err := readModelHTTPRequest(flow)
		if err != nil {
			return
		}
		defer req.Body.Close()
		req = req.WithContext(ctx)
		writer := newModelWireResponse(flow)
		serveModelUpstream(writer, req, peer.InstanceID, p.Session, p.Proxy, p.Stream)
		_ = writer.finish()
		_ = flow.CloseWrite()
	}()
}

// Read headers separately so an untrusted peer cannot allocate an unbounded
// HTTP header map. The body is bounded independently by the gateway policy.
func readModelHTTPHeader(r io.Reader) ([]byte, *bufio.Reader, error) {
	br := bufio.NewReaderSize(r, 4096)
	header := make([]byte, 0, 4096)
	for {
		line, err := br.ReadSlice('\n')
		if len(header)+len(line) > 32<<10 {
			return nil, nil, errors.New("model HTTP headers too large")
		}
		header = append(header, line...)
		if err != nil {
			return nil, nil, err
		}
		if len(line) == 2 && line[0] == '\r' {
			break
		}
	}
	return header, br, nil
}
func readModelHTTPRequest(r io.Reader) (*http.Request, error) {
	header, br, err := readModelHTTPHeader(r)
	if err != nil {
		return nil, err
	}
	return http.ReadRequest(bufio.NewReader(io.MultiReader(bytes.NewReader(header), br)))
}
