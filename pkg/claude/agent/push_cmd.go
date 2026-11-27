package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude notifications push …`: the operator's controls for Web Push, the
// CLI twin of the dashboard's (agentd push.go). Devices subscribe from the
// dashboard, since a subscription is the browser's; everything else is here
// too. The daemon refuses these to agents.

// pushDevice mirrors db.PushSubscription as the API shows it.
type pushDevice struct {
	ID        int64      `json:"id"`
	Label     string     `json:"label"`
	CreatedAt time.Time  `json:"created_at"`
	SeenAt    time.Time  `json:"seen_at"`
	LastOKAt  *time.Time `json:"last_ok_at"`
	LastError string     `json:"last_error"`
	Failures  int        `json:"failures"`
}

// pushStateView mirrors GET /v1/push.
type pushStateView struct {
	Enabled       bool            `json:"enabled"`
	Categories    map[string]bool `json:"categories"`
	CategoryOrder []string        `json:"category_order"`
	ContentFree   bool            `json:"content_free"`
	Devices       []pushDevice    `json:"devices"`
	PublicKey     string          `json:"public_key"`
	Removed       *int64          `json:"removed"`
}

type pushJSONParams struct {
	JSON bool `long:"json" help:"Output JSON"`
}

type pushRmParams struct {
	Device string `positional:"true" help:"Device id (see devices)"`
	JSON   bool   `long:"json" help:"Output JSON"`
}

type pushTestParams struct {
	Device string `positional:"true" optional:"true" help:"Device id; omit to send to every device"`
}

type pushSetParams struct {
	Setting string `positional:"true" help:"A category (see status) or content-free"`
	Value   string `positional:"true" help:"on | off"`
	JSON    bool   `long:"json" help:"Output JSON"`
}

type pushResetParams struct {
	Yes bool `long:"yes" help:"Confirm: every device is removed and must subscribe again"`
}

// NotificationsCmd is `tclaude notifications`.
func NotificationsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "notifications", Short: "Notification settings beyond the dashboard bell (operator only)"}
	push := &cobra.Command{
		Use:   "push",
		Short: "Web Push to your phone or browser: devices, categories, content-free mode, test",
		Long: `Web Push sends this node's notifications to the devices you subscribed from the
dashboard (More › Phone push on a phone), so they arrive with the app closed.
Nothing is sent until a device subscribes. The master switch (the dashboard bell)
and the agent and group mutes apply to push too; each category has its own
on/off here. Content-free mode sends "tclaude needs you" and no text.`,
	}
	push.AddCommand(
		boa.CmdT[pushJSONParams]{Use: "status", Short: "Show the categories, content-free mode and devices", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *pushJSONParams, _ *cobra.Command, _ []string) {
			configExit(runPushStatus(p.JSON, false, cliStdout, cliStderr))
		}}.ToCobra(),
		boa.CmdT[pushJSONParams]{Use: "devices", Short: "List the subscribed devices", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *pushJSONParams, _ *cobra.Command, _ []string) {
			configExit(runPushStatus(p.JSON, true, cliStdout, cliStderr))
		}}.ToCobra(),
		boa.CmdT[pushRmParams]{Use: "rm", Short: "Remove a device", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *pushRmParams, _ *cobra.Command, _ []string) {
			configExit(runPushRm(p, cliStdout, cliStderr))
		}}.ToCobra(),
		boa.CmdT[pushTestParams]{Use: "test", Short: "Send a test notification to one device or all", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *pushTestParams, _ *cobra.Command, _ []string) {
			configExit(runPushTest(p, cliStdout, cliStderr))
		}}.ToCobra(),
		boa.CmdT[pushSetParams]{Use: "set", Short: "Turn a category, or content-free mode, on or off", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *pushSetParams, _ *cobra.Command, _ []string) {
			configExit(runPushSet(p, cliStdout, cliStderr))
		}}.ToCobra(),
		boa.CmdT[pushResetParams]{Use: "reset-keys", Short: "Make a new push key; removes every device", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *pushResetParams, _ *cobra.Command, _ []string) {
			configExit(runPushReset(p, cliStdout, cliStderr))
		}}.ToCobra(),
	)
	cmd.AddCommand(push)
	return cmd
}

// pushRequest calls the daemon and maps a failure to an exit code.
func pushRequest(method, path string, in, out any, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	if err := DaemonRequest(method, path, in, out, DaemonOpts{}); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		var de *DaemonError
		if errors.As(err, &de) {
			switch de.Status {
			case http.StatusNotFound:
				return rcNotFound
			case http.StatusForbidden, http.StatusUnauthorized:
				return rcAuth
			case http.StatusBadRequest, http.StatusConflict:
				return rcInvalidArg
			}
		}
		return rcIOFailure
	}
	return rcOK
}

func runPushStatus(asJSON, devicesOnly bool, stdout, stderr io.Writer) int {
	var st pushStateView
	if rc := pushRequest(http.MethodGet, "/v1/push", nil, &st, stderr); rc != 0 {
		return rc
	}
	if asJSON {
		var v any = st
		if devicesOnly {
			v = st.Devices
		}
		return writePushJSON(stdout, v)
	}
	if !devicesOnly {
		printPushSettings(stdout, st)
		fmt.Fprintln(stdout)
	}
	printPushDevices(stdout, st.Devices)
	return rcOK
}

func writePushJSON(stdout io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return rcIOFailure
	}
	return rcOK
}

func printPushSettings(w io.Writer, st pushStateView) {
	if !st.Enabled {
		fmt.Fprintln(w, "Notifications are off for this node (the dashboard bell's master switch): nothing is pushed.")
	}
	fmt.Fprintln(w, "Categories:")
	for _, c := range st.CategoryOrder {
		fmt.Fprintf(w, "  %-20s %s\n", c, onOff(st.Categories[c]))
	}
	fmt.Fprintf(w, "Content-free:          %s\n", onOff(st.ContentFree))
}

func printPushDevices(w io.Writer, devices []pushDevice) {
	if len(devices) == 0 {
		fmt.Fprintln(w, "No device is subscribed. Subscribe one from the dashboard: More › Phone push on a phone.")
		return
	}
	fmt.Fprintln(w, "Devices:")
	for _, d := range devices {
		last := "never delivered"
		if d.LastOKAt != nil {
			last = "last delivered " + d.LastOKAt.Local().Format("2006-01-02 15:04")
		}
		line := fmt.Sprintf("  %-4d %-24s subscribed %s, %s", d.ID, d.Label, d.CreatedAt.Local().Format("2006-01-02"), last)
		if d.LastError != "" {
			line += " (last error: " + d.LastError + ")"
		}
		fmt.Fprintln(w, line)
	}
}

func parseDeviceID(s string, stderr io.Writer) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || id <= 0 {
		fmt.Fprintln(stderr, "Error: a device id is a positive number (see `tclaude notifications push devices`)")
		return 0, false
	}
	return id, true
}

func runPushRm(p *pushRmParams, stdout, stderr io.Writer) int {
	id, ok := parseDeviceID(p.Device, stderr)
	if !ok {
		return rcInvalidArg
	}
	var st pushStateView
	if rc := pushRequest(http.MethodDelete, "/v1/push/devices/"+strconv.FormatInt(id, 10), nil, &st, stderr); rc != 0 {
		return rc
	}
	if p.JSON {
		return writePushJSON(stdout, st.Devices)
	}
	fmt.Fprintf(stdout, "Removed device %d.\n", id)
	return rcOK
}

func runPushTest(p *pushTestParams, stdout, stderr io.Writer) int {
	body := map[string]any{}
	if p.Device != "" {
		id, ok := parseDeviceID(p.Device, stderr)
		if !ok {
			return rcInvalidArg
		}
		body["device"] = id
	}
	var out struct {
		Queued int64 `json:"queued"`
	}
	if rc := pushRequest(http.MethodPost, "/v1/push/test", body, &out, stderr); rc != 0 {
		return rc
	}
	fmt.Fprintf(stdout, "Test notification sent to %d device(s).\n", out.Queued)
	return rcOK
}

func runPushSet(p *pushSetParams, stdout, stderr io.Writer) int {
	var on bool
	switch strings.ToLower(strings.TrimSpace(p.Value)) {
	case "on", "true", "yes":
		on = true
	case "off", "false", "no":
	default:
		fmt.Fprintln(stderr, "Error: the value is on or off")
		return rcInvalidArg
	}
	setting := strings.TrimSpace(p.Setting)
	body := map[string]any{}
	if setting == "content-free" || setting == "content_free" {
		body["content_free"] = on
	} else {
		body["categories"] = map[string]bool{setting: on}
	}
	var st pushStateView
	if rc := pushRequest(http.MethodPost, "/v1/push/config", body, &st, stderr); rc != 0 {
		return rc
	}
	if p.JSON {
		return writePushJSON(stdout, st)
	}
	printPushSettings(stdout, st)
	return rcOK
}

func runPushReset(p *pushResetParams, stdout, stderr io.Writer) int {
	if !p.Yes {
		fmt.Fprintln(stderr, "Error: reset-keys removes every device, which must then subscribe again; pass --yes to go ahead")
		return rcInvalidArg
	}
	var st pushStateView
	if rc := pushRequest(http.MethodPost, "/v1/push/reset-keys", nil, &st, stderr); rc != 0 {
		return rc
	}
	removed := int64(0)
	if st.Removed != nil {
		removed = *st.Removed
	}
	fmt.Fprintf(stdout, "New push key on the next subscribe; removed %d device(s).\n", removed)
	return rcOK
}
