package configbundle

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigBundlePrepareAndResolve(t *testing.T) {
	t.Setenv("HOME", "/home/example")
	b := Bundle{Format: Format, FormatVersion: 1, Sections: map[string][]Item{"profiles": {{Name: "worker", Value: json.RawMessage(`{"path":"/opt/private","cwd":"/home/example/project","script":"echo ${HOME}; api_key=abc123456789","environment":[{"name":"API_TOKEN","value":"dont-export-me"},{"name":"LANG","value":"en_US.UTF-8"}],"darwin_disable_keychain_write":true}`)}}}}
	require.NoError(t, b.Prepare())
	raw := string(b.Sections["profiles"][0].Value)
	assert.NotContains(t, raw, "dont-export-me")
	assert.NotContains(t, raw, "/opt/private")
	assert.Contains(t, raw, `${HOME}/project`)
	assert.Contains(t, raw, `echo ${HOME}; api_key=abc123456789`)
	assert.Contains(t, raw, `"darwin_disable_keychain_write":true`)
	require.Len(t, b.Flags, 1)
	require.Len(t, b.Placeholders, 1)
	missing, err := b.Resolve(map[string]string{b.Placeholders[0].Name: `/new/"quoted`})
	require.NoError(t, err)
	assert.Empty(t, missing)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b.Sections["profiles"][0].Value, &got))
	assert.Equal(t, `/new/"quoted`, got["path"])
	assert.Equal(t, "/home/example/project", got["cwd"])
	assert.Equal(t, "echo ${HOME}; api_key=abc123456789", got["script"])
}

func TestConfigBundleSelectAndVersion(t *testing.T) {
	b := Bundle{Format: Format, FormatVersion: 1, Sections: map[string][]Item{"roles": {{Name: "one", Value: json.RawMessage(`{}`)}, {Name: "two", Value: json.RawMessage(`{}`)}}}}
	require.NoError(t, b.Validate())
	require.Error(t, b.Select(nil, []string{"rolse"}))
	require.NoError(t, b.Select([]string{"roles"}, []string{"roles/one"}))
	require.Len(t, b.Sections["roles"], 1)
	b.FormatVersion = 2
	require.ErrorContains(t, b.Validate(), "format_version 2")
	b.FormatVersion = 1
	b.Sections["roles"] = append(b.Sections["roles"], b.Sections["roles"][0])
	require.ErrorContains(t, b.Validate(), "duplicate")
}

func TestConfigBundleOpaqueTextDoesNotBecomeBinding(t *testing.T) {
	b := Bundle{Sections: map[string][]Item{"process-templates": {{Name: "build", Value: json.RawMessage(`{"source":"echo ${HOME} ${ordinary_shell_var}"}`)}}}}
	missing, err := b.Resolve(nil)
	require.NoError(t, err)
	assert.Empty(t, missing)
	assert.True(t, strings.Contains(string(b.Sections["process-templates"][0].Value), "${ordinary_shell_var}"))
}

func TestConfigBundlePreservesAllTemplateProseAndThreshold(t *testing.T) {
	b := Bundle{Sections: map[string][]Item{"templates": {{Name: "crew", Value: json.RawMessage(`{"work_pattern":[{"value":"/opt/prose ${HOME} ${task}"}],"process":[{"criteria":"/opt/criteria ${criterion}"}],"rhythms":[{"body":"/opt/body ${message}","subject":"/opt/subject ${title}"}]}`)}}, "config": {{Name: "claude_resume", Value: json.RawMessage(`{"token_threshold":12345,"threshold_minutes":30}`)}}}}
	originals := map[string]string{}
	for section, items := range b.Sections {
		originals[section] = string(items[0].Value)
	}
	require.NoError(t, b.Prepare())
	assert.Empty(t, b.Placeholders)
	missing, err := b.Resolve(nil)
	require.NoError(t, err)
	assert.Empty(t, missing)
	for section, items := range b.Sections {
		assert.JSONEq(t, originals[section], string(items[0].Value))
	}
}
