package paf

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

// The schemas are a byte-for-byte copy of docs/paf/schema/v0.1; a test keeps
// them in sync.
//
//go:embed schema/v0.1
var schemaFS embed.FS

// SchemaBaseURI is the $id prefix of the v0.1 schemas.
const SchemaBaseURI = "https://raw.githubusercontent.com/tofutools/tclaude/main/docs/paf/schema/v0.1/"

// Schema names, relative to SchemaBaseURI.
const (
	SchemaManifest    = "manifest.schema.json"
	SchemaAgent       = "agent.schema.json"
	SchemaEnvironment = "environment.schema.json"
	SchemaLine        = "transcript-line.schema.json"
	SchemaLosses      = "losses.schema.json"
	SchemaGroup       = "group.schema.json"
	SchemaPrivacy     = "privacy.schema.json"
	SchemaSignature   = "signature.schema.json"
	SchemaExtTclaude  = "ext/tclaude.schema.json"
)

var (
	schemaOnce     sync.Once
	schemaResolved map[string]*jsonschema.Resolved
	schemaErr      error
)

func loadSchema(name string) (*jsonschema.Schema, error) {
	b, err := schemaFS.ReadFile(path.Join("schema/v0.1", name))
	if err != nil {
		return nil, err
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("schema %s: %w", name, err)
	}
	return &s, nil
}

func resolveSchemas() {
	loader := func(u *url.URL) (*jsonschema.Schema, error) {
		name, ok := strings.CutPrefix(u.String(), SchemaBaseURI)
		if !ok {
			return nil, fmt.Errorf("paf: schema %s is not bundled", u)
		}
		return loadSchema(name)
	}
	schemaResolved = map[string]*jsonschema.Resolved{}
	for _, name := range []string{SchemaManifest, SchemaAgent, SchemaEnvironment, SchemaLine, SchemaLosses, SchemaGroup, SchemaPrivacy, SchemaSignature, SchemaExtTclaude} {
		s, err := loadSchema(name)
		if err != nil {
			schemaErr = err
			return
		}
		r, err := s.Resolve(&jsonschema.ResolveOptions{BaseURI: SchemaBaseURI + name, Loader: loader})
		if err != nil {
			schemaErr = fmt.Errorf("paf: resolve schema %s: %w", name, err)
			return
		}
		schemaResolved[name] = r
	}
}

// ValidateSchema validates one decoded JSON document (as produced by
// json.Unmarshal into any) against a bundled v1 schema.
func ValidateSchema(name string, doc any) error {
	schemaOnce.Do(resolveSchemas)
	if schemaErr != nil {
		return schemaErr
	}
	r, ok := schemaResolved[name]
	if !ok {
		return fmt.Errorf("paf: unknown schema %s", name)
	}
	return r.Validate(doc)
}

// validateJSON parses raw JSON and validates it against a schema.
func validateJSON(name string, raw []byte) error {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	return ValidateSchema(name, doc)
}
