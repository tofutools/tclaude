package agentd

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// diffSensitive is wider than the page's pattern on purpose: it asks whether a
// key looks like it could hold something secret, and the page's pattern has to
// agree. A key it catches wrongly goes in diffNotSecret with what it holds.
var diffSensitive = regexp.MustCompile(`(?i)pass|secret|tok|cred|key|auth|cert|pem|p12|sign|salt|hmac|jwt|ssh|cookie|session|invite|bearer`)

// diffNotSecret lists keys that look sensitive but are not, and free-form values
// that hold no secret. Each says what it holds.
var diffNotSecret = map[string]string{
	"config.session":       "session settings",
	"config.session_watch": "session watch settings",
	"config.dashboard.close_terminal_tabs.session_ended":              "when to close a terminal tab",
	"config.processes.refuse_unsigned_imports":                        "a switch",
	"config.agent.http_proxies.*.model_policy.session_daily_requests": "a request limit",
}

// walkDiffKeys visits each JSON key a type can write. free reports a value whose
// inner names are not in the type, so only its own key can mask it.
func walkDiffKeys(t reflect.Type, path string, seen map[reflect.Type]bool, visit func(path, key string, free bool)) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	if t.Kind() == reflect.Map {
		walkDiffKeys(t.Elem(), path+".*", seen, visit)
		return
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return
	}
	seen[t] = true
	defer delete(seen, t)
	for i := range t.NumField() {
		field := t.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" || !field.IsExported() {
			continue
		}
		if field.Anonymous && name == "" {
			walkDiffKeys(field.Type, path, seen, visit)
			continue
		}
		if name == "" {
			name = field.Name
		}
		visit(path+"."+name, name, diffFreeForm(field.Type))
		walkDiffKeys(field.Type, path+"."+name, seen, visit)
	}
}

// diffFreeForm reports a value that can hold names the type does not declare: a
// map of text, or anything the type leaves open.
func diffFreeForm(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	if t == reflect.TypeFor[json.RawMessage]() || t.Kind() == reflect.Interface {
		return true
	}
	if t.Kind() != reflect.Map {
		return false
	}
	return diffFreeForm(t.Elem()) || t.Elem().Kind() == reflect.String
}

// diffSecretPattern reads the page's own pattern, so the two cannot drift.
func diffSecretPattern(t *testing.T) *regexp.Regexp {
	t.Helper()
	source, err := os.ReadFile("dashboard/js/capture-snapshot.js")
	require.NoError(t, err)
	found := regexp.MustCompile(`(?m)^const DIFF_SECRET_NAME = /(.+)/i;$`).FindSubmatch(source)
	require.NotNil(t, found, "the pattern must stay on one line for this test")
	return regexp.MustCompile(`(?i)` + string(found[1]))
}

// A confirm dialog prints config.json and sandbox profiles as text, so a key the
// page's pattern misses would reach a capture. Adding a secret to either shape
// fails here until the pattern covers it.
func TestDiffSecretsCoverConfigShapes(t *testing.T) {
	masked := diffSecretPattern(t)
	shapes := map[string]reflect.Type{
		"config":  reflect.TypeFor[config.Config](),
		"sandbox": reflect.TypeFor[sandboxProfileJSON](),
	}
	seen := map[string]bool{}
	for name, shape := range shapes {
		walkDiffKeys(shape, name, map[reflect.Type]bool{}, func(path, key string, free bool) {
			seen[key] = true
			if masked.MatchString(key) {
				return
			}
			if _, fine := diffNotSecret[path]; fine {
				return
			}
			assert.False(t, diffSensitive.MatchString(key), "%s looks secret but a diff dialog would show it: widen DIFF_SECRET_NAME, or say what it holds in diffNotSecret", path)
			assert.False(t, free, "%s holds names no type declares, so a diff dialog cannot mask them: name it to match DIFF_SECRET_NAME, or say what it holds in diffNotSecret", path)
		})
	}
	for _, key := range []string{"invite", "api_key_file", "value", "environment"} {
		assert.True(t, seen[key], "the walk no longer reaches %s", key)
	}
	for _, key := range []string{"invite", "api_key_file", "value", "client_secret", "Passwd", "COOKIE"} {
		assert.True(t, masked.MatchString(key), key)
	}
}

// The walk has to notice a secret added to a shape, or the test above proves nothing.
func TestDiffSecretsWalkFlagsANewField(t *testing.T) {
	type nested struct {
		SigningCert string            `json:"signing_cert"`
		Extra       map[string]string `json:"extra"`
		Hidden      string            `json:"-"`
	}
	type shape struct {
		Hub []nested `json:"hub"`
	}
	free := map[string]bool{}
	walkDiffKeys(reflect.TypeFor[shape](), "x", map[reflect.Type]bool{}, func(path, _ string, open bool) { free[path] = open })
	assert.Equal(t, map[string]bool{"x.hub": false, "x.hub.signing_cert": false, "x.hub.extra": true}, free)
	assert.True(t, diffSensitive.MatchString("signing_cert"))
	assert.False(t, diffSecretPattern(t).MatchString("signing_cert"), "so this one would have to be listed or the pattern widened")
}
