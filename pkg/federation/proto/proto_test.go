package proto

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIdentityRoundTripAndPerms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fed", "instance.key")
	a, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("key perms = %v, want 0600", st.Mode().Perm())
	}
	b, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID() != b.ID() {
		t.Fatalf("reload changed id: %s vs %s", a.ID(), b.ID())
	}
	if !ValidInstanceID(a.ID()) {
		t.Fatalf("invalid id %q", a.ID())
	}
	if err := SaveIdentity(path, a); err == nil {
		t.Fatal("SaveIdentity overwrote an existing key")
	}
}

func TestSealOpen(t *testing.T) {
	alice, _ := NewIdentity()
	bob, _ := NewIdentity()
	env, err := NewEnvelope(alice, KindMail, Endpoint{Agent: "agt_a"}, Endpoint{Instance: bob.ID(), Agent: "agt_b"},
		time.Hour, MailPayload{Body: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := Seal(alice, env)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	got, err := Open(s, alice.Pub, bob.ID(), now)
	if err != nil {
		t.Fatal(err)
	}
	var mp MailPayload
	if err := got.DecodePayload(&mp); err != nil || mp.Body != "hi" {
		t.Fatalf("payload = %+v, %v", mp, err)
	}

	// Wrong pinned key.
	if _, err := Open(s, bob.Pub, bob.ID(), now); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong key: %v", err)
	}
	// Wrong recipient.
	if _, err := Open(s, alice.Pub, alice.ID(), now); !errors.Is(err, ErrWrongTarget) {
		t.Fatalf("wrong target: %v", err)
	}
	// Expired.
	if _, err := Open(s, alice.Pub, bob.ID(), now.Add(2*time.Hour)); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired: %v", err)
	}
	// Tampered bytes.
	var m map[string]any
	_ = json.Unmarshal(s.Env, &m)
	m["kind"] = KindAck
	tampered, _ := json.Marshal(m)
	if _, err := Open(&Sealed{Env: tampered, Sig: s.Sig}, alice.Pub, bob.ID(), now); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered: %v", err)
	}
	// Signed by alice but claiming to be from bob's instance.
	env2 := *env
	env2.From.Instance = bob.ID()
	if _, err := Seal(alice, &env2); err == nil {
		t.Fatal("Seal accepted a foreign from.instance")
	}
}

func TestHello(t *testing.T) {
	id, _ := NewIdentity()
	other, _ := NewIdentity()
	f := &Frame{Type: FrameHello, InstanceID: id.ID(), PubKey: id.Pub, Sig: SignHello(id, "hub1", "n1")}
	if !VerifyHello(f, "hub1", "n1") {
		t.Fatal("valid hello rejected")
	}
	if VerifyHello(f, "hub2", "n1") || VerifyHello(f, "hub1", "n2") {
		t.Fatal("hello replayable across hubs or nonces")
	}
	f.InstanceID = other.ID()
	if VerifyHello(f, "hub1", "n1") {
		t.Fatal("hello accepted with id not derived from key")
	}
}
