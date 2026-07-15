package pathv1

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	legacy "github.com/tofutools/tclaude/pkg/claude/process/state"
)

func TestBuildInitializationProducesStrictCompleteSchema7Checkpoint(t *testing.T) {
	tmpl, st, needed := initializationFixture(t)
	if err := ValidateUnambiguousLegacyInitialization(&st, tmpl); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := BuildInitialization(t.Context(), needed, tmpl)
	if err != nil {
		t.Fatal(err)
	}
	data, err := EncodeCheckpointV7(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := DecodeCheckpointV7(data)
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip.Digest != checkpoint.Digest || roundTrip.Initialize.Command.ID == "" || roundTrip.Initialize.AdminRecord.ID == "" {
		t.Fatalf("incomplete round trip: %#v", roundTrip.Initialize)
	}
	if report := ValidateAggregate(roundTrip.Initialize.Aggregate.View()); !report.Valid() {
		t.Fatalf("invalid initialized aggregate: %#v", report.Diagnostics)
	}
	if _, err := legacy.Decode(data); !errors.Is(err, legacy.ErrNewerSchemaVersion) {
		t.Fatalf("active v6 decoder error = %v, want ErrNewerSchemaVersion", err)
	}
}

func TestPristineClassifierRejectsEveryProgressSurface(t *testing.T) {
	tmpl, pristine, _ := initializationFixture(t)
	tests := []struct {
		name   string
		mutate func(*legacy.State)
	}{
		{"status", func(st *legacy.State) { st.Status = legacy.RunStatusPaused }},
		{"sequence", func(st *legacy.State) { st.LastLogSeq = 1 }},
		{"command", func(st *legacy.State) {
			st.OutstandingCommands["c"] = legacy.OutstandingCommand{ID: "c", Status: legacy.CommandStatusObserved}
		}},
		{"wait", func(st *legacy.State) { st.Waits["w"] = legacy.WaitRecord{ID: "w", Status: legacy.WaitStatusSatisfied} }},
		{"timer", func(st *legacy.State) {
			st.Timers["t"] = legacy.TimerRecord{ID: "t", Status: legacy.WaitStatusSatisfied}
		}},
		{"contact", func(st *legacy.State) { st.Contacts["c"] = legacy.ContactState{CommandID: "c"} }},
		{"obligation", func(st *legacy.State) {
			st.Obligations["o"] = legacy.ObligationRecord{ID: "o", Status: legacy.WaitStatusSatisfied}
		}},
		{"admin", func(st *legacy.State) { st.AdminRecords = append(st.AdminRecords, legacy.AdminRecord{}) }},
		{"node", func(st *legacy.State) {
			node := st.Nodes[tmpl.Start]
			node.Status = legacy.NodeStatusCompleted
			st.Nodes[tmpl.Start] = node
		}},
		{"attempt", func(st *legacy.State) {
			node := st.Nodes[tmpl.Start]
			node.ActiveAttempt = &legacy.AttemptState{Attempt: 1}
			st.Nodes[tmpl.Start] = node
		}},
		{"block", func(st *legacy.State) {
			node := st.Nodes[tmpl.Start]
			node.BlockedReason = "poison"
			st.Nodes[tmpl.Start] = node
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			st := legacy.Clone(pristine)
			test.mutate(&st)
			if err := ValidateUnambiguousLegacyInitialization(&st, tmpl); !errors.Is(err, ErrInitializationAmbiguous) {
				t.Fatalf("error = %v, want typed ambiguity", err)
			}
		})
	}
}

func TestInitializationProofAndReplayFailClosed(t *testing.T) {
	tmpl, _, needed := initializationFixture(t)
	drain := needed
	drain.Reason = UpgradeLegacyDrainRequired
	drain.ActiveLegacyIDs = []LegacyActiveID{{Kind: LegacyActiveWait, ID: "wait"}}
	if _, err := BuildInitialization(t.Context(), drain, tmpl); !errors.Is(err, ErrInitializationInvalid) {
		t.Fatalf("drain error = %v", err)
	}
	admin := needed
	admin.CheckpointAdminRecords = []CheckpointLegacyAdminRecord{{}}
	if _, err := BuildInitialization(t.Context(), admin, tmpl); !errors.Is(err, ErrInitializationInvalid) {
		t.Fatalf("admin proof error = %v", err)
	}

	checkpoint, err := BuildInitialization(t.Context(), needed, tmpl)
	if err != nil {
		t.Fatal(err)
	}
	if err := ExactInitializationReplay(checkpoint, needed); err != nil {
		t.Fatal(err)
	}
	stale := needed
	stale.Checkpoint.Digest = strings.Repeat("c", 64)
	if err := ExactInitializationReplay(checkpoint, stale); !errors.Is(err, ErrInitializationInconsistent) {
		t.Fatalf("stale replay error = %v", err)
	}
	tampered := *checkpoint
	tampered.Digest = strings.Repeat("d", 64)
	if err := ValidateCheckpointV7(&tampered); !errors.Is(err, ErrInitializationInvalid) {
		t.Fatalf("tampered checkpoint error = %v", err)
	}
}

func TestInitializationCheckpointSchemaCompatibility(t *testing.T) {
	if LegacyMaxSchemaVersion != 6 || CheckpointStateSchemaVersion != 7 {
		t.Fatalf("schema boundary changed: legacy=%d checkpoint=%d", LegacyMaxSchemaVersion, CheckpointStateSchemaVersion)
	}
	if _, err := DecodeCheckpointV7([]byte(`{"stateSchemaVersion":6}`)); !errors.Is(err, ErrCheckpointSchemaInvalid) {
		t.Fatalf("schema-6 checkpoint error = %v", err)
	}
	// Version authority wins before strict unknown-field decoding, matching the
	// existing active-state compatibility contract.
	if _, err := DecodeCheckpointV7([]byte(`{"stateSchemaVersion":8,"future":true}`)); !errors.Is(err, ErrCheckpointSchemaNewer) {
		t.Fatalf("schema-8 checkpoint error = %v", err)
	}
}

func TestInitializationCheckpointIndependentGoldenSerialization(t *testing.T) {
	tmpl, _, needed := initializationFixture(t)
	checkpoint, err := BuildInitialization(t.Context(), needed, tmpl)
	if err != nil {
		t.Fatal(err)
	}
	data, err := EncodeCheckpointV7(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	serialized := sha256.Sum256(data)
	const wantSerialized = "e89b7e057214463d1de5f5783129f20c79cae46881e9a11ce2bfd4c1344ec0bd"
	if got := hex.EncodeToString(serialized[:]); got != wantSerialized {
		t.Fatalf("schema-7 serialized golden = %s", got)
	}

	// This test-local encoder is deliberately independent of parseJCS/writeJCS:
	// it decodes through encoding/json, sorts ASCII fixture keys itself, and
	// recursively emits compact JSON before hashing the complete event.
	eventJSON, err := json.Marshal(checkpoint.Initialize)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := independentCanonicalJSON(eventJSON)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(canonical)
	const wantDigest = "2e0c73bed54aafba2856bee1607fbd01e85837e2bc7809ce937aa0923d106a18"
	if got := hex.EncodeToString(digest[:]); got != wantDigest || checkpoint.Digest != wantDigest {
		t.Fatalf("schema-7 independent event digest = %s, checkpoint = %s", got, checkpoint.Digest)
	}
	const wantAggregate = "07fad4111e949df6d4d889057a0e062cfa1c7949e44ee2fbbdf2fc20aa56ff7f"
	if checkpoint.Initialize.AggregateDigest != wantAggregate {
		t.Fatalf("schema-7 aggregate golden = %s", checkpoint.Initialize.AggregateDigest)
	}
}

func initializationFixture(t *testing.T) (*model.Template, legacy.State, UpgradeNeeded) {
	t.Helper()
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "demo", Start: "implement",
		Nodes: map[string]model.Node{
			"implement": {Type: model.NodeTypeStart, Next: model.Next{"done": "end"}},
			"end":       {Type: model.NodeTypeEnd},
		},
	}
	templateHash, err := model.SemanticHash(tmpl)
	if err != nil {
		t.Fatal(err)
	}
	ref := model.TemplateRef(tmpl.ID, templateHash)
	st := legacy.New("run-init", ref, ref, []legacy.NodeInit{
		{ID: "implement", Type: model.NodeTypeStart, Status: legacy.NodeStatusReady},
		{ID: "end", Type: model.NodeTypeEnd, Status: legacy.NodeStatusPending},
	})
	st.Status = legacy.RunStatusRunning
	checkpointJSON, err := legacy.Encode(&st)
	if err != nil {
		t.Fatal(err)
	}
	needed, err := AssessUpgradeNeeded(t.Context(), checkpointJSON, &st, ref, strings.Repeat("b", 64), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return tmpl, st, needed
}

func independentCanonicalJSON(data []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := writeIndependentCanonical(&out, value); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func writeIndependentCanonical(out *bytes.Buffer, value any) error {
	switch value := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if value {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		encoded, _ := json.Marshal(value)
		out.Write(encoded)
	case json.Number:
		out.WriteString(string(value))
	case []any:
		out.WriteByte('[')
		for i, item := range value {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := writeIndependentCanonical(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			encoded, _ := json.Marshal(key)
			out.Write(encoded)
			out.WriteByte(':')
			if err := writeIndependentCanonical(out, value[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("unsupported independent JSON value %T", value)
	}
	return nil
}
