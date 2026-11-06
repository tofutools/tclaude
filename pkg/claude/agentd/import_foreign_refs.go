package agentd

import (
	"encoding/json"
	"fmt"
	"strings"

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
// Only recipients are checked. A foreign sender reaches only the file's own
// agents, and a legitimate archive can carry mail from an agent outside the
// group.

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
	own := map[string]bool{"": true} // operator and system mail is from or to ""
	for _, id := range importedIdentityConvIDs(exp) {
		own[id] = true
	}
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
