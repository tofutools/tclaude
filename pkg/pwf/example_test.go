//ci:reads docs/pwf/

package pwf_test

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/pwf"
)

const (
	docsDir    = "../../docs/pwf"
	exampleDir = docsDir + "/examples/ship-feature"
	schemaBase = "https://raw.githubusercontent.com/tofutools/tclaude/main/docs/pwf/schema/v0.1/"
)

// exampleKey is the published example signing key (an Ed25519 seed of 32
// 0x01 bytes, as for docs/paf/examples). Never trust it for real content.
func exampleKey() paf.SigningKey {
	return paf.SigningKey{Key: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32)), KeyID: "example", Signer: &paf.Signer{Kind: "publisher", Name: "example"}}
}

// TestSpecExampleOpensAndMatchesSchemas checks the unpacked example in
// docs/pwf/examples: zipped, it opens, verifies with the example key, and its
// JSON matches the published schemas. Run with PWF_REGEN_EXAMPLE=1 to rewrite
// it from the test templates after a format change.
func TestSpecExampleOpensAndMatchesSchemas(t *testing.T) {
	if os.Getenv("PWF_REGEN_EXAMPLE") == "1" {
		regenerateExample(t)
	}
	files := map[string][]byte{}
	require.NoError(t, filepath.WalkDir(exampleDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(exampleDir, p)
		files[filepath.ToSlash(rel)], err = os.ReadFile(p)
		return err
	}))
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.Create(n)
		require.NoError(t, err)
		_, err = w.Write(files[n])
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	a, err := pwf.Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()), paf.Limits{})
	require.NoError(t, err)
	assert.True(t, a.SignedBy(exampleKey().Key.Public().(ed25519.PublicKey)))
	assert.Equal(t, "Ship a feature", a.Workflow.Title)

	for doc, schema := range map[string]string{pwf.ManifestPath: "manifest.schema.json", pwf.WorkflowPath: "workflow.schema.json"} {
		var v any
		require.NoError(t, json.Unmarshal(files[doc], &v))
		require.NoError(t, resolvedSchema(t, schema).Validate(v), doc)
	}
}

func resolvedSchema(t *testing.T, name string) *jsonschema.Resolved {
	t.Helper()
	load := func(name string) (*jsonschema.Schema, error) {
		b, err := os.ReadFile(filepath.Join(docsDir, "schema/v0.1", name))
		if err != nil {
			return nil, err
		}
		var s jsonschema.Schema
		return &s, json.Unmarshal(b, &s)
	}
	s, err := load(name)
	require.NoError(t, err)
	r, err := s.Resolve(&jsonschema.ResolveOptions{BaseURI: schemaBase + name, Loader: func(u *url.URL) (*jsonschema.Schema, error) {
		n, ok := strings.CutPrefix(u.String(), schemaBase)
		if !ok {
			return nil, fmt.Errorf("schema %s is not local", u)
		}
		return load(n)
	}})
	require.NoError(t, err)
	return r
}

func regenerateExample(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, pwf.Write(&buf, pwf.Bundle{
		Root:      refOf(t, rootSource),
		Title:     "Ship a feature",
		Readme:    []byte("# Ship a feature\n\nA developer implements the issue, a reviewer checks it, you approve, and the PR is merged.\n"),
		Templates: []pwf.Template{{Source: []byte(rootSource), Head: true}, {Source: []byte(childSource), Head: true}},
	}, pwf.WriteOptions{Producer: paf.Producer{Name: "tclaude", Version: "example"}, CreatedAt: time.Date(2026, 10, 15, 18, 0, 0, 0, time.UTC), Keys: []paf.SigningKey{exampleKey()}}))
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(exampleDir))
	for _, f := range zr.File {
		rc, err := f.Open()
		require.NoError(t, err)
		body, err := io.ReadAll(rc)
		require.NoError(t, err)
		_ = rc.Close()
		dst := filepath.Join(exampleDir, filepath.FromSlash(f.Name))
		require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
		require.NoError(t, os.WriteFile(dst, body, 0o644))
	}
}
