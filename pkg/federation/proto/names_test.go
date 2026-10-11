package proto

import "testing"

func TestValidConversationRefPortableHarnesses(t *testing.T) {
	for _, id := range []string{"019fe740-43a4-7023-b8ae-1ee64459f2a1", "ses_historysource", "ses_0188abcd1234abcdefghijkLMN"} {
		if !ValidConversationRef(id) {
			t.Errorf("native conversation refused: %q", id)
		}
	}
	for _, id := range []string{"", "ses_", "ses_../secret", "bad-conversation", "agt_actor"} {
		if ValidConversationRef(id) {
			t.Errorf("invalid conversation accepted: %q", id)
		}
	}
}
