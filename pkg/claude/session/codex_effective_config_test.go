package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCodexEffectiveConfigNamesRemotelyDeliveredOrigins(t *testing.T) {
	raw := json.RawMessage(`{
	  "config": {
	    "model_provider": "corp",
	    "openai_base_url": "https://openai-gateway.example/v1",
	    "chatgpt_base_url": "https://codex.acme.example/backend-api/",
	    "cli_auth_credentials_store": "file",
	    "model_providers": {
	      "corp": {
	        "base_url": "https://models.example/v1",
	        "requires_openai_auth": true
	      }
	    }
	  },
	  "origins": {
	    "model": {
	      "name": {"type": "user", "file": "/home/u/.codex/config.toml"},
	      "version": "sha256:user"
	    },
	    "model_provider": {
	      "name": {"type": "enterpriseManaged", "id": "l1", "name": "acme-workspace"},
	      "version": "sha256:bundle"
	    },
	    "model_providers.corp.base_url": {
	      "name": {"type": "enterpriseManaged", "id": "l1", "name": "acme-workspace"},
	      "version": "sha256:bundle"
	    },
	    "chatgpt_base_url": {
	      "name": {"type": "mdm", "domain": "com.openai.codex", "key": "config"},
	      "version": "sha256:mdm"
	    },
	    "openai_base_url": {
	      "name": {"type": "system", "file": "/etc/codex/config.toml"},
	      "version": "sha256:system"
	    }
	  }
	}`)

	config, err := parseCodexEffectiveConfig(raw)
	require.NoError(t, err)

	assert.Equal(t, "corp", config.ModelProvider)
	assert.Equal(t, "https://openai-gateway.example/v1", config.OpenAIBaseURL)
	assert.Equal(t, "https://codex.acme.example/backend-api/", config.ChatGPTBaseURL)
	assert.Equal(t, "file", config.AuthStore)
	require.Contains(t, config.ModelProviders, "corp")
	assert.Equal(t, "https://models.example/v1",
		config.ModelProviders["corp"].BaseURL)
	assert.True(t, config.ModelProviders["corp"].RequiresOpenAIAuth)

	// Only remotely delivered layers are reported, and only for keys that can
	// move model traffic: a local system file and an unrelated key are not
	// out-of-band origins, so naming them would dilute the disclosure.
	assert.Equal(t, []codexRemoteConfigOrigin{
		{
			Key: "chatgpt_base_url", Layer: "mdm",
			Name: "com.openai.codex/config", Version: "sha256:mdm",
		},
		{
			Key: "model_provider", Layer: "enterpriseManaged",
			Name: "acme-workspace", Version: "sha256:bundle",
		},
		{
			Key: "model_providers.corp.base_url", Layer: "enterpriseManaged",
			Name: "acme-workspace", Version: "sha256:bundle",
		},
	}, config.RemoteOrigins)

	assert.Equal(t,
		`model_provider from enterpriseManaged layer "acme-workspace" (sha256:bundle)`,
		config.RemoteOrigins[1].String())
	// The MDM layer carries a preferences domain/key rather than an
	// admin-facing name; rendering the bare layer type would tell the operator
	// only what the words "mdm layer" already said.
	assert.Equal(t,
		`chatgpt_base_url from mdm layer "com.openai.codex/config" (sha256:mdm)`,
		config.RemoteOrigins[0].String())
}

func TestCodexProviderRoutingKeyMatchesTableMembers(t *testing.T) {
	for _, key := range []string{
		"model_provider", "model_providers",
		"model_providers.corp.base_url",
		"openai_base_url", "chatgpt_base_url", "profile", "profiles.work",
	} {
		assert.Truef(t, codexProviderRoutingKey(key), "expected %q to route", key)
	}
	for _, key := range []string{
		"model", "model_provider_extra", "sandbox_mode", "",
	} {
		assert.Falsef(t, codexProviderRoutingKey(key),
			"expected %q not to route", key)
	}
}

func TestCodexRemoteConfigOriginRendersWithoutOptionalFields(t *testing.T) {
	// An unnamed, unversioned layer must not render dangling punctuation.
	assert.Equal(t, "model_provider from enterpriseManaged layer",
		codexRemoteConfigOrigin{
			Key: "model_provider", Layer: "enterpriseManaged",
		}.String())
	assert.Equal(t, `model_provider from enterpriseManaged layer "acme"`,
		codexRemoteConfigOrigin{
			Key: "model_provider", Layer: "enterpriseManaged", Name: "acme",
		}.String())
}
