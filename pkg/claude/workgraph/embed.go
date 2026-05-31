package workgraph

import (
	"embed"
	"io/fs"
	"path"
	"sort"
)

// exampleFS holds the example templates shipped with the binary. They are the
// only templates tclaude ships; everything else is user data on disk. They
// double as canonical fixtures for the loader/parser tests.
//
//go:embed example
var exampleFS embed.FS

// exampleNames returns the names of the embedded example templates, sorted.
func exampleNames() []string {
	entries, err := fs.ReadDir(exampleFS, "example")
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// loadExample loads and validates an embedded example template by name.
func loadExample(name string) (*Template, error) {
	sub, err := fs.Sub(exampleFS, path.Join("example", name))
	if err != nil {
		return nil, err
	}
	return LoadFS(sub, string(SourceExample)+":"+name, SourceExample, "")
}
