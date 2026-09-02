package proxy

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// awb_test.go covers what `tclaude proxy awb` OFFERS and what it decides to
// SEND — the two halves the daemon cannot check for itself.
//
// The command tree is pinned because it is a promise: an agent that has been
// taught `awb` should find the same verbs here, and the flags that were left
// out should stay out. The request bodies are pinned because "clear it" and
// "leave it alone" differ on the wire only in whether a key is present, and
// which one a caller gets is decided by cobra's Changed().

// --- the command surface ---

// awbVerbPaths is every verb this proxy promises, as a path through the tree.
// Written out rather than derived, because a verb quietly disappearing is
// exactly what this is guarding against.
var awbVerbPaths = [][]string{
	{"whoami"},
	{"show"}, {"list"}, {"ready"}, {"blocked"}, {"search"},
	{"create"}, {"update"}, {"claim"}, {"release"}, {"close"}, {"reopen"}, {"delete"},
	{"label", "add"}, {"label", "rm"},
	{"dep", "add"}, {"dep", "rm"}, {"dep", "tree"},
	{"comment", "add"}, {"comment", "list"}, {"activity"},
	{"attach", "add"}, {"attach", "list"}, {"attach", "show"}, {"attach", "get"},
	{"attach", "delete"},
}

func TestAWBOffersEveryVerb(t *testing.T) {
	root := awbCmd()
	for _, path := range awbVerbPaths {
		cmd, _, err := root.Find(path)
		require.NoError(t, err, "%v", path)
		require.Equal(t, path[len(path)-1], cmd.Name(),
			"`tclaude proxy awb %s` must exist", strings.Join(path, " "))
	}
}

// TestAWBOmitsTheLocalOnlyFlags pins the deliberate omissions. Each names a
// database, a directory or a terminal that the daemon rather than the agent
// decides about, so offering it would promise something the proxy cannot mean.
func TestAWBOmitsTheLocalOnlyFlags(t *testing.T) {
	root := awbCmd()
	for _, path := range awbVerbPaths {
		cmd, _, err := root.Find(path)
		require.NoError(t, err)
		for _, flag := range []string{"db", "attachments", "color", "no-color", "no-context"} {
			assert.Nil(t, cmd.Flags().Lookup(flag),
				"`awb %s` must not offer --%s", strings.Join(path, " "), flag)
		}
	}
}

// TestAWBEveryVerbOffersBothOutputModes is the guard the spelled-out --json /
// --compact declarations need: boa registers no flag at all for an embedded
// params struct, so a new verb that "inherits" them inherits nothing.
func TestAWBEveryVerbOffersBothOutputModes(t *testing.T) {
	root := awbCmd()
	for _, path := range awbVerbPaths {
		cmd, _, err := root.Find(path)
		require.NoError(t, err)
		if path[len(path)-1] == "get" {
			// `attach get` is the exception, and awb's own: its output is not
			// text and not a mode.
			assert.Nil(t, cmd.Flags().Lookup("json"))
			assert.Nil(t, cmd.Flags().Lookup("compact"))
			continue
		}
		for _, flag := range []string{"json", "compact", "ask-human"} {
			assert.NotNil(t, cmd.Flags().Lookup(flag),
				"`awb %s` must offer --%s", strings.Join(path, " "), flag)
		}
	}
}

// TestAWBReadyHidesTheFiltersItFixesForItself mirrors awb: `ready` answers "what
// should nobody-in-particular pick up next", so an assignee or status filter is
// a usage error rather than a flag that parses and changes nothing.
func TestAWBReadyHidesTheFiltersItFixesForItself(t *testing.T) {
	root := awbCmd()
	ready, _, err := root.Find([]string{"ready"})
	require.NoError(t, err)
	for _, flag := range []string{"mine", "assignee", "unassigned", "status", "include-closed"} {
		assert.Nil(t, ready.Flags().Lookup(flag), "`awb ready` must not offer --%s", flag)
	}
	for _, flag := range []string{"type", "priority", "label", "workspace", "parent", "sort", "limit"} {
		assert.NotNil(t, ready.Flags().Lookup(flag), "`awb ready` must offer --%s", flag)
	}

	blocked, _, err := root.Find([]string{"blocked"})
	require.NoError(t, err)
	assert.Nil(t, blocked.Flags().Lookup("status"), "`blocked` fixes the status set for itself")
	assert.NotNil(t, blocked.Flags().Lookup("mine"), "`blocked` does take an assignee filter")
}

// TestAWBSearchDeclaresTheSameFiltersAsTheOtherListings is the guard the
// duplicated declaration needs: `search` cannot embed awbFilterParams, so the
// two sets are written twice and could drift.
func TestAWBSearchDeclaresTheSameFiltersAsTheOtherListings(t *testing.T) {
	root := awbCmd()
	list, _, err := root.Find([]string{"list"})
	require.NoError(t, err)
	search, _, err := root.Find([]string{"search"})
	require.NoError(t, err)

	list.Flags().VisitAll(func(f *pflag.Flag) {
		assert.NotNil(t, search.Flags().Lookup(f.Name),
			"`search` is missing --%s, which `list` offers", f.Name)
	})
	search.Flags().VisitAll(func(f *pflag.Flag) {
		assert.NotNil(t, list.Flags().Lookup(f.Name),
			"`search` offers --%s, which `list` does not — the two filter sets have drifted", f.Name)
	})
}

// TestAWBSortVocabularyIsPerVerb: boa ENFORCES an alternatives list, so only
// `search` may accept --sort relevance.
func TestAWBSortVocabularyIsPerVerb(t *testing.T) {
	assert.NotContains(t, awbSortAlternatives(false), "relevance")
	assert.Contains(t, awbSortAlternatives(true), "relevance")
	assert.Contains(t, awbSortAlternatives(false), "-priority")
	for _, sort := range []string{"order", "workspace", "status", "assignee", "blockers"} {
		assert.Contains(t, awbSortAlternatives(false), sort)
	}
}

func TestAWBProxyOutcomeAcceptsLegacyProjects(t *testing.T) {
	var out awbProxyOutcome
	require.NoError(t, json.Unmarshal([]byte(`{"projects":["awb"]}`), &out))
	out.normalizeCompatibility()
	assert.Equal(t, []string{"awb"}, out.Workspaces)
}

// --- the output modes ---

func TestAWBOutputMode(t *testing.T) {
	var stderr bytes.Buffer

	compact, rc := awbOutputMode(false, false, &stderr)
	assert.Equal(t, rcOK, rc)
	assert.False(t, compact, "--json is the default")

	compact, rc = awbOutputMode(false, true, &stderr)
	assert.Equal(t, rcOK, rc)
	assert.True(t, compact)

	_, rc = awbOutputMode(true, true, &stderr)
	assert.Equal(t, rcInvalidArg, rc, "awb refuses the combination, and so does this")
	assert.Contains(t, stderr.String(), "mutually exclusive")
}

// --- rendering ---

func TestAWBOutcomeRender(t *testing.T) {
	t.Run("compact text, with the trailing newline restored", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		out := &awbProxyOutcome{Text: "awb-a3f9c1 P1 open bug \"Thing\""}
		assert.Equal(t, rcOK, out.render(&stdout, &stderr))
		assert.Equal(t, "awb-a3f9c1 P1 open bug \"Thing\"\n", stdout.String())
	})

	t.Run("JSON is indented, and its key order is preserved", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		out := &awbProxyOutcome{JSON: json.RawMessage(`{"id":"awb-1","priority":2}`)}
		assert.Equal(t, rcOK, out.render(&stdout, &stderr))
		assert.Equal(t, "{\n  \"id\": \"awb-1\",\n  \"priority\": 2\n}\n", stdout.String(),
			"indenting rather than re-marshalling is what keeps the key order and the number types")
	})

	t.Run("a silent mutation prints nothing", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		out := &awbProxyOutcome{Workspaces: []string{"awb"}}
		assert.Equal(t, rcOK, out.render(&stdout, &stderr))
		assert.Empty(t, stdout.String(), "awb prints nothing on a successful mutation")
	})

	t.Run("content is written raw, bytes included", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		out := &awbProxyOutcome{Content: []byte("a\x00b"), HasContent: true}
		assert.Equal(t, rcOK, out.render(&stdout, &stderr))
		assert.Equal(t, "a\x00b", stdout.String())
	})

	t.Run("an empty attachment is not the same as no attachment", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		out := &awbProxyOutcome{HasContent: true}
		assert.Equal(t, rcOK, out.render(&stdout, &stderr))
		assert.Empty(t, stdout.String())
	})
}

// --- what create sends ---

func parsedAWBCreate(t *testing.T, argv ...string) (*cobra.Command, *awbCreateParams) {
	t.Helper()
	cmd := awbCreateCmd()
	require.NoError(t, cmd.Flags().Parse(argv))

	flags := cmd.Flags()
	p := &awbCreateParams{}
	get := func(name string) string {
		v, err := flags.GetString(name)
		require.NoError(t, err)
		return v
	}
	getAll := func(name string) []string {
		v, err := flags.GetStringSlice(name)
		require.NoError(t, err)
		return v
	}
	p.Description = get("description")
	p.DescriptionFile = get("description-file")
	p.Workspace = get("workspace")
	p.Type = get("type")
	p.Assignees = getAll("assignee")
	p.HasParent = get("has-parent")
	if flags.Changed("priority") {
		v, err := flags.GetInt("priority")
		require.NoError(t, err)
		p.Priority = &v
	}
	for _, bind := range []struct {
		name string
		into *[]string
	}{
		{"blocked-by", &p.BlockedBy},
		{"discovered-from", &p.DiscoveredFrom},
		{"related", &p.Related},
	} {
		v, err := flags.GetStringSlice(bind.name)
		require.NoError(t, err)
		*bind.into = v
	}
	if args := flags.Args(); len(args) > 0 {
		p.Title = args[0]
	}
	return cmd, p
}

func TestAWBCreateBody(t *testing.T) {
	t.Run("the minimum, with nothing invented", func(t *testing.T) {
		cmd, p := parsedAWBCreate(t, "--workspace", "awb", "Parser crashes")
		body, rc := buildAWBCreateBody(p, cmd, strings.NewReader(""), os.Stderr)
		require.Equal(t, rcOK, rc)
		assert.Equal(t, map[string]any{
			"workspace": "awb", "title": "Parser crashes", "compact": false,
		}, body, "an omitted type and priority must be AWB's defaults, not zeroes this sends")
	})

	t.Run("everything", func(t *testing.T) {
		cmd, p := parsedAWBCreate(t,
			"--workspace", "awb", "--type", "bug", "--priority", "1",
			"--assignee", "claude-1", "--assignee", "claude-2", "--has-parent", "awb-000001",
			"--blocked-by", "awb-000002", "--related", "awb-000003",
			"--description", "body text", "Parser crashes")
		body, rc := buildAWBCreateBody(p, cmd, strings.NewReader(""), os.Stderr)
		require.Equal(t, rcOK, rc)
		assert.Equal(t, "bug", body["type"])
		assert.Equal(t, 1, body["priority"])
		assert.Equal(t, []string{"claude-1", "claude-2"}, body["assignees"])
		assert.Equal(t, "awb-000001", body["has_parent"])
		assert.Equal(t, []string{"awb-000002"}, body["blocked_by"])
		assert.Equal(t, []string{"awb-000003"}, body["related"])
		assert.Equal(t, "body text", body["description"])
	})

	t.Run("priority 0 is a real priority", func(t *testing.T) {
		cmd, p := parsedAWBCreate(t, "--workspace", "awb", "--priority", "0", "Urgent")
		body, rc := buildAWBCreateBody(p, cmd, strings.NewReader(""), os.Stderr)
		require.Equal(t, rcOK, rc)
		assert.Equal(t, 0, body["priority"],
			"0 is the HIGHEST priority, so an unset int cannot stand in for 'not asked for'")
	})

	t.Run("workspace may be omitted for daemon-side inference", func(t *testing.T) {
		cmd, p := parsedAWBCreate(t, "Parser crashes")
		var stderr bytes.Buffer
		body, rc := buildAWBCreateBody(p, cmd, strings.NewReader(""), &stderr)
		assert.Equal(t, rcOK, rc)
		assert.NotContains(t, body, "workspace")
		assert.Empty(t, stderr.String())
	})

	t.Run("a title is required", func(t *testing.T) {
		cmd, p := parsedAWBCreate(t, "--workspace", "awb")
		var stderr bytes.Buffer
		_, rc := buildAWBCreateBody(p, cmd, strings.NewReader(""), &stderr)
		assert.Equal(t, rcInvalidArg, rc)
	})

	t.Run("--description-file - reads stdin", func(t *testing.T) {
		cmd, p := parsedAWBCreate(t, "--workspace", "awb", "--description-file", "-", "Thing")
		body, rc := buildAWBCreateBody(p, cmd, strings.NewReader("from stdin"), os.Stderr)
		require.Equal(t, rcOK, rc)
		assert.Equal(t, "from stdin", body["description"])
	})
}

// --- what update sends ---

func parsedAWBUpdate(t *testing.T, argv ...string) (*cobra.Command, *awbUpdateParams) {
	t.Helper()
	cmd := awbUpdateCmd()
	require.NoError(t, cmd.Flags().Parse(argv))

	flags := cmd.Flags()
	p := &awbUpdateParams{}
	for _, bind := range []struct {
		name string
		into **string
	}{
		{"title", &p.Title},
		{"type", &p.Type},
	} {
		if flags.Changed(bind.name) {
			v, err := flags.GetString(bind.name)
			require.NoError(t, err)
			*bind.into = &v
		}
	}
	if flags.Changed("priority") {
		v, err := flags.GetInt("priority")
		require.NoError(t, err)
		p.Priority = &v
	}
	description, err := flags.GetString("description")
	require.NoError(t, err)
	p.Description = description
	file, err := flags.GetString("description-file")
	require.NoError(t, err)
	p.DescriptionFile = file
	if args := flags.Args(); len(args) > 0 {
		p.ID = args[0]
	}
	return cmd, p
}

func TestAWBUpdateBody(t *testing.T) {
	t.Run("only what was typed", func(t *testing.T) {
		cmd, p := parsedAWBUpdate(t, "--title", "New title", "awb-a3f9c1")
		body, rc := buildAWBUpdateBody(p, cmd, strings.NewReader(""), os.Stderr)
		require.Equal(t, rcOK, rc)
		assert.Equal(t, map[string]any{
			"id": "awb-a3f9c1", "title": "New title", "compact": false,
		}, body)
	})

	t.Run("an empty --description CLEARS, and omitting it leaves the description alone",
		func(t *testing.T) {
			cmd, p := parsedAWBUpdate(t, "--description", "", "awb-a3f9c1")
			body, rc := buildAWBUpdateBody(p, cmd, strings.NewReader(""), os.Stderr)
			require.Equal(t, rcOK, rc)
			require.Contains(t, body, "description",
				"`--description ''` is the clear, and it differs from omitting the flag only in "+
					"whether the key is present")
			assert.Equal(t, "", body["description"])

			cmd, p = parsedAWBUpdate(t, "--title", "x", "awb-a3f9c1")
			body, rc = buildAWBUpdateBody(p, cmd, strings.NewReader(""), os.Stderr)
			require.Equal(t, rcOK, rc)
			assert.NotContains(t, body, "description")
		})

	t.Run("an id is required", func(t *testing.T) {
		cmd, p := parsedAWBUpdate(t, "--title", "x")
		var stderr bytes.Buffer
		_, rc := buildAWBUpdateBody(p, cmd, strings.NewReader(""), &stderr)
		assert.Equal(t, rcInvalidArg, rc)
	})

	t.Run("no field flags at all succeeds and changes nothing, as awb does", func(t *testing.T) {
		cmd, p := parsedAWBUpdate(t, "awb-a3f9c1")
		body, rc := buildAWBUpdateBody(p, cmd, strings.NewReader(""), os.Stderr)
		require.Equal(t, rcOK, rc)
		assert.Equal(t, map[string]any{"id": "awb-a3f9c1", "compact": false}, body)
	})
}

// --- the relation flags ---

func TestAWBDepRelationIsExactlyOne(t *testing.T) {
	blocker := "awb-000001"
	parent := "awb-000002"

	relType, other, rc := (&awbDepParams{BlockedBy: &blocker}).relation(os.Stderr)
	require.Equal(t, rcOK, rc)
	assert.Equal(t, "blocked-by", relType)
	assert.Equal(t, "awb-000001", other)

	var stderr bytes.Buffer
	_, _, rc = (&awbDepParams{}).relation(&stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "exactly one")

	stderr.Reset()
	_, _, rc = (&awbDepParams{BlockedBy: &blocker, HasParent: &parent}).relation(&stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "not 2")
}

// --- delete's confirmation ---

func TestAWBDeleteRefusesWithoutForce(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := runAWBDelete(&awbForceParams{ID: "awb-a3f9c1"}, &stdout, &stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "not recoverable")
	assert.Empty(t, stdout.String(),
		"the confirmation is checked before the daemon is contacted at all")
}

// --- attachment reading ---

func TestReadAWBAttachment(t *testing.T) {
	path := t.TempDir() + "/notes.md"
	require.NoError(t, os.WriteFile(path, []byte("hello"), 0o600))

	content, name, rc := readAWBAttachment(path, strings.NewReader(""), os.Stderr)
	require.Equal(t, rcOK, rc)
	assert.Equal(t, []byte("hello"), content)
	assert.Equal(t, "notes.md", name, "the default name is the file's own base name")

	t.Run("stdin has no name of its own", func(t *testing.T) {
		content, name, rc := readAWBAttachment("-", strings.NewReader("piped"), os.Stderr)
		require.Equal(t, rcOK, rc)
		assert.Equal(t, []byte("piped"), content)
		assert.Empty(t, name, "so the caller must supply --name")
	})

	t.Run("an empty file is not an attachment", func(t *testing.T) {
		empty := t.TempDir() + "/empty"
		require.NoError(t, os.WriteFile(empty, nil, 0o600))
		var stderr bytes.Buffer
		_, _, rc := readAWBAttachment(empty, strings.NewReader(""), &stderr)
		assert.Equal(t, rcInvalidArg, rc)
	})

	t.Run("over the limit is refused rather than buffered and then rejected", func(t *testing.T) {
		big := t.TempDir() + "/big"
		require.NoError(t, os.WriteFile(big, make([]byte, maxAWBAttachmentCLIBytes+1), 0o600))
		var stderr bytes.Buffer
		_, _, rc := readAWBAttachment(big, strings.NewReader(""), &stderr)
		assert.Equal(t, rcInvalidArg, rc)
		assert.Contains(t, stderr.String(), "limit")
	})

	t.Run("a missing file", func(t *testing.T) {
		var stderr bytes.Buffer
		_, _, rc := readAWBAttachment(t.TempDir()+"/absent", strings.NewReader(""), &stderr)
		assert.Equal(t, rcIOFailure, rc)
	})
}

func TestAWBFilterBody(t *testing.T) {
	limit := 10
	v := awbFilterValues{
		Statuses: []string{"open", " "}, Types: []string{"bug"}, Labels: []string{"parser"},
		Workspaces: []string{"awb"}, Mine: true, Limit: limit, Sort: "-priority",
		Priorities: []int{0, 1},
	}
	body := v.body(true)
	assert.Equal(t, []string{"open"}, body["statuses"], "a blank repeated value is dropped")
	assert.Equal(t, []string{"bug"}, body["types"])
	assert.Equal(t, []string{"parser"}, body["labels"])
	assert.Equal(t, []string{"awb"}, body["workspaces"])
	assert.Equal(t, []int{0, 1}, body["priorities"])
	assert.Equal(t, true, body["mine"])
	assert.Equal(t, 10, body["limit"])
	assert.Equal(t, "-priority", body["sort"])
	assert.Equal(t, true, body["compact"])

	empty := awbFilterValues{}.body(false)
	assert.Equal(t, map[string]any{"compact": false}, empty,
		"an unfiltered listing must send no filter at all, so the daemon supplies the defaults")
}

// --- the transport's own rule ---

// TestRejectInvalidUTF8 covers the one validation that cannot live in the
// daemon, because the transport destroys the evidence before it gets there.
//
// encoding/json replaces an invalid byte sequence with U+FFFD, so a daemon-side
// utf8.ValidString always passes and the original bytes are gone. AWB refuses
// such a body; without this check the proxy would store a replacement character
// and report success — in an append-only timeline nothing can edit afterwards.
func TestRejectInvalidUTF8(t *testing.T) {
	t.Run("json really does destroy it, which is why this check is here", func(t *testing.T) {
		encoded, err := json.Marshal(map[string]any{"body": "bad\xffbyte"})
		require.NoError(t, err)
		var out struct {
			Body string `json:"body"`
		}
		require.NoError(t, json.Unmarshal(encoded, &out))
		assert.True(t, utf8.ValidString(out.Body),
			"by the time the daemon sees it, it is valid — so the daemon cannot be the gate")
		assert.NotEqual(t, "bad\xffbyte", out.Body)
	})

	t.Run("a bad string field is refused, and named", func(t *testing.T) {
		var stderr bytes.Buffer
		rc := rejectInvalidUTF8(map[string]any{
			"id": "awb-a3f9c1", "body": "bad\xffbyte",
		}, &stderr)
		assert.Equal(t, rcInvalidArg, rc)
		assert.Contains(t, stderr.String(), "body")
		assert.Contains(t, stderr.String(), "not valid UTF-8")
	})

	t.Run("a bad element of a repeatable field is refused too", func(t *testing.T) {
		var stderr bytes.Buffer
		rc := rejectInvalidUTF8(map[string]any{"labels": []string{"ok", "bad\xff"}}, &stderr)
		assert.Equal(t, rcInvalidArg, rc)
		assert.Contains(t, stderr.String(), "labels")
	})

	t.Run("attachment content is skipped: base64 carries bytes exactly", func(t *testing.T) {
		var stderr bytes.Buffer
		rc := rejectInvalidUTF8(map[string]any{
			"id": "awb-a3f9c1", "name": "trace.bin", "content": []byte{0xff, 0xfe, 0x00},
		}, &stderr)
		assert.Equal(t, rcOK, rc,
			"binary content is exactly what attach add is for; it does not travel as a JSON string")
		assert.Empty(t, stderr.String())
	})

	t.Run("ordinary bodies pass, multi-byte characters included", func(t *testing.T) {
		var stderr bytes.Buffer
		rc := rejectInvalidUTF8(map[string]any{
			"body": "Reproduced — with an empty token stream 🎉", "compact": true, "limit": 10,
		}, &stderr)
		assert.Equal(t, rcOK, rc)
	})
}

// TestAWBHelpDoesNotPromiseCloseReasonClearing pins the schema migration into
// the user-facing text.
//
// AWB 0.6 has no mutable close-reason field: a reason is a comment, and it
// survives both a reopen and a forced claim. Help that says otherwise sends an
// agent to avoid a safe transition, or to report that history was destroyed
// when `comment list` still shows it.
func TestAWBHelpDoesNotPromiseCloseReasonClearing(t *testing.T) {
	root := awbCmd()
	for _, path := range awbVerbPaths {
		cmd, _, err := root.Find(path)
		require.NoError(t, err)
		text := cmd.Short + "\n" + cmd.Long
		lower := strings.ToLower(text)
		for _, claim := range []string{
			"clearing the close reason",
			"clears the close reason",
			"clear the close reason",
		} {
			assert.NotContains(t, lower, claim,
				"`awb %s` still promises to clear a close reason, which AWB no longer stores",
				strings.Join(path, " "))
		}
	}

	reopen, _, err := root.Find([]string{"reopen"})
	require.NoError(t, err)
	assert.Contains(t, reopen.Short, "clear every assignee",
		"reopen's short help should say what it actually does")
}

// TestAWBCommentAddRefusesInvalidUTF8BeforeTheDaemon is the end-to-end half of
// the check above: it goes through the real verb, so a future refactor that
// moved the guard out of the shared call path would fail here.
//
// It also proves the refusal happens before the daemon is contacted at all —
// which matters because there is no daemon in this test, and a check that ran
// later would report "agentd is not running" instead of the real problem.
func TestAWBCommentAddRefusesInvalidUTF8BeforeTheDaemon(t *testing.T) {
	file := t.TempDir() + "/note.md"
	require.NoError(t, os.WriteFile(file, []byte("latin-1 caf\xe9\n"), 0o600))

	var stdout, stderr bytes.Buffer
	rc := runAWBCommentAdd(&awbCommentAddParams{
		ID: "awb-a3f9c1", BodyFile: file,
	}, strings.NewReader(""), &stdout, &stderr)

	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "not valid UTF-8")
	assert.NotContains(t, stderr.String(), "agentd is not running",
		"the refusal must come before the daemon call, or it reports the wrong problem")
	assert.Empty(t, stdout.String())
}
