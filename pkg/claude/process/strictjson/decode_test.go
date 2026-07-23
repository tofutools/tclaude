package strictjson

import (
	"strings"
	"testing"
)

func TestDecodeRejectsDuplicateNamesAtEveryDepth(t *testing.T) {
	for _, input := range []string{
		`{"status":"running","status":"failed"}`,
		`{"nodes":{"task":"ready","task":"pending"}}`,
		`{"nodes":{"task":"ready","t\u0061sk":"pending"}}`,
		`{"commands":[{"id":"one","id":"two"}]}`,
	} {
		var dst any
		if err := Decode([]byte(input), &dst); err == nil || !strings.Contains(err.Error(), "duplicate object member") {
			t.Fatalf("Decode(%s) error = %v", input, err)
		}
	}
}

func TestDecodeRejectsUnknownAndTrailingDataForTypedDestination(t *testing.T) {
	type value struct {
		Status string `json:"status"`
	}
	for _, input := range []string{
		`{"status":"running","unknown":true}`,
		`{"status":"running"} {}`,
	} {
		var dst value
		if err := Decode([]byte(input), &dst); err == nil {
			t.Fatalf("Decode(%s) unexpectedly succeeded", input)
		}
	}
	var dst value
	if err := Decode([]byte{'{', '"', 's', 't', 'a', 't', 'u', 's', '"', ':', '"', 0xff, '"', '}'}, &dst); err == nil {
		t.Fatal("Decode accepted invalid UTF-8")
	}
}
