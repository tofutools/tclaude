// Package selftest provides hidden integration tests for manual verification.
package selftest

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/spf13/cobra"
)

type Params struct{}

func Cmd() *cobra.Command {
	cmd := boa.CmdT[Params]{
		Use:         "selftest",
		Short:       "Run integration self-tests (for developers)",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *Params, cmd *cobra.Command, args []string) {
			pass, fail := 0, 0
			for _, tc := range tests {
				fmt.Printf("  %-50s ", tc.name+"...")
				if err := tc.fn(); err != nil {
					fmt.Printf("FAIL: %v\n", err)
					fail++
				} else {
					fmt.Println("PASS")
					pass++
				}
			}
			fmt.Printf("\n%d passed, %d failed\n", pass, fail)
			if fail > 0 {
				os.Exit(1)
			}
		},
	}.ToCobra()
	cmd.Hidden = true
	return cmd
}

type testCase struct {
	name string
	fn   func() error
}

var tests = []testCase{
	{"credentials: read", testCredentialsRead},
	{"credentials: json round-trip preserves fields", testCredentialsRoundTrip},
	{"credentials: has refresh token", testCredentialsHasRefreshToken},
	{"usage API: fetch", testUsageFetch},
	{"usage API: fetch with retry", testUsageFetchWithRetry},
}

func testCredentialsRead() error {
	_, err := usageapi.GetAccessToken()
	return err
}

func testCredentialsRoundTrip() error {
	token, err := usageapi.GetAccessToken()
	if err != nil {
		return fmt.Errorf("cannot read token: %w", err)
	}

	// Read raw credentials, parse through the same map[string]json.RawMessage
	// path our updateCredentials uses, re-serialize, and compare
	raw, store, err := usageapi.ReadCredentialsForTest()
	if err != nil {
		return fmt.Errorf("cannot read raw credentials: %w", err)
	}
	fmt.Printf("(store=%s) ", store)

	var blob map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blob); err != nil {
		return fmt.Errorf("parse blob: %w", err)
	}

	oauthRaw, ok := blob["claudeAiOauth"]
	if !ok {
		return fmt.Errorf("missing claudeAiOauth")
	}
	var oauth map[string]json.RawMessage
	if err := json.Unmarshal(oauthRaw, &oauth); err != nil {
		return fmt.Errorf("parse oauth: %w", err)
	}

	// Verify expected fields
	expected := []string{"accessToken", "refreshToken", "expiresAt", "scopes"}
	for _, key := range expected {
		if _, ok := oauth[key]; !ok {
			return fmt.Errorf("missing field %q", key)
		}
	}

	// Re-serialize and compare semantically
	blob["claudeAiOauth"], _ = json.Marshal(oauth)
	reserialized, _ := json.Marshal(blob)

	var origMap, reserMap interface{}
	json.Unmarshal(raw, &origMap)
	json.Unmarshal(reserialized, &reserMap)

	origJSON, _ := json.MarshalIndent(origMap, "", "  ")
	reserJSON, _ := json.MarshalIndent(reserMap, "", "  ")

	if string(origJSON) != string(reserJSON) {
		return fmt.Errorf("JSON differs after round-trip")
	}

	// Verify the token we got matches what's in the blob
	var tokenStr string
	json.Unmarshal(oauth["accessToken"], &tokenStr)
	if tokenStr != token {
		return fmt.Errorf("GetAccessToken() != blob accessToken")
	}

	return nil
}

func testCredentialsHasRefreshToken() error {
	raw, _, err := usageapi.ReadCredentialsForTest()
	if err != nil {
		return err
	}

	var creds struct {
		ClaudeAiOauth struct {
			RefreshToken string `json:"refreshToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &creds); err != nil {
		return err
	}
	if creds.ClaudeAiOauth.RefreshToken == "" {
		return fmt.Errorf("no refresh token (token refresh on 429 will not work)")
	}
	return nil
}

func testUsageFetch() error {
	token, err := usageapi.GetAccessToken()
	if err != nil {
		return err
	}
	resp, err := usageapi.Fetch(token)
	if err != nil {
		return err
	}
	if resp.FiveHour != nil {
		fmt.Printf("(5h=%.0f%%) ", resp.FiveHour.Utilization)
	}
	if resp.SevenDay != nil {
		fmt.Printf("(7d=%.0f%%) ", resp.SevenDay.Utilization)
	}
	return nil
}

func testUsageFetchWithRetry() error {
	token, err := usageapi.GetAccessToken()
	if err != nil {
		return err
	}
	resp, err := usageapi.FetchWithRetry(token)
	if err != nil {
		return err
	}
	if resp.FiveHour != nil {
		fmt.Printf("(5h=%.0f%%) ", resp.FiveHour.Utilization)
	}
	return nil
}
