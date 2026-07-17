package pathv1

import (
	"bytes"
	"slices"
	"testing"
)

func FuzzInputSetCanonicalOrder(f *testing.F) {
	f.Add("a", "b")
	f.Add("é", "e\u0301")
	f.Fuzz(func(t *testing.T, a, b string) {
		left, err := InputSetIdentity([]string{a, b, a})
		if err != nil {
			t.Skip()
		}
		values := []string{a, b, a}
		slices.Reverse(values)
		right, err := InputSetIdentity(values)
		if err != nil {
			t.Skip()
		}
		if left != right {
			t.Fatalf("set identity depends on input order")
		}
	})
}
func FuzzDecodeNeverPanics(f *testing.F) {
	f.Add([]byte(`{"protocol":"path_v1"}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, data []byte) { _, _ = Decode(data) })
}

func FuzzCanonicalCheckpointProjection(f *testing.F) {
	f.Add([]byte(`{"status":"running","n":1e20,"nested":{"😀":true}}`))
	f.Add([]byte(`{"a":1,"a":2}`))
	f.Add([]byte(`{"bad":"\ud800"}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		projection, err := CanonicalCheckpointProjection(data, "")
		if err != nil {
			return
		}
		if len(projection) > MaxCheckpointBytes {
			t.Fatalf("successful projection exceeds checkpoint limit: %d", len(projection))
		}
		repeated, err := CanonicalCheckpointProjection(projection, "")
		if err != nil {
			t.Fatalf("canonical projection cannot be reparsed: %v", err)
		}
		if !bytes.Equal(repeated, projection) {
			t.Fatalf("canonical projection is not idempotent: %q != %q", repeated, projection)
		}
	})
}
