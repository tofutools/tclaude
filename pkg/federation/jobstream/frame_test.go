package jobstream

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func TestFollowReconnectPreservesSeparateChannelsAndSkipsDuplicates(t *testing.T) {
	var wire bytes.Buffer
	e := NewEncoder(&wire)
	_, _ = e.Write(Stdout, []byte("first\n"))
	_, _ = e.Write(Stderr, []byte("warning\n"))
	_, _ = e.Write(Stdout, []byte("second\n"))
	all := append([]byte{}, wire.Bytes()...)
	var cursor Cursor
	var out, errOut bytes.Buffer
	first, e1 := Read(&wire)
	if e1 != nil {
		t.Fatal(e1)
	}
	if e1 = cursor.Apply(first, &out, &errOut); e1 != nil {
		t.Fatal(e1)
	}
	reconnect := bytes.NewReader(all)
	for {
		f, err := Read(reconnect)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err = cursor.Apply(f, &out, &errOut); err != nil {
			t.Fatal(err)
		}
	}
	if out.String() != "first\nsecond\n" || errOut.String() != "warning\n" {
		t.Fatalf("output %q, stderr %q", out.String(), errOut.String())
	}
	if cursor.Stdout != 13 || cursor.Stderr != 8 {
		t.Fatalf("cursor %+v", cursor)
	}
}
func TestRejectsGapsOverlapsOversizedAndPartialFrames(t *testing.T) {
	cursor := Cursor{Stdout: 3}
	for _, f := range []Frame{{Channel: Stdout, Offset: 4, Data: []byte("gap")}, {Channel: Stdout, Offset: 2, Data: []byte("overlap")}} {
		if !errors.Is(cursor.Apply(f, io.Discard, io.Discard), ErrGap) {
			t.Fatal("accepted noncontiguous frame")
		}
	}
	var head [headerSize]byte
	head[0] = Stdout
	binary.BigEndian.PutUint32(head[9:], MaxChunk+1)
	if _, e := Read(bytes.NewReader(head[:])); !errors.Is(e, ErrFrame) {
		t.Fatalf("oversized: %v", e)
	}
	binary.BigEndian.PutUint32(head[9:], 4)
	if _, e := Read(bytes.NewReader(append(head[:], []byte("ab")...))); !errors.Is(e, io.ErrUnexpectedEOF) {
		t.Fatalf("partial frame: %v", e)
	}
	head[0] = 9
	if _, e := Read(bytes.NewReader(head[:])); !errors.Is(e, ErrFrame) {
		t.Fatalf("unknown channel: %v", e)
	}
}
func TestEncoderBoundsEachChannelWithoutBlockingPipeDrain(t *testing.T) {
	var wire bytes.Buffer
	e := NewEncoder(&wire)
	data := bytes.Repeat([]byte{0xff}, MaxChannel+100)
	if n, err := e.Write(Stdout, data); err != nil || n != len(data) {
		t.Fatalf("write %d: %v", n, err)
	}
	var c Cursor
	var out bytes.Buffer
	for {
		f, err := Read(&wire)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err = c.Apply(f, &out, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if out.Len() != MaxChannel || !bytes.Equal(out.Bytes(), data[:MaxChannel]) {
		t.Fatal("output cap or raw bytes changed")
	}
}
