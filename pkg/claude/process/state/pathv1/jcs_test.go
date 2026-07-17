package pathv1

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestCanonicalCheckpointProjectionExpansionBound(t *testing.T) {
	const expansion = len("100000000000000000000") - len("1e20")
	for _, test := range []struct {
		name       string
		outputSize int
	}{
		{name: "exact_bound", outputSize: MaxCheckpointBytes},
		{name: "bound_plus_one", outputSize: MaxCheckpointBytes + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			const prefix = `{"n":1e20,"p":"`
			const suffix = `"}`
			inputSize := test.outputSize - expansion
			input := []byte(prefix + strings.Repeat("x", inputSize-len(prefix)-len(suffix)) + suffix)
			if len(input) > MaxCheckpointBytes {
				t.Fatalf("input size = %d, exceeds source limit", len(input))
			}

			got, err := CanonicalCheckpointProjection(input, "")
			if test.outputSize == MaxCheckpointBytes {
				if err != nil {
					t.Fatal(err)
				}
				if len(got) != MaxCheckpointBytes {
					t.Fatalf("projection size = %d, want %d", len(got), MaxCheckpointBytes)
				}
				return
			}

			var over *OverBudgetError
			if !errors.As(err, &over) || over.Limit != "checkpoint_bytes" || over.Value != MaxCheckpointBytes+1 || over.Maximum != MaxCheckpointBytes {
				t.Fatalf("bound+1 projection error = %#v (%v)", over, err)
			}
			if got != nil {
				t.Fatalf("bound+1 projection returned %d bytes", len(got))
			}
		})
	}
}

func TestJCSAppendixBNumberVectors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		bits uint64
		raw  string
		want string
	}{
		{0x0000000000000000, "0", "0"},
		{0x8000000000000000, "-0", "0"},
		{0x0000000000000001, "5e-324", "5e-324"},
		{0x8000000000000001, "-5e-324", "-5e-324"},
		{0x7fefffffffffffff, "1.7976931348623157e+308", "1.7976931348623157e+308"},
		{0xffefffffffffffff, "-1.7976931348623157e+308", "-1.7976931348623157e+308"},
		{0x4340000000000000, "9007199254740992", "9007199254740992"},
		{0xc340000000000000, "-9007199254740992", "-9007199254740992"},
		{0x4430000000000000, "295147905179352830000", "295147905179352830000"},
		{0x44b52d02c7e14af5, "9.999999999999997e+22", "9.999999999999997e+22"},
		{0x44b52d02c7e14af6, "1e+23", "1e+23"},
		{0x44b52d02c7e14af7, "1.0000000000000001e+23", "1.0000000000000001e+23"},
		{0x444b1ae4d6e2ef4e, "999999999999999700000", "999999999999999700000"},
		{0x444b1ae4d6e2ef4f, "999999999999999900000", "999999999999999900000"},
		{0x444b1ae4d6e2ef50, "1e+21", "1e+21"},
		{0x3eb0c6f7a0b5ed8c, "9.999999999999997e-7", "9.999999999999997e-7"},
		{0x3eb0c6f7a0b5ed8d, "0.000001", "0.000001"},
		{0x41b3de4355555553, "333333333.3333332", "333333333.3333332"},
		{0x41b3de4355555554, "333333333.33333325", "333333333.33333325"},
		{0x41b3de4355555555, "333333333.3333333", "333333333.3333333"},
		{0x41b3de4355555556, "333333333.3333334", "333333333.3333334"},
		{0x41b3de4355555557, "333333333.33333343", "333333333.33333343"},
		{0xbecbf647612f3696, "-0.0000033333333333333333", "-0.0000033333333333333333"},
		{0x43143ff3c1cb0959, "1424953923781206.2", "1424953923781206.2"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.want, func(t *testing.T) {
			number, err := strconv.ParseFloat(test.raw, 64)
			if err != nil {
				t.Fatal(err)
			}
			if got := math.Float64bits(number); got != test.bits {
				t.Fatalf("%s bits = %016x, want %016x", test.raw, got, test.bits)
			}
			got, err := canonicalJCSNumber(test.raw)
			if err != nil || got != test.want {
				t.Fatalf("canonical number = %q, %v; want %q", got, err, test.want)
			}
		})
	}
	for _, raw := range []string{"NaN", "Infinity", "-Infinity", "1e9999"} {
		if got, err := canonicalJCSNumber(raw); err == nil {
			t.Errorf("invalid I-JSON number %q encoded as %q", raw, got)
		}
	}
}

func TestJCSAppendixBCanonicalBytesAndHash(t *testing.T) {
	t.Parallel()
	input := []byte(`{"status":"running","numbers":[0,-0,5e-324,-5e-324,1.7976931348623157e+308,-1.7976931348623157e+308,9007199254740992,-9007199254740992,295147905179352830000,9.999999999999997e+22,1e+23,1.0000000000000001e+23,999999999999999700000,999999999999999900000,1e+21,9.999999999999997e-7,0.000001,333333333.3333332,333333333.33333325,333333333.3333333,333333333.3333334,333333333.33333343,-0.0000033333333333333333,1424953923781206.2]}`)
	want := []byte(`{"numbers":[0,0,5e-324,-5e-324,1.7976931348623157e+308,-1.7976931348623157e+308,9007199254740992,-9007199254740992,295147905179352830000,9.999999999999997e+22,1e+23,1.0000000000000001e+23,999999999999999700000,999999999999999900000,1e+21,9.999999999999997e-7,0.000001,333333333.3333332,333333333.33333325,333333333.3333333,333333333.3333334,333333333.33333343,-0.0000033333333333333333,1424953923781206.2]}`)
	assertJCSBytesAndHash(t, input, want, "97b7dcd31a1b944b6636617c253d22e1cb3bc123e17cbba27f41195d173ff335")
}

func TestJCSUTF16PropertyOrderingBytesAndHash(t *testing.T) {
	t.Parallel()
	input := []byte(`{"\u20ac":"Euro Sign","\r":"Carriage Return","\ufb33":"Hebrew Letter Dalet With Dagesh","1":"One","\ud83d\ude00":"Emoji: Grinning Face","\u0080":"Control","\u00f6":"Latin Small Letter O With Diaeresis"}`)
	want := []byte("{\"\\r\":\"Carriage Return\",\"1\":\"One\",\"\u0080\":\"Control\",\"ö\":\"Latin Small Letter O With Diaeresis\",\"€\":\"Euro Sign\",\"😀\":\"Emoji: Grinning Face\",\"דּ\":\"Hebrew Letter Dalet With Dagesh\"}")
	assertJCSBytesAndHash(t, input, want, "5e321556d22018a9656991a9e94f77ec175fa193e52a2429d312f8419ec8b08c")
}

func assertJCSBytesAndHash(t *testing.T, input, want []byte, wantHash string) {
	t.Helper()
	got, err := CanonicalCheckpointProjection(input, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("canonical bytes = %s\nwant = %s", got, want)
	}
	sum := sha256.Sum256(got)
	if gotHash := hex.EncodeToString(sum[:]); gotHash != wantHash {
		t.Fatalf("canonical hash = %s, want %s", gotHash, wantHash)
	}
}
