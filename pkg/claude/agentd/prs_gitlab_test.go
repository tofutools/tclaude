package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

func TestGitLabMRRefFromURL(t *testing.T) {
	ref, ok := gitlabMRRefFromURL("https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback/-/merge_requests/157")
	require.True(t, ok)
	assert.Equal(t, "https://gitlab.yamzz.test:6080", ref.base)
	assert.Equal(t, "gitlab.yamzz.test", ref.host, "the host carries no port, as the allow-list spells it")
	assert.Equal(t, "yamzz/sub/previewplayback", ref.project)
	assert.Equal(t, 157, ref.number)
	assert.Equal(t, "https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback!157", ref.key(),
		"the whole origin, so two instances on one host never share state")
	assert.Equal(t, "https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback/-/merge_requests/157", ref.canonical())
	assert.Equal(t, "gitlab.yamzz.test/yamzz/sub/previewplayback", ref.remote().Key())

	t.Run("a sub-page still identifies the merge request", func(t *testing.T) {
		ref, ok := gitlabMRRefFromURL("https://gitlab.com/acme/app/-/merge_requests/42/diffs?x=1")
		require.True(t, ok)
		assert.Equal(t, 42, ref.number)
		assert.Equal(t, "acme/app", ref.project)
	})
	for _, bad := range []string{
		"https://github.com/tofutools/tclaude/pull/42",
		"http://gitlab.com/acme/app/-/merge_requests/42",
		"https://gitlab.com/app/-/merge_requests/42",
		"https://gitlab.com/acme/app/-/issues/42",
		"https://gitlab.com/acme/app/merge_requests/42",
		"https://gitlab.com/acme/app/-/merge_requests/0",
		"https://gitlab.com/acme/app/-/merge_requests/x",
		"https://user:pw@gitlab.com/acme/app/-/merge_requests/1",
		"https://gitlab.com/ac me/app/-/merge_requests/1",
		"",
	} {
		_, ok := gitlabMRRefFromURL(bad)
		assert.False(t, ok, bad)
	}
}

func TestPRStateKeyAndNumberCoverGitLab(t *testing.T) {
	const mr = "https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback/-/merge_requests/157"
	assert.Equal(t, "gitlab:https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback!157", prStateKey(mr))
	other, ok := gitlabMRRefFromURL("https://gitlab.yamzz.test:7080/yamzz/sub/previewplayback/-/merge_requests/157")
	require.True(t, ok)
	assert.NotEqual(t, prStateKey(mr), prStateKey(other.canonical()), "another port is another instance")
	assert.Equal(t, prStateKey(mr), prStateKey(mr+"/diffs"), "sub-pages share one identity")
	assert.True(t, samePRURL(mr, mr+"/pipelines"))
	assert.Equal(t, 157, deriveForgePRNumber(mr))
	assert.Equal(t, 42, deriveForgePRNumber("https://github.com/tofutools/tclaude/pull/42"))
	assert.Equal(t, 0, deriveForgePRNumber("https://example.com/review/7"))
	assert.True(t, isForgePresentedPRURL(mr))
	assert.False(t, isForgePresentedPRURL("https://example.com/review/7"))
}

func TestValidateAgentPRURLAcceptsACanonicalMergeRequest(t *testing.T) {
	require.NoError(t, validateAgentPRURL("https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback/-/merge_requests/157", true))
	require.NoError(t, validateAgentPRURL("https://gitlab.com/acme/app/-/merge_requests/1", true))
	for _, rawURL := range []string{
		"https://gitlab.com/acme/app/-/merge_requests/1/diffs",
		"https://gitlab.com/acme/app/-/merge_requests/1?x=1",
		"https://gitlab.com/acme/app/-/merge_requests/1/",
		"https://gitlab.com/acme/app/-/merge_requests/01",
		"http://gitlab.com/acme/app/-/merge_requests/1",
		"https://gitlab.com/acme/app/-/issues/1",
	} {
		assert.Error(t, validateAgentPRURL(rawURL, true), rawURL)
	}
}

func TestGitLabMRPolicyBase(t *testing.T) {
	policy := config.GitProxyConfig{
		AllowedRemotes: []string{"gitlab.yamzz.test/yamzz"},
		GitLabHosts:    map[string]string{"gitlab.yamzz.test": "https://gitlab.yamzz.test:6080"},
	}
	ref, ok := gitlabMRRefFromURL("https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback/-/merge_requests/157")
	require.True(t, ok)
	base, err := gitlabMRPolicyBase(ref, policy)
	require.NoError(t, err)
	assert.Equal(t, "https://gitlab.yamzz.test:6080", base)

	t.Run("the URL must spell the configured base", func(t *testing.T) {
		ref, _ := gitlabMRRefFromURL("https://gitlab.yamzz.test/yamzz/sub/previewplayback/-/merge_requests/157")
		_, err := gitlabMRPolicyBase(ref, policy)
		assert.ErrorContains(t, err, "does not match the configured instance base")
	})
	t.Run("an unconfigured instance", func(t *testing.T) {
		ref, _ := gitlabMRRefFromURL("https://gitlab.other.test/yamzz/app/-/merge_requests/1")
		_, err := gitlabMRPolicyBase(ref, policy)
		assert.ErrorContains(t, err, "not a configured GitLab instance")
	})
	t.Run("a project outside the allow-list", func(t *testing.T) {
		ref, _ := gitlabMRRefFromURL("https://gitlab.yamzz.test:6080/other/app/-/merge_requests/1")
		_, err := gitlabMRPolicyBase(ref, policy)
		assert.ErrorContains(t, err, "allowed_remotes")
	})
	t.Run("gitlab.com needs no entry but still needs the allow-list", func(t *testing.T) {
		ref, _ := gitlabMRRefFromURL("https://gitlab.com/acme/app/-/merge_requests/1")
		_, err := gitlabMRPolicyBase(ref, config.GitProxyConfig{AllowedRemotes: []string{"gitlab.com/acme"}})
		assert.NoError(t, err)
		_, err = gitlabMRPolicyBase(ref, config.GitProxyConfig{})
		assert.Error(t, err)
	})
}

func TestGitLabJobCheckRun(t *testing.T) {
	cases := []struct {
		job        glJob
		bucket     string
		conclusion string
	}{
		{glJob{Name: "build", Status: "success"}, "pass", "success"},
		{glJob{Name: "test", Status: "failed"}, "fail", "failed"},
		{glJob{Name: "lint", Status: "failed", AllowFailure: true}, "skipped", "failed (allowed)"},
		{glJob{Name: "deploy", Status: "manual"}, "pending", "manual"},
		{glJob{Name: "old", Status: "canceled"}, "skipped", "cancelled"}, // one spelling for the summary
		{glJob{Name: "run", Status: "running"}, "pending", "running"},
		{glJob{Name: "wait", Status: "waiting_for_resource"}, "pending", "waiting for resource"},
	}
	for _, tc := range cases {
		run, ok := gitlabJobCheckRun(tc.job, "https://gitlab.example.com/a/b/-/pipelines/9")
		require.True(t, ok, tc.job.Name)
		assert.Equal(t, tc.bucket, run.Bucket, tc.job.Name)
		assert.Equal(t, tc.conclusion, run.Conclusion, tc.job.Name)
		assert.Equal(t, "https://gitlab.example.com/a/b/-/pipelines/9", run.RunURL)
	}
	_, ok := gitlabJobCheckRun(glJob{Status: "success"}, "")
	assert.False(t, ok, "a nameless job has no row")
}

func TestPRStateFromGitLab(t *testing.T) {
	assert.Equal(t, "open", prStateFromGitLab("opened", false))
	assert.Equal(t, "draft", prStateFromGitLab("opened", true))
	assert.Equal(t, "open", prStateFromGitLab("locked", false))
	assert.Equal(t, "merged", prStateFromGitLab("merged", true), "a merged draft is merged")
	assert.Equal(t, "closed", prStateFromGitLab("closed", false))
	assert.Equal(t, "", prStateFromGitLab("", false))
}
