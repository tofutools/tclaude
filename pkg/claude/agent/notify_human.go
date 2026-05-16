package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/humannotify"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent notify-human` — send the human a notification on an
// external channel (Telegram today). Permission-gated on human.notify
// so only a coordinating agent the human trusts (the PO) can use it;
// workers cannot spam the channel. The bare verb sends; the
// `resolve-chat-id` subcommand is a one-shot Telegram setup helper.

type notifyHumanParams struct {
	Body     string `pos:"true" optional:"true" help:"Notification text (or use --file)."`
	Subject  string `long:"subject" short:"s" optional:"true" help:"Optional one-line subject."`
	File     string `long:"file" short:"f" optional:"true" help:"Read the body from this file ('-' reads stdin). Sidesteps shell quoting — best for long, multi-line, or backtick-containing bodies."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
}

func notifyHumanCmd() *cobra.Command {
	return boa.CmdT[notifyHumanParams]{
		Use:   "notify-human",
		Short: "Send the human a notification on an external channel (e.g. Telegram)",
		Long: "Sends a message to the human on a configured external channel — letting a coordinating agent reach the human outside the terminal, e.g. on their phone.\n\n" +
			"The channel is configured under `human_notify` in ~/.tclaude/config.json (Telegram is the first transport). Sending is gated on the `human.notify` permission, which the human grants to the trusted coordinating agent (the PO) — so workers cannot spam the channel.\n\n" +
			"Give the body inline or with --file (--file - reads stdin). The `resolve-chat-id` subcommand helps obtain the Telegram chat_id during setup.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			notifyHumanResolveChatIDCmd(),
		},
		InitFuncCtx: func(ctx *boa.HookContext, p *notifyHumanParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *notifyHumanParams, _ *cobra.Command, _ []string) {
			os.Exit(runNotifyHuman(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runNotifyHuman(p *notifyHumanParams, stdin io.Reader, stdout, stderr io.Writer) int {
	body, rc := resolveBodyInput(p.Body, p.File, "the body argument", stdin, stderr)
	if rc != rcOK {
		return rc
	}
	if strings.TrimSpace(body) == "" {
		fmt.Fprintln(stderr, "Error: a notification body is required — pass it inline or via --file")
		return rcInvalidArg
	}
	ask, err := ParseAskHuman(p.AskHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}

	payload := map[string]any{"body": body}
	if s := strings.TrimSpace(p.Subject); s != "" {
		payload["subject"] = s
	}
	var resp struct {
		Transport string `json:"transport"`
		Delivered bool   `json:"delivered"`
		Handle    string `json:"handle"`
	}
	if err := DaemonRequest(http.MethodPost, "/v1/notify-human", payload, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Notified the human via %s.\n", resp.Transport)
	return rcOK
}

// ---- resolve-chat-id ----

type notifyHumanResolveChatIDParams struct{}

func notifyHumanResolveChatIDCmd() *cobra.Command {
	return boa.CmdT[notifyHumanResolveChatIDParams]{
		Use:   "resolve-chat-id",
		Short: "List the Telegram chats your bot has seen, to find your chat_id",
		Long: "One-shot Telegram setup helper. Calls the Bot API getUpdates with the bot token from ~/.tclaude/config.json (human_notify.telegram.bot_token) and prints the chats the bot has recently received a message from.\n\n" +
			"Setup flow: create a bot with @BotFather, put its token in config.json, send your new bot any message from Telegram, then run this command and copy the chat_id into human_notify.telegram.chat_id.\n\n" +
			"It only reads — it does not consume updates — so it is safe to run repeatedly.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(_ *notifyHumanResolveChatIDParams, _ *cobra.Command, _ []string) {
			os.Exit(runNotifyHumanResolveChatID(os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runNotifyHumanResolveChatID(stdout, stderr io.Writer) int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "Error: loading config: %v\n", err)
		return rcIOFailure
	}
	token := ""
	if cfg.HumanNotify != nil && cfg.HumanNotify.Telegram != nil {
		token = strings.TrimSpace(cfg.HumanNotify.Telegram.BotToken)
	}
	if token == "" {
		fmt.Fprintf(stderr, "Error: no Telegram bot token configured.\n"+
			"Set human_notify.telegram.bot_token in %s (get a token from @BotFather), then re-run.\n",
			config.ConfigPath())
		return rcInvalidArg
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	candidates, err := humannotify.ResolveTelegramChatIDs(ctx, token)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	if len(candidates) == 0 {
		fmt.Fprintln(stdout, "No chats found. Send your bot a message from Telegram first, then re-run.")
		return rcOK
	}
	fmt.Fprintf(stdout, "%-18s  %-12s  %s\n", "CHAT_ID", "TYPE", "CHAT")
	fmt.Fprintln(stdout, strings.Repeat("─", 52))
	for _, c := range candidates {
		fmt.Fprintf(stdout, "%-18s  %-12s  %s\n", c.ChatID, c.Type, c.Label)
	}
	fmt.Fprintf(stdout, "\nCopy the chat_id you want into human_notify.telegram.chat_id in %s\n",
		config.ConfigPath())
	return rcOK
}
