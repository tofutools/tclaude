package testharness

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// OpenCodeHistoryCLI simulates only the supported native CLI subprocess
// boundary. Fixtures use the same info/messages/parts projection as OpenCode.
// A helper process calls this and exits before the Go test runner prints PASS.
func OpenCodeHistoryCLI(args []string) error {
	root := filepath.Join(os.Getenv("HOME"), "opencode-history-sim")
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	path := func(id string) string { return filepath.Join(root, id+".json") }
	if len(args) < 2 {
		return fmt.Errorf("missing native command")
	}
	switch args[0] {
	case "serve":
		return serveOpenCodeHistory(root, args[1:])
	case "export":
		raw, err := os.ReadFile(path(args[1]))
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(raw)
		return err
	case "import":
		raw, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		var data struct {
			Info struct {
				ID        string `json:"id"`
				Directory string `json:"directory"`
			} `json:"info"`
		}
		if err = json.Unmarshal(raw, &data); err != nil {
			return err
		}
		if data.Info.ID == "" {
			return fmt.Errorf("missing session id")
		}
		return os.WriteFile(path(data.Info.ID), raw, 0600)
	case "session":
		if args[1] == "delete" && len(args) > 2 {
			return os.Remove(path(args[2]))
		}
		if args[1] != "list" {
			return fmt.Errorf("unsupported session command")
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			return err
		}
		list := []map[string]any{}
		for _, entry := range entries {
			raw, err := os.ReadFile(filepath.Join(root, entry.Name()))
			if err != nil {
				return err
			}
			var data struct {
				Info struct {
					ID, Title, Directory string
					Time                 struct{ Created, Updated int64 }
				}
			}
			if err = json.Unmarshal(raw, &data); err != nil {
				return err
			}
			list = append(list, map[string]any{"id": data.Info.ID, "title": data.Info.Title, "directory": data.Info.Directory, "projectId": "global", "created": data.Info.Time.Created, "updated": data.Info.Time.Updated})
		}
		return json.NewEncoder(os.Stdout).Encode(list)
	}
	return fmt.Errorf("unsupported native command")
}

func AppendOpenCodeHistory(home, id, cwd, text, assistant string) error {
	root := filepath.Join(home, "opencode-history-sim")
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	path := filepath.Join(root, id+".json")
	var data struct {
		Info     map[string]any   `json:"info"`
		Messages []map[string]any `json:"messages"`
	}
	raw, err := os.ReadFile(path)
	if err == nil {
		if err = json.Unmarshal(raw, &data); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	now := time.Now().UnixMilli()
	if data.Info == nil {
		data.Info = map[string]any{"id": id, "slug": "history", "projectID": "global", "directory": cwd, "title": "traveller", "version": "1.18.35", "time": map[string]any{"created": now, "updated": now}}
	}
	mid := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	pid := fmt.Sprintf("prt_%d", time.Now().UnixNano())
	data.Messages = append(data.Messages, map[string]any{"info": map[string]any{"id": mid, "sessionID": id, "role": "user", "time": map[string]any{"created": now}, "agent": "build", "model": map[string]string{"providerID": "test", "modelID": "fixture"}}, "parts": []map[string]any{{"id": pid, "sessionID": id, "messageID": mid, "type": "text", "text": text}}})
	if assistant != "" {
		aid := fmt.Sprintf("msg_%d", time.Now().UnixNano())
		ap := fmt.Sprintf("prt_%d", time.Now().UnixNano())
		data.Messages = append(data.Messages, map[string]any{"info": map[string]any{"id": aid, "sessionID": id, "role": "assistant", "parentID": mid, "time": map[string]any{"created": now, "completed": now}, "path": map[string]string{"cwd": cwd, "root": cwd}, "modelID": "fixture", "providerID": "test", "mode": "build", "agent": "build", "cost": 0, "tokens": map[string]any{"input": 1, "output": 1, "reasoning": 0, "cache": map[string]int{"read": 0, "write": 0}}}, "parts": []map[string]any{{"id": ap, "sessionID": id, "messageID": aid, "type": "text", "text": assistant}}})
	}
	raw, err = json.Marshal(data)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0600)
}

// Only the external managed-server subprocess is simulated. Native storage,
// session permission reapplication and daemon federation handlers remain real.
func serveOpenCodeHistory(root string, args []string) error {
	port := "0"
	host := "127.0.0.1"
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--port" {
			port = args[i+1]
		}
		if args[i] == "--hostname" {
			host = args[i+1]
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/global/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "1.18.35"})
	})
	mux.HandleFunc("/event", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	})
	mux.HandleFunc("/session/status", func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(map[string]any{}) })
	mux.HandleFunc("/session/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/session/")
		id, action, _ := strings.Cut(rest, "/")
		path := filepath.Join(root, id+".json")
		raw, err := os.ReadFile(path)
		if err != nil {
			http.Error(w, "unknown session", 404)
			return
		}
		var data struct {
			Info     map[string]any   `json:"info"`
			Messages []map[string]any `json:"messages"`
		}
		if err = json.Unmarshal(raw, &data); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if r.Method == "PATCH" {
			var patch map[string]any
			if err = json.NewDecoder(r.Body).Decode(&patch); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			for key, val := range patch {
				data.Info[key] = val
			}
			raw, _ = json.Marshal(data)
			_ = os.WriteFile(path, raw, 0600)
		}
		if action == "message" || action == "prompt_async" {
			if r.Method == "GET" {
				_ = json.NewEncoder(w).Encode(data.Messages)
				return
			}
			w.WriteHeader(204)
			return
		}
		if action == "abort" {
			_ = json.NewEncoder(w).Encode(true)
			return
		}
		_ = json.NewEncoder(w).Encode(data.Info)
	})
	mux.HandleFunc("/tui/publish", func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(true) })
	listener, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return err
	}
	defer listener.Close()
	return http.Serve(listener, mux)
}
