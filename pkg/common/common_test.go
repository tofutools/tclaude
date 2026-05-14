package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSize_Bytes(t *testing.T) {
	tests := []struct {
		input    string
		expected int64
	}{
		{"0", 0},
		{"100", 100},
		{"1024", 1024},
		{"100b", 100},
		{"100B", 100},
	}

	for _, tt := range tests {
		result, err := ParseSize(tt.input)
		if !assert.NoErrorf(t, err, "ParseSize(%q) returned error", tt.input) {
			continue
		}
		assert.Equalf(t, tt.expected, result, "ParseSize(%q)", tt.input)
	}
}

func TestParseSize_Kilobytes(t *testing.T) {
	tests := []struct {
		input    string
		expected int64
	}{
		{"1k", KB},
		{"1K", KB},
		{"1kb", KB},
		{"1KB", KB},
		{"10k", 10 * KB},
		{"1.5k", int64(1.5 * float64(KB))},
	}

	for _, tt := range tests {
		result, err := ParseSize(tt.input)
		if !assert.NoErrorf(t, err, "ParseSize(%q) returned error", tt.input) {
			continue
		}
		assert.Equalf(t, tt.expected, result, "ParseSize(%q)", tt.input)
	}
}

func TestParseSize_Megabytes(t *testing.T) {
	tests := []struct {
		input    string
		expected int64
	}{
		{"1m", MB},
		{"1M", MB},
		{"1mb", MB},
		{"1MB", MB},
		{"10m", 10 * MB},
		{"1.5m", int64(1.5 * float64(MB))},
	}

	for _, tt := range tests {
		result, err := ParseSize(tt.input)
		if !assert.NoErrorf(t, err, "ParseSize(%q) returned error", tt.input) {
			continue
		}
		assert.Equalf(t, tt.expected, result, "ParseSize(%q)", tt.input)
	}
}

func TestParseSize_Gigabytes(t *testing.T) {
	tests := []struct {
		input    string
		expected int64
	}{
		{"1g", GB},
		{"1G", GB},
		{"1gb", GB},
		{"1GB", GB},
		{"2g", 2 * GB},
	}

	for _, tt := range tests {
		result, err := ParseSize(tt.input)
		if !assert.NoErrorf(t, err, "ParseSize(%q) returned error", tt.input) {
			continue
		}
		assert.Equalf(t, tt.expected, result, "ParseSize(%q)", tt.input)
	}
}

func TestParseSize_Terabytes(t *testing.T) {
	tests := []struct {
		input    string
		expected int64
	}{
		{"1t", TB},
		{"1T", TB},
		{"1tb", TB},
		{"1TB", TB},
	}

	for _, tt := range tests {
		result, err := ParseSize(tt.input)
		if !assert.NoErrorf(t, err, "ParseSize(%q) returned error", tt.input) {
			continue
		}
		assert.Equalf(t, tt.expected, result, "ParseSize(%q)", tt.input)
	}
}

func TestParseSize_WithWhitespace(t *testing.T) {
	tests := []struct {
		input    string
		expected int64
	}{
		{"  100  ", 100},
		{"10 m", 10 * MB},
		{" 1 g ", GB},
	}

	for _, tt := range tests {
		result, err := ParseSize(tt.input)
		if !assert.NoErrorf(t, err, "ParseSize(%q) returned error", tt.input) {
			continue
		}
		assert.Equalf(t, tt.expected, result, "ParseSize(%q)", tt.input)
	}
}

func TestParseSize_Invalid(t *testing.T) {
	tests := []string{
		"",
		"abc",
		"-10",
		"10x",
		"10 xyz",
		"m10",
	}

	for _, tt := range tests {
		_, err := ParseSize(tt)
		require.Errorf(t, err, "ParseSize(%q) should return error", tt)
	}
}
