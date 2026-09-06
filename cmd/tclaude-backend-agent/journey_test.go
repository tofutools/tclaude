package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

func TestJourneyCLIRequiresExplicitSelectionsAndRequestIdentity(t *testing.T) {
	for _, args := range [][]string{
		{"history", "read", "conversation-a"},
		{"history", "read", "conversation-a", "--revision", "2", "--point", "point-a"},
		{"history", "refresh", "default"},
		{"workspace", "remove", "workspace-a", "--revision", "2"},
		{"work", "cancel", "work-a", "--revision", "3", "--reason", "stop"},
	} {
		t.Run(args[0]+"-"+args[1], func(t *testing.T) {
			calls := 0
			root := &cobra.Command{Use: "test", SilenceErrors: true, SilenceUsage: true}
			registerJourney(root, func(*cobra.Command, string, string, any) error { calls++; return nil })
			root.SetArgs(args)
			if err := root.Execute(); err == nil || calls != 0 {
				t.Fatalf("incomplete selection reached API: err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestJourneyCLIWorkSpecAndPointSelectionRemainExplicit(t *testing.T) {
	spec := filepath.Join(t.TempDir(), "work.json")
	if err := os.WriteFile(spec, []byte(`{"SourceMode":"fresh_handoff","FreshHandoff":"selected context","Brief":"check this change","Outcome":{"Mode":"human_decision"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	var method, path string
	var body map[string]any
	calls := 0
	root := &cobra.Command{Use: "test"}
	registerJourney(root, func(_ *cobra.Command, m, p string, b any) error {
		calls++
		method, path = m, p
		data, err := json.Marshal(b)
		if err != nil {
			return err
		}
		return json.Unmarshal(data, &body)
	})
	root.SetArgs([]string{"work", "start", "work-a", "--request-id", "request-a", "--spec-file", spec})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || method != "POST" || path != "/v2/work" || body["request_id"] != "request-a" || body["id"] != "work-a" {
		t.Fatalf("work request altered: %s %s %+v calls=%d", method, path, body, calls)
	}
	decoded := body["spec"].(map[string]any)
	if decoded["SourceMode"] != "fresh_handoff" || decoded["FreshHandoff"] != "selected context" {
		t.Fatalf("source mode lost: %+v", decoded)
	}
	root = &cobra.Command{Use: "test"}
	registerJourney(root, func(_ *cobra.Command, m, p string, b any) error {
		method, path = m, p
		data, _ := json.Marshal(b)
		return json.Unmarshal(data, &body)
	})
	root.SetArgs([]string{"history", "read", "conversation-a", "--revision", "7", "--point", "point-a", "--point-revision", "4"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	selection := body["selection"].(map[string]any)
	if path != "/v2/history/read" || selection["ConversationID"] != "conversation-a" || selection["ExpectedConversationRevision"] != float64(7) || selection["ExpectedPointRevision"] != float64(4) {
		t.Fatalf("point revision lost: %+v", body)
	}
}
