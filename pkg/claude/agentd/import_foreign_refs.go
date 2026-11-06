package agentd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/tofutools/tclaude/pkg/claude/common/groupexport"
)

// The importer remaps only the file's own conversations: its Convs and both
// ends of its successions. An archived message or head alias that addresses
// any other conversation reaches that local conversation unchanged, with the
// archived sender, so a file could plant mail that looks like it came from a
// trusted agent (tcl-8eyl1x). A peer import refuses such a file
// (peerImportForeignRefs); this node's human import drops those entries and
// says so (dropImportForeignRefs).
//
// A foreign sender reaches only the file's own agents, and a legitimate
// archive carries mail from agents outside the group (a lead, the steward).
// But a sender that is a conversation on this node would show the imported
// agents mail from that trusted local agent, by its id and its name, and list
// it as sent in that agent's history (tcl-tns4sa). Every import path remaps
// such a sender to a fresh id (remapImportForeignSenders), which names nobody;
// a sender on no conversation here is kept.

// importForeignMessage is one archived message addressed outside the file.
type importForeignMessage struct {
	index int
	// malformed is a non-empty recipient list that is not a JSON list of
	// conversation ids: what it addresses cannot be told, so it counts as
	// foreign.
	malformed bool
}

// importForeignRefs classifies exp's messages and head aliases, before any
// remap, by whether they address only the file's own conversations. It
// returns the foreign ones in file order.
func importForeignRefs(exp *groupexport.Export) (messages []importForeignMessage, aliases []int) {
	own := importOwnConvs(exp)
	for i, m := range exp.Messages {
		refs := []string{m.ToConv, m.OriginalToConv}
		malformed := false
		for _, list := range []string{m.ToRecipients, m.CcRecipients} {
			if strings.TrimSpace(list) == "" {
				continue
			}
			var convs []string
			if json.Unmarshal([]byte(list), &convs) != nil {
				malformed = true
				continue
			}
			refs = append(refs, convs...)
		}
		foreign := malformed
		for _, ref := range refs {
			foreign = foreign || !own[ref]
		}
		if foreign {
			messages = append(messages, importForeignMessage{index: i, malformed: malformed})
		}
	}
	for i, a := range exp.HeadAliases {
		if !own[a.AnchorConvID] {
			aliases = append(aliases, i)
		}
	}
	return messages, aliases
}

// importOwnConvs is the set of conversation ids the importer remaps: the
// file's own conversations, both ends of its successions, and "" (operator
// and system mail is from or to "").
func importOwnConvs(exp *groupexport.Export) map[string]bool {
	own := map[string]bool{"": true}
	for _, id := range importedIdentityConvIDs(exp) {
		own[id] = true
	}
	return own
}

// remapImportForeignSenders gives every sender that is outside the file but a
// conversation on this node (local reports that) a fresh id, one per sender so
// its thread still reads as one, on its messages and on the head aliases it
// created. Nothing else in a message names its sender: the stored agent id is
// derived from the new id on insert, and every name shown for a sender is
// looked up from it, so the message shows an unknown sender. It returns a
// warning naming the messages by archived id.
func remapImportForeignSenders(exp *groupexport.Export, local func(string) bool) []string {
	own := importOwnConvs(exp)
	fresh := map[string]string{}
	remap := func(conv string) (string, bool) {
		if own[conv] {
			return conv, false
		}
		if id, ok := fresh[conv]; ok {
			return id, true
		}
		if !local(conv) {
			return conv, false
		}
		fresh[conv] = uuid.NewString()
		return fresh[conv], true
	}
	var ids []string
	for i := range exp.Messages {
		m := &exp.Messages[i]
		if to, ok := remap(m.FromConv); ok {
			m.FromConv = to
			ids = append(ids, fmt.Sprint(m.ID))
		}
	}
	aliases := 0
	for i := range exp.HeadAliases {
		a := &exp.HeadAliases[i]
		if to, ok := remap(a.ByConv); ok {
			a.ByConv = to
			aliases++
		}
	}
	var warnings []string
	if len(ids) > 0 {
		noun := "archived messages"
		if len(ids) == 1 {
			noun = "archived message"
		}
		warnings = append(warnings, fmt.Sprintf(
			"%d %s came from a conversation on this node outside the file; they land from an unknown sender instead (archived ids: %s)",
			len(ids), noun, listBounded(ids)))
	}
	if aliases > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%d head alias(es) were created by a conversation on this node outside the file; they land as created by an unknown conversation", aliases))
	}
	return warnings
}

// maxForeignListed bounds how many dropped entries a warning names.
const maxForeignListed = 20

// dropImportForeignRefs removes from exp the messages and head aliases that
// address a conversation outside the file, and returns a warning for each
// kind it dropped. Messages are named by their archived id and aliases by
// handle, quoted; no message text reaches the warning.
func dropImportForeignRefs(exp *groupexport.Export) []string {
	messages, aliases := importForeignRefs(exp)
	var warnings []string
	if len(messages) > 0 {
		drop := make(map[int]bool, len(messages))
		ids := make([]string, 0, len(messages))
		for _, m := range messages {
			drop[m.index] = true
			ids = append(ids, fmt.Sprint(exp.Messages[m.index].ID))
		}
		kept := exp.Messages[:0:0]
		for i, m := range exp.Messages {
			if !drop[i] {
				kept = append(kept, m)
			}
		}
		exp.Messages = kept
		noun := "archived messages"
		if len(ids) == 1 {
			noun = "archived message"
		}
		warnings = append(warnings, fmt.Sprintf(
			"dropped %d %s addressed to conversations outside the file, so none lands in a local agent's inbox (archived ids: %s)",
			len(ids), noun, listBounded(ids)))
	}
	if len(aliases) > 0 {
		drop := make(map[int]bool, len(aliases))
		handles := make([]string, 0, len(aliases))
		for _, i := range aliases {
			drop[i] = true
			handles = append(handles, fmt.Sprintf("%q", exp.HeadAliases[i].Handle))
		}
		kept := exp.HeadAliases[:0:0]
		for i, a := range exp.HeadAliases {
			if !drop[i] {
				kept = append(kept, a)
			}
		}
		exp.HeadAliases = kept
		warnings = append(warnings, fmt.Sprintf(
			"dropped %d head alias(es) pointing at conversations outside the file: %s", len(handles), listBounded(handles)))
	}
	return warnings
}

// listBounded joins up to maxForeignListed items and counts the rest.
func listBounded(items []string) string {
	if len(items) <= maxForeignListed {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(items[:maxForeignListed], ", "), len(items)-maxForeignListed)
}
