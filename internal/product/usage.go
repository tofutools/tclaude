package product

import (
	"fmt"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func registerUsage(root *cobra.Command, call apiCall) {
	usage := boa.CmdT[struct{}]{Use: "usage", Short: "Read attributable usage and explicit source coverage"}.ToCobra()
	for _, refresh := range []bool{false, true} {
		name := "query"
		if refresh {
			name = "refresh"
		}
		cmd := boa.CmdT[struct{}]{Use: name, Short: "Select a conversation or execution"}.ToCobra()
		cmd.Args = cobra.NoArgs
		var conversation, execution, cursor string
		var limit int
		cmd.Flags().StringVar(&conversation, "conversation", "", "Conversation ID")
		cmd.Flags().StringVar(&execution, "execution", "", "Execution ID")
		if !refresh {
			cmd.Flags().StringVar(&cursor, "cursor", "", "Next page cursor")
			cmd.Flags().IntVar(&limit, "limit", 50, "Maximum observations")
		}
		cmd.RunE = func(cmd *cobra.Command, _ []string) error {
			if conversation == "" && execution == "" {
				return fmt.Errorf("select --conversation or --execution")
			}
			target := app.UsageTarget{ConversationID: model.ConversationID(conversation), ExecutionID: model.ExecutionID(execution)}
			if refresh {
				return call(cmd, "POST", "/v2/usage/refresh", map[string]any{"target": target})
			}
			return call(cmd, "POST", "/v2/usage/query", map[string]any{"filter": app.UsageFilter{Target: target, Limit: limit, Cursor: cursor}})
		}
		usage.AddCommand(cmd)
	}
	activity := boa.CmdT[struct{}]{Use: "activity", Short: "Read durable operations and outcomes for one target"}.ToCobra()
	activity.Args = cobra.NoArgs
	var agent, conversation, execution, work, cursor, after string
	var limit int
	activity.Flags().StringVar(&agent, "agent", "", "Agent ID")
	activity.Flags().StringVar(&conversation, "conversation", "", "Conversation ID")
	activity.Flags().StringVar(&execution, "execution", "", "Execution ID")
	activity.Flags().StringVar(&work, "work", "", "Work Run ID")
	activity.Flags().StringVar(&cursor, "cursor", "", "Next page cursor")
	activity.Flags().StringVar(&after, "after", "", "Earliest timestamp (RFC3339)")
	activity.Flags().IntVar(&limit, "limit", 50, "Maximum records")
	activity.RunE = func(cmd *cobra.Command, _ []string) error {
		n := 0
		for _, v := range []string{agent, conversation, execution, work} {
			if v != "" {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("select exactly one of --agent, --conversation, --execution or --work")
		}
		var since time.Time
		var err error
		if after != "" {
			since, err = time.Parse(time.RFC3339, after)
			if err != nil {
				return err
			}
		}
		return call(cmd, "POST", "/v2/activity/query", map[string]any{"filter": app.ActivityFilter{Target: app.ActivityTarget{AgentID: model.AgentID(agent), ConversationID: model.ConversationID(conversation), ExecutionID: model.ExecutionID(execution), WorkRunID: model.WorkRunID(work)}, Limit: limit, Cursor: cursor, After: since}})
	}
	root.AddCommand(usage, activity)
}
