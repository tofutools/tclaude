package model

import (
	"strings"
	"testing"
)

func messageStepTemplate(performer *Performer, decision bool) *Template {
	tell := Node{Type: NodeTypeTask, Performer: performer, Next: Next{DefaultOutcome: "end"}}
	if decision {
		tell = Node{Type: NodeTypeDecision, Performer: performer, Next: Next{"yes": "end", "no": "end"}}
	}
	return &Template{APIVersion: APIVersion, Kind: Kind, ID: "message-step", Start: "start",
		Params: map[string]Param{"what": {Type: "string"}},
		Nodes: map[string]Node{
			"start": {Type: NodeTypeStart, Next: Next{DefaultOutcome: "tell"}},
			"tell":  tell,
			"end":   {Type: NodeTypeEnd},
		}}
}

func TestMessagePerformerValidates(t *testing.T) {
	ok := &Performer{Kind: PerformerMessage, To: []string{"operator", "lead", "group", "role:reviewer", "agent:rita"},
		Subject: "Ready: {{params.what}}", Text: "{{params.what}} is ready."}
	if codes := loopErrorCodes(messageStepTemplate(ok, false)); len(codes) != 0 {
		t.Fatalf("errors = %v", codes)
	}
	cases := map[string]struct {
		performer *Performer
		decision  bool
		code      string
	}{
		"no text":           {performer: &Performer{Kind: PerformerMessage, To: []string{"operator"}}, code: "missing_text"},
		"no recipient":      {performer: &Performer{Kind: PerformerMessage, Text: "hi"}, code: "missing_recipient"},
		"unknown recipient": {performer: &Performer{Kind: PerformerMessage, To: []string{"everyone"}, Text: "hi"}, code: "invalid_recipient"},
		"empty role":        {performer: &Performer{Kind: PerformerMessage, To: []string{"role:"}, Text: "hi"}, code: "invalid_recipient"},
		"param recipient":   {performer: &Performer{Kind: PerformerMessage, To: []string{"agent:{{params.who}}"}, Text: "hi"}, code: "invalid_recipient"},
		"twice":             {performer: &Performer{Kind: PerformerMessage, To: []string{"lead", "lead"}, Text: "hi"}, code: "duplicate_recipient"},
		"two-line subject":  {performer: &Performer{Kind: PerformerMessage, To: []string{"lead"}, Subject: "a\nb", Text: "hi"}, code: "invalid_subject"},
		"long text":         {performer: &Performer{Kind: PerformerMessage, To: []string{"lead"}, Text: strings.Repeat("x", MaxMessageTextBytes+1)}, code: "invalid_text"},
		"unknown param":     {performer: &Performer{Kind: PerformerMessage, To: []string{"lead"}, Text: "{{params.nope}}"}, code: "undeclared_param_ref"},
		"prompt on message": {performer: &Performer{Kind: PerformerMessage, To: []string{"lead"}, Text: "hi", Prompt: "x"}, code: "kind_scoped_field"},
		"to on an agent":    {performer: &Performer{Kind: PerformerAgent, Prompt: "x", To: []string{"lead"}}, code: "kind_scoped_field"},
		"message decides":   {performer: &Performer{Kind: PerformerMessage, To: []string{"lead"}, Text: "hi"}, decision: true, code: "message_on_decision"},
	}
	for name, tc := range cases {
		codes := loopErrorCodes(messageStepTemplate(tc.performer, tc.decision))
		if _, found := codes[tc.code]; !found {
			t.Errorf("%s: want %s, got %v", name, tc.code, codes)
		}
	}
}

func TestMessageTextReadsOutputs(t *testing.T) {
	tmpl := messageStepTemplate(&Performer{Kind: PerformerMessage, To: []string{"lead"}, Text: "PR: {{ outputs.pr }}"}, false)
	warned := false
	for _, diagnostic := range Validate(tmpl, NormalizeEdges(tmpl)) {
		if diagnostic.Code == "unknown_capture_ref" && strings.HasSuffix(diagnostic.Path, ".text") {
			warned = true
		}
	}
	if !warned {
		t.Fatal("an output no step declares is flagged in a message's text, as in a prompt")
	}
}
