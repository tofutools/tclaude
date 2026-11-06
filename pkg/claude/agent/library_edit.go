package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// The library edit commands (roles, templates, spawn profiles, sandbox
// profiles) each replace one named library entry from a JSON file, and share
// how they read it and how they report the result.

// loadLibraryJSONFile reads a --file JSON body (a path, or - for stdin) and
// unmarshals it into a T. noun names the entry in errors ("role", "template").
func loadLibraryJSONFile[T any](noun, file string, stdin io.Reader, stderr io.Writer) (*T, int) {
	if strings.TrimSpace(file) == "" {
		fmt.Fprintf(stderr, "Error: --file is required (path to a %s JSON file, or - to read stdin)\n", noun)
		return nil, rcInvalidArg
	}
	raw, rc := resolveBodyInput("", file, "--file", stdin, stderr)
	if rc != rcOK {
		return nil, rc
	}
	var v T
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		fmt.Fprintf(stderr, "Error: --file is not valid %s JSON: %v\n", noun, err)
		return nil, rcInvalidArg
	}
	return &v, rcOK
}

// printLibraryUpdated reports an edit of the entry named name, which the daemon
// stored as stored: an edit whose JSON names another entry renames it.
func printLibraryUpdated(stdout io.Writer, noun, name, stored string) {
	if stored != name {
		fmt.Fprintf(stdout, "Updated %s %q → renamed to %q\n", noun, name, stored)
		return
	}
	fmt.Fprintf(stdout, "Updated %s %q\n", noun, stored)
}
