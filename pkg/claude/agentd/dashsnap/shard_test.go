package dashsnap

import (
	"fmt"
	"testing"
)

func TestParseShard(t *testing.T) {
	cases := []struct {
		spec    string
		want    Shard
		wantErr bool
	}{
		{spec: "", want: Shard{Index: 1, Total: 1}},
		{spec: "   ", want: Shard{Index: 1, Total: 1}},
		{spec: "1/1", want: Shard{Index: 1, Total: 1}},
		{spec: "2/4", want: Shard{Index: 2, Total: 4}},
		{spec: " 3 / 4 ", want: Shard{Index: 3, Total: 4}},
		{spec: "4/4", want: Shard{Index: 4, Total: 4}},
		{spec: "4", wantErr: true},
		{spec: "0/4", wantErr: true},
		{spec: "5/4", wantErr: true},
		{spec: "1/0", wantErr: true},
		{spec: "-1/4", wantErr: true},
		{spec: "1/-4", wantErr: true},
		{spec: "a/4", wantErr: true},
		{spec: "1/b", wantErr: true},
		{spec: "1/2/3", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.spec), func(t *testing.T) {
			got, err := ParseShard(tc.spec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseShard(%q) = %+v, want error", tc.spec, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseShard(%q): %v", tc.spec, err)
			}
			if got != tc.want {
				t.Fatalf("ParseShard(%q) = %+v, want %+v", tc.spec, got, tc.want)
			}
		})
	}
}

// TestShardPick proves the contract the sharded invocation relies on: for any
// shard count, the shards are disjoint, together cover the whole matrix in
// order, and stay balanced to within one state.
func TestShardPick(t *testing.T) {
	states := make([]State, 11)
	for i := range states {
		states[i].Key = fmt.Sprintf("state-%02d", i)
	}

	identity, err := ParseShard("")
	if err != nil {
		t.Fatalf("ParseShard(\"\"): %v", err)
	}
	if identity.Enabled() {
		t.Fatal("identity shard must not report Enabled")
	}
	if identity.Suffix() != "" {
		t.Fatalf("identity shard suffix = %q, want empty", identity.Suffix())
	}
	if got := identity.Pick(states); len(got) != len(states) {
		t.Fatalf("identity Pick returned %d of %d states", len(got), len(states))
	}

	for total := 1; total <= len(states)+1; total++ {
		var recombined []string
		byIndex := make([][]State, 0, total)
		for idx := 1; idx <= total; idx++ {
			shard := Shard{Index: idx, Total: total}
			picked := shard.Pick(states)
			byIndex = append(byIndex, picked)
			lo, hi := len(states)/total, (len(states)+total-1)/total
			if len(picked) < lo || len(picked) > hi {
				t.Fatalf("shard %d/%d picked %d states, want %d..%d", idx, total, len(picked), lo, hi)
			}
		}
		// Round-robin recombination: taking one state per shard in rotation
		// must reproduce the original matrix order exactly (disjoint + total
		// coverage + order preserved).
		for round := 0; ; round++ {
			advanced := false
			for _, picked := range byIndex {
				if round < len(picked) {
					recombined = append(recombined, picked[round].Key)
					advanced = true
				}
			}
			if !advanced {
				break
			}
		}
		if len(recombined) != len(states) {
			t.Fatalf("shards of %d recombine to %d states, want %d", total, len(recombined), len(states))
		}
		for i, key := range recombined {
			if key != states[i].Key {
				t.Fatalf("shards of %d recombine out of order at %d: got %s want %s", total, i, key, states[i].Key)
			}
		}
	}

	if got := (Shard{Index: 2, Total: 4}).Suffix(); got != "-shard2of4" {
		t.Fatalf("Suffix() = %q, want -shard2of4", got)
	}
}
