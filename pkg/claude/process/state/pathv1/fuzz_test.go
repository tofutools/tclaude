package pathv1

import (
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
