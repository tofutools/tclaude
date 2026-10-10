package agentd

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/federation/client"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

type boardInviteToken struct {
	Hub    string `json:"hub"`
	Board  string `json:"board"`
	Secret string `json:"secret"`
	Key    string `json:"key"`
}

func registerBoardRoutes(mux *http.ServeMux, prefix string, dashboard bool) {
	registerBoardItemRoutes(mux, prefix, dashboard)
	for pattern, operation := range map[string]string{
		"GET ": "boards.list", "POST ": "boards.create", "POST /join": "join", "GET /{board}": "boards.get", "DELETE /{board}": "boards.delete",
		"DELETE /{board}/membership": "leave", "GET /{board}/members": "members.list", "PUT /{board}/members/{instance}": "members.set", "DELETE /{board}/members/{instance}": "members.remove",
		"GET /{board}/invites": "invites.list", "POST /{board}/invites": "invites.create", "DELETE /{board}/invites/{token_id}": "invites.revoke", "POST /{board}/rotate-key": "keys.rotate",
	} {
		method, tail, _ := strings.Cut(pattern, " ")
		handler := boardOperatorRoute(operation)
		if dashboard {
			handler = dashboardFederationRoute(handler)
		}
		mux.HandleFunc(method+" "+prefix+tail, handler)
	}
}
func boardOperatorRoute(operation string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireHuman(w, r, "manage content boards") {
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
		payload := map[string]any{}
		if r.Method == "POST" || r.Method == "PUT" {
			dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, proto.MaxAdminPayload))
			if err := dec.Decode(&payload); err != nil || payload == nil {
				writeError(w, 400, "invalid_arg", "expected one JSON object")
				return
			}
			var extra any
			if dec.Decode(&extra) != io.EOF {
				writeError(w, 400, "invalid_arg", "expected one JSON object")
				return
			}
		}
		for _, key := range []string{"board", "instance", "token_id"} {
			if v := r.PathValue(key); v != "" {
				payload[key] = v
			}
		}
		if v := r.URL.Query().Get("cursor"); v != "" {
			payload["cursor"] = v
		}
		access, err := newBoardItemAccess(r.Context())
		if err != nil {
			writeError(w, 409, "hub_required", err.Error())
			return
		}
		opts := access.opts
		identity := opts.Identity
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		call := func(op string, p any) (json.RawMessage, error) {
			result, e := client.BoardCall(ctx, opts, "", op, p)
			if e != nil {
				return nil, e
			}
			if result.Status >= 400 {
				return nil, &boardHTTPError{result.Status, result.Code, result.Error}
			}
			return result.Body, nil
		}
		var body json.RawMessage
		board, _ := payload["board"].(string)
		keyForBoard := func() (int64, []byte, error) {
			raw, e := call("boards.get", map[string]any{"board": board})
			if e != nil {
				return 0, nil, e
			}
			var state struct {
				Epoch int64 `json:"epoch"`
			}
			if e = json.Unmarshal(raw, &state); e != nil {
				return 0, nil, e
			}
			raw, e = call("keys.get", map[string]any{"board": board, "epoch": state.Epoch})
			if e != nil {
				return 0, nil, e
			}
			var keys struct {
				Keys map[string]*proto.Encrypted `json:"keys"`
			}
			if e = json.Unmarshal(raw, &keys); e != nil {
				return 0, nil, e
			}
			key, e := proto.OpenBoardKey(identity, board, state.Epoch, keys.Keys[strconv.FormatInt(state.Epoch, 10)])
			return state.Epoch, key, e
		}
		switch operation {
		case "boards.create":
			board = proto.NewEnvelopeID()
			key := make([]byte, 32)
			if _, err = rand.Read(key); err == nil {
				var box *proto.Encrypted
				box, err = proto.SealBoardKey(identity.Pub, board, 1, key)
				if err == nil {
					payload["board"] = board
					payload["envelopes"] = map[string]*proto.Encrypted{identity.ID(): box}
					payload["key_proofs"] = map[string][]byte{identity.ID(): proto.BoardKeyProof(key, board, 1, identity.ID())}
					body, err = call(operation, payload)
				}
			}
		case "join":
			token, _ := payload["token"].(string)
			var invitation boardInviteToken
			var raw []byte
			raw, err = base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, "board1_"))
			if err == nil {
				err = json.Unmarshal(raw, &invitation)
			}
			if err != nil || !proto.ValidStreamID(invitation.Board) || !proto.ValidStreamID(invitation.Secret) {
				writeError(w, 400, "board_invite", "invalid board invitation")
				return
			}
			if invitation.Hub != opts.URL {
				writeError(w, 409, "hub_mismatch", "invitation belongs to a different hub; configure that hub URL first")
				return
			}
			var result *proto.HubAdminResult
			result, err = client.BoardCall(ctx, opts, invitation.Secret, "keys.join", map[string]any{"board": invitation.Board, "token": invitation.Secret})
			if err == nil && result.Status >= 400 {
				err = &boardHTTPError{result.Status, result.Code, result.Error}
			}
			if err == nil {
				var reply struct {
					Epoch   int64  `json:"epoch"`
					Package string `json:"key_package"`
				}
				err = json.Unmarshal(result.Body, &reply)
				if err == nil {
					var wrapping, ciphertext, key []byte
					wrapping, err = hex.DecodeString(invitation.Key)
					if err == nil {
						ciphertext, err = base64.RawStdEncoding.DecodeString(reply.Package)
					}
					if err == nil {
						key, err = proto.OpenBoardContent(wrapping, invitation.Board, reply.Epoch, invitation.Secret, ciphertext)
					}
					if err == nil {
						var box *proto.Encrypted
						box, err = proto.SealBoardKey(identity.Pub, invitation.Board, reply.Epoch, key)
						if err == nil {
							_, err = call("keys.install", map[string]any{"board": invitation.Board, "epoch": reply.Epoch, "envelopes": map[string]*proto.Encrypted{identity.ID(): box}, "key_proofs": map[string][]byte{identity.ID(): proto.BoardKeyProof(key, invitation.Board, reply.Epoch, identity.ID())}})
						}
					}
				}
			}
			if err == nil {
				body, err = call("boards.get", map[string]any{"board": invitation.Board})
			}
		case "invites.create":
			var epoch int64
			var key []byte
			epoch, key, err = keyForBoard()
			if err == nil {
				wrapping := make([]byte, 32)
				_, err = rand.Read(wrapping)
				if err == nil {
					secret := proto.NewEnvelopeID()
					var cipher []byte
					cipher, err = proto.SealBoardContent(wrapping, board, epoch, secret, key)
					if err == nil {
						payload["epoch"] = epoch
						payload["token"] = secret
						payload["key_package"] = base64.RawStdEncoding.EncodeToString(cipher)
						if _, ok := payload["ttl_seconds"]; !ok {
							payload["ttl_seconds"] = 3600
						}
						body, err = call(operation, payload)
						if err == nil {
							var reply map[string]any
							err = json.Unmarshal(body, &reply)
							if err == nil {
								var raw []byte
								raw, err = json.Marshal(boardInviteToken{opts.URL, board, secret, hex.EncodeToString(wrapping)})
								if err == nil {
									reply["token"] = "board1_" + base64.RawURLEncoding.EncodeToString(raw)
									body, err = json.Marshal(reply)
								}
							}
						}
					}
				}
			}
		case "keys.rotate":
			var epoch int64
			var oldKey []byte
			epoch, oldKey, err = keyForBoard()
			if err == nil {
				var members []struct {
					Instance string `json:"instance"`
					Pub      []byte `json:"pubkey"`
					Proof    []byte `json:"key_proof"`
				}
				cursor := ""
				for {
					var raw json.RawMessage
					raw, err = call("members.list", map[string]any{"board": board, "cursor": cursor})
					if err != nil {
						break
					}
					var page struct {
						Members []struct {
							Instance string `json:"instance"`
							Pub      []byte `json:"pubkey"`
							Proof    []byte `json:"key_proof"`
						} `json:"members"`
						Cursor string `json:"next_cursor"`
					}
					err = json.Unmarshal(raw, &page)
					if err != nil {
						break
					}
					members = append(members, page.Members...)
					cursor = page.Cursor
					if cursor == "" {
						break
					}
				}
				if err == nil {
					key := make([]byte, 32)
					_, err = rand.Read(key)
					boxes := map[string]*proto.Encrypted{}
					proofs := map[string][]byte{}
					for _, member := range members {
						if err != nil {
							break
						}
						if !proto.VerifyBoardKeyProof(oldKey, board, epoch, member.Instance, member.Proof) {
							err = fmt.Errorf("board member has not proved possession of the current key; finish joining or remove that member")
							break
						}
						proofs[member.Instance] = proto.BoardKeyProof(key, board, epoch+1, member.Instance)
						pub := member.Pub
						if proto.InstanceID(pub) != member.Instance {
							err = fmt.Errorf("board member key does not match identity")
						}
						if err == nil {
							boxes[member.Instance], err = proto.SealBoardKey(pub, board, epoch+1, key)
						}
					}
					if err == nil {
						body, err = call(operation, map[string]any{"board": board, "epoch": epoch + 1, "envelopes": boxes, "key_proofs": proofs})
					}
				}
			}
		case "leave":
			payload["instance"] = identity.ID()
			body, err = call("members.remove", payload)
		default:
			body, err = call(operation, payload)
		}
		if err != nil {
			var refused *client.RefusedError
			if e, ok := err.(*boardHTTPError); ok {
				writeError(w, e.Status, e.Code, e.Message)
			} else if errors.As(err, &refused) && refused.Code == "board_invite" {
				// The hub refuses a bad invite in the board hello, before any RPC.
				writeError(w, 403, "board_invite", "board invitation refused (expired, used, cancelled, or the board changed)")
			} else {
				writeError(w, 502, "board_unavailable", fmt.Sprintf("board operation failed: %v", err))
			}
			return
		}
		var out any
		if err = json.Unmarshal(body, &out); err != nil {
			writeError(w, 502, "board_reply", "invalid board response")
			return
		}
		writeJSON(w, 200, out)
	}
}

type boardHTTPError struct {
	Status        int
	Code, Message string
}

func (e *boardHTTPError) Error() string { return e.Message }
