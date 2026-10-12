package harness

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestPiDirTrust(t *testing.T) {
	agent := t.TempDir()
	getenv := func(k string) string {
		if k == PiAgentDirEnvVar {
			return agent
		}
		return ""
	}
	project := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(agent, "trust.json")
	if err := os.WriteFile(store, []byte("{\n  \"/other\": false\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePiDirTrustedForLaunch(getenv, project); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(store)
	canonical, _ := filepath.EvalSymlinks(project)
	keys := []string{`"/other": false`, strconv.Quote(canonical) + ": true"}
	if canonical != project {
		keys = append(keys, strconv.Quote(project)+": true")
	}
	sort.Strings(keys)
	want := "{\n  " + strings.Join(keys, ",\n  ") + "\n}\n"
	if string(got) != want {
		t.Fatalf("store:\n%s\nwant:\n%s", got, want)
	}
	if _, err := os.Stat(store + ".lock"); !os.IsNotExist(err) {
		t.Fatal("pi's lock directory was left behind")
	}
	// Idempotent.
	if err := EnsurePiDirTrustedForLaunch(getenv, project); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(store); string(again) != want {
		t.Fatalf("second run rewrote the store:\n%s", again)
	}
}

func TestPlanPiDirTrust(t *testing.T) {
	cases := []struct {
		name, data string
		changed    bool
		wantErr    bool
	}{
		{name: "empty", data: "", changed: true},
		{name: "ancestor trusted", data: `{"/a": true}`},
		{name: "exact distrust", data: `{"/a/b": false}`, wantErr: true},
		{name: "ancestor distrust", data: `{"/a": false}`, changed: true},
		{name: "cleared entry", data: `{"/a/b": null}`, changed: true},
		{name: "not an object", data: `[]`, wantErr: true},
		{name: "bad value", data: `{"/a": "yes"}`, wantErr: true},
	}
	for _, tc := range cases {
		changed, _, err := planPiDirTrust([]byte(tc.data), "/a/b")
		if (err != nil) != tc.wantErr || changed != tc.changed {
			t.Errorf("%s: changed=%v err=%v", tc.name, changed, err)
		}
	}
}

func TestPiDirTrustDispatch(t *testing.T) {
	h := MustGet(PiName)
	if DirTrustStore(h) != piTrustStoreUI {
		t.Fatalf("store UI = %q", DirTrustStore(h))
	}
	if ok, err := ResolveTrustDir(h, true); !ok || err != nil {
		t.Fatalf("ResolveTrustDir = %v, %v", ok, err)
	}
}
