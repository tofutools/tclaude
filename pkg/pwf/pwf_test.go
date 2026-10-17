package pwf_test

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/pwf"
)

const childSource = `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: review-loop
name: Review loop
start: s
params:
  target:
    type: string
nodes:
  s:
    type: start
    next:
      pass: review
  review:
    type: task
    performer:
      kind: agent
      profile: reviewer
      prompt: Review {{ params.target }}
    next:
      pass: done
  done:
    type: end
`

const rootSource = `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: ship-feature
name: Ship feature
description: Implement, review, approve, merge.
start: begin
params:
  issue:
    type: string
    required: true
nodes:
  begin:
    type: start
    next:
      pass: build
  build:
    type: task
    performer:
      kind: agent
      profile: dev
      assignee: checkout
      prompt: Implement {{ params.issue }}
    next:
      pass: review
  review:
    type: subprocess
    subprocess:
      templateRef: review-loop
      inputs:
        target: "{{ params.issue }}"
    next:
      pass: approve
  approve:
    type: decision
    performer:
      kind: human
      ask: Ship it?
    next:
      ship: merge
      hold: done
  merge:
    type: task
    performer:
      kind: program
      profile: git
      run: gh
      args: [pr, merge, --squash]
    next:
      pass: done
  done:
    type: end
`

func refOf(t *testing.T, source string) string {
	t.Helper()
	_, ref, err := pwf.ParseTemplate([]byte(source))
	require.NoError(t, err)
	return ref
}

func writeBundle(t *testing.T, keys ...paf.SigningKey) []byte {
	t.Helper()
	var buf bytes.Buffer
	err := pwf.Write(&buf, pwf.Bundle{
		Root:   refOf(t, rootSource),
		Readme: []byte("# Ship feature\n"),
		Templates: []pwf.Template{
			{Source: []byte(childSource), Head: true},
			{Source: []byte(rootSource), Head: true},
		},
	}, pwf.WriteOptions{Producer: paf.Producer{Name: "tclaude", Version: "test"}, CreatedAt: time.Unix(1_700_000_000, 0), Keys: keys})
	require.NoError(t, err)
	return buf.Bytes()
}

func open(t *testing.T, data []byte) (*pwf.Archive, error) {
	t.Helper()
	return pwf.Open(bytes.NewReader(data), int64(len(data)), paf.Limits{})
}

func TestRoundTripSummarizesWhatTheWorkflowNeeds(t *testing.T) {
	a, err := open(t, writeBundle(t))
	require.NoError(t, err)
	wf := a.Workflow
	assert.Equal(t, "Ship feature", wf.Title, "the title defaults to the root's name")
	assert.Equal(t, "Implement, review, approve, merge.", wf.Summary)
	assert.Equal(t, refOf(t, rootSource), wf.Root)
	require.Len(t, wf.Templates, 2)
	assert.Equal(t, "ship-feature", wf.Templates[0].ID, "root first")
	assert.Equal(t, []pwf.Param{{Name: "issue", Type: "string", Required: true}}, wf.Params)
	assert.Equal(t, []pwf.Use{{Name: "dev", Steps: []string{"ship-feature:build"}}, {Name: "reviewer", Steps: []string{"review-loop:review"}}}, wf.Roles)
	assert.Equal(t, []pwf.Use{{Name: "checkout", Steps: []string{"ship-feature:build"}}}, wf.Assignees)
	assert.Equal(t, []pwf.Program{{Step: "ship-feature:merge", Profile: "git", Run: "gh", Args: []string{"pr", "merge", "--squash"}}}, wf.Programs)
	assert.Equal(t, []string{"ship-feature:approve"}, wf.Humans)
	assert.Equal(t, "# Ship feature\n", string(a.Readme))
	assert.Equal(t, rootSource, string(a.Root().Source), "the authored source travels byte for byte")
	assert.Equal(t, "review-loop", a.Head("review-loop").Parsed.ID)
	assert.Empty(t, a.Signatures)
}

// draftSource fails validation: its task has no performer. The store keeps
// such drafts, so a workflow carries them too.
const draftSource = `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: draft
start: s
nodes:
  s:
    type: start
    next:
      pass: work
  work:
    type: task
    next:
      pass: done
  done:
    type: end
`

func TestAnIncompleteWorkflowIsWrittenAndItsProblemsReported(t *testing.T) {
	bundle := pwf.Bundle{Root: refOf(t, rootSource), Templates: []pwf.Template{{Source: []byte(rootSource), Head: true}}}
	want := `template "ship-feature" references "review-loop", which the file does not carry`
	problems, err := pwf.Problems(bundle)
	require.NoError(t, err)
	assert.Equal(t, []string{want}, problems)
	var buf bytes.Buffer
	require.NoError(t, pwf.Write(&buf, bundle, pwf.WriteOptions{}))
	a, err := open(t, buf.Bytes())
	require.NoError(t, err)
	assert.Equal(t, []string{want}, a.Problems, "a reader recomputes the same problems")
}

func TestATemplateWithValidationErrorsIsCarried(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, pwf.Write(&buf, pwf.Bundle{Root: refOf(t, draftSource), Templates: []pwf.Template{{Source: []byte(draftSource), Head: true}}}, pwf.WriteOptions{}))
	a, err := open(t, buf.Bytes())
	require.NoError(t, err)
	require.NotEmpty(t, a.Problems)
	assert.Contains(t, a.Problems[0], `template "draft": `)
	assert.Equal(t, a.Root().Problems[0], strings.TrimPrefix(a.Problems[0], `template "draft": `))

	clean, err := open(t, writeBundle(t))
	require.NoError(t, err)
	assert.Empty(t, clean.Problems)
}

func TestATemplateTheStoreCouldNotKeepIsRefused(t *testing.T) {
	_, _, err := pwf.ParseTemplate([]byte(draftSource + "surprise: true\n"))
	require.ErrorContains(t, err, `template "draft" cannot be carried`)
}

func TestSignaturesVerifyAndAreFormatSeparated(t *testing.T) {
	seed := bytes.Repeat([]byte{1}, ed25519.SeedSize)
	key := ed25519.NewKeyFromSeed(seed)
	data := writeBundle(t, paf.SigningKey{Key: key, KeyID: "node-1", Signer: &paf.Signer{Kind: "instance"}})
	a, err := open(t, data)
	require.NoError(t, err)
	require.Len(t, a.Signatures, 1)
	assert.Equal(t, paf.SignatureValid, a.Signatures[0].Status)
	assert.True(t, a.SignedBy(key.Public().(ed25519.PublicKey)))

	// A PAF signature over the same manifest bytes does not verify a PWF.
	pafSig, err := paf.Sign(a.ManifestRaw, paf.SigningKey{Key: key, KeyID: "node-1"})
	require.NoError(t, err)
	status, _ := paf.VerifyContext("pwf-manifest-signature-v1\n", a.ManifestRaw, pafSig)
	assert.Equal(t, paf.SignatureInvalid, status)
}

// rezip rewrites an archive, letting edit change or drop entries.
func rezip(t *testing.T, data []byte, edit func(name string, body []byte) ([]byte, bool)) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, f := range zr.File {
		rc, err := f.Open()
		require.NoError(t, err)
		body, err := io.ReadAll(rc)
		require.NoError(t, err)
		_ = rc.Close()
		body, keep := edit(f.Name, body)
		if !keep {
			continue
		}
		w, err := zw.Create(f.Name)
		require.NoError(t, err)
		_, err = w.Write(body)
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return out.Bytes()
}

func TestOpenRefusesTamperedFiles(t *testing.T) {
	good := writeBundle(t)
	cases := map[string]struct {
		edit func(name string, body []byte) ([]byte, bool)
		want string
	}{
		"changed template": {func(name string, body []byte) ([]byte, bool) {
			if strings.HasPrefix(name, pwf.TemplatesDir+"ship-feature/") {
				return bytes.Replace(body, []byte("--squash"), []byte("--admin!"), 1), true
			}
			return body, true
		}, "hash does not match the manifest"},
		"dropped dependency": {func(name string, body []byte) ([]byte, bool) {
			return body, !strings.HasPrefix(name, pwf.TemplatesDir+"review-loop/")
		}, `listed entry "templates/review-loop/`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			data := rezip(t, good, tc.edit)
			_, err := open(t, data)
			require.ErrorContains(t, err, tc.want)
		})
	}

	extra := rezip(t, good, func(_ string, body []byte) ([]byte, bool) { return body, true })
	var buf bytes.Buffer
	zr, _ := zip.NewReader(bytes.NewReader(extra), int64(len(extra)))
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		require.NoError(t, zw.Copy(f))
	}
	w, _ := zw.Create("evil.sh")
	_, _ = w.Write([]byte("rm -rf /"))
	require.NoError(t, zw.Close())
	_, err := open(t, buf.Bytes())
	require.ErrorContains(t, err, `entry "evil.sh" is not listed in the manifest`)
}

func TestSubprocessRefsSeesInlineBodies(t *testing.T) {
	tmpl, _, err := pwf.ParseTemplate([]byte(rootSource))
	require.NoError(t, err)
	refs := pwf.SubprocessRefs(tmpl)
	require.Len(t, refs, 1)
	assert.Equal(t, "review-loop", refs[0].TemplateRef)
	assert.Equal(t, model.NodeTypeSubprocess, tmpl.Nodes["review"].Type)
}

const loopSource = `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: rework
name: Rework until approved
start: implement
nodes:
  implement:
    type: task
    performer: {kind: agent, profile: dev, prompt: Implement it}
    next: review
  review:
    type: task
    performer: {kind: agent, profile: reviewer, prompt: Review it}
    next: decide
  decide:
    type: decision
    performer: {kind: human, ask: "Ship it?"}
    next: {approved: done, changes: implement, escalate: stuck}
    loops:
      changes: {maxIterations: 3, onExhausted: escalate}
  done: {type: end, result: success}
  stuck: {type: end, result: canceled}
`

func TestSummaryListsBoundedLoops(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, pwf.Write(&buf, pwf.Bundle{
		Root:      refOf(t, loopSource),
		Templates: []pwf.Template{{Source: []byte(loopSource), Head: true}},
	}, pwf.WriteOptions{Producer: paf.Producer{Name: "tclaude", Version: "test"}, CreatedAt: time.Unix(1_700_000_000, 0)}))
	a, err := open(t, buf.Bytes())
	require.NoError(t, err)
	assert.Equal(t, []pwf.Loop{{Step: "rework:decide", Outcome: "changes", To: "rework:implement",
		MaxIterations: 3, OnExhausted: "escalate"}}, a.Workflow.Loops)

	plain, err := open(t, writeBundle(t))
	require.NoError(t, err)
	assert.Nil(t, plain.Workflow.Loops, "a workflow without loops carries no loops field")
}
