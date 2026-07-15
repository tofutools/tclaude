package pathv1

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

func Encode(st *RoutingState) ([]byte, error) {
	if st == nil {
		return nil, fmt.Errorf("nil path-v1 routing state")
	}
	if err := validateSerializationEnvelope(st); err != nil {
		return nil, err
	}
	clone := Clone(*st)
	data, err := json.MarshalIndent(clone, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode path-v1 routing state: %w", err)
	}
	data = append(data, '\n')
	if len(data) > MaxCheckpointBytes {
		return nil, &OverBudgetError{Limit: "checkpoint_bytes", Value: len(data), Maximum: MaxCheckpointBytes}
	}
	return data, nil
}

func Decode(data []byte) (*RoutingState, error) {
	if len(data) > MaxCheckpointBytes {
		return nil, &OverBudgetError{Limit: "checkpoint_bytes", Value: len(data), Maximum: MaxCheckpointBytes}
	}
	// encoding/json intentionally accepts duplicate object names and replaces
	// malformed Unicode. Run the bounded strict parser first so neither can be
	// normalized into an apparently valid routing record.
	if _, err := parseJCS(data); err != nil {
		return nil, fmt.Errorf("decode path-v1 strict JSON: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var st RoutingState
	if err := dec.Decode(&st); err != nil {
		return nil, fmt.Errorf("decode path-v1 routing state: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, fmt.Errorf("decode path-v1 routing state trailing JSON: %w", err)
		}
		return nil, fmt.Errorf("decode path-v1 routing state: multiple values")
	}
	if err := validateSerializationEnvelope(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

// validateSerializationEnvelope deliberately validates only the dormant
// record envelope. Whole-state authority and conservation validation belongs
// to the later aggregate-invariants layer; Encode and Decode never authorize
// path-v1 execution.
func validateSerializationEnvelope(st *RoutingState) error {
	if st.Protocol != Protocol {
		return fmt.Errorf("path-v1 protocol %q, want %q", st.Protocol, Protocol)
	}
	if st.Encoding != Encoding {
		return fmt.Errorf("path-v1 encoding %d, want %d", st.Encoding, Encoding)
	}
	maps := []struct {
		name string
		nil  bool
	}{
		{"paths", st.Paths == nil},
		{"scopes", st.Scopes == nil},
		{"reservations", st.Reservations == nil},
		{"activations", st.Activations == nil},
		{"candidateClosures", st.CandidateClosures == nil},
		{"causeRecords", st.CauseRecords == nil},
		{"causeSets", st.CauseSets == nil},
		{"detachmentSets", st.DetachmentSets == nil},
		{"detachments", st.Detachments == nil},
		{"propagation", st.Propagation == nil},
	}
	for _, item := range maps {
		if item.nil {
			return fmt.Errorf("path-v1 %s map is nil", item.name)
		}
	}
	return nil
}
