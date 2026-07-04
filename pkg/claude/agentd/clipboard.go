package agentd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/wsl"
)

// maxClipboardBytes caps the decoded text an agent may write to the
// human's clipboard. Generous enough for a long snippet or draft, but
// bounded so one misbehaving sender can't pump an unbounded blob through
// the daemon into the platform copy tool.
const maxClipboardBytes = 256 * 1024

// maxClipboardRequestBytes bounds the raw POST body the daemon buffers,
// enforced by http.MaxBytesReader *before* the JSON decode — the same
// wire-vs-decoded split as maxNotifyHumanRequestBytes. JSON escaping
// inflates content (control / HTML-significant chars expand to a 6-byte
// \uXXXX), so the wire cap is the decoded cap times 6 plus envelope
// headroom: loose enough that no legitimate body is rejected pre-decode,
// yet far below the range where buffering it is a memory-DoS.
const maxClipboardRequestBytes = 6*maxClipboardBytes + 1024

// clipboardWrite is the platform clipboard-write seam. Production points
// it at writeToClipboard (which execs wl-copy/xclip/xsel, pbcopy, or
// clip.exe); flow tests swap in a recorder via SetClipboardWriterForTest
// so the copy path is asserted without touching a real display.
var clipboardWrite = writeToClipboard

// clipboardRequest is the POST /v1/clipboard body.
type clipboardRequest struct {
	Text string `json:"text"`
}

// handleClipboard serves POST /v1/clipboard — the daemon side of
// `tclaude agent clipboard`. It gates on the human.clipboard slug (NOT
// owner-implied and NOT default-granted — writing the operator's real
// clipboard needs an explicit grant or the --ask-human popup), then hands
// the text to the platform copy tool via the clipboardWrite seam.
//
// The daemon performs the write because the agent's sandbox can't reach
// the host display / clipboard; agentd runs on the host and can.
func handleClipboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	// Gate on the slug. requirePermission (not the *Ex owner-bypass form):
	// group ownership does NOT confer clipboard access — an explicit grant
	// or a per-call --ask-human approval are the only paths.
	if _, ok := requirePermission(w, r, PermHumanClipboard); !ok {
		return
	}
	// Cap the buffered body before decoding — see maxClipboardRequestBytes.
	r.Body = http.MaxBytesReader(w, r.Body, maxClipboardRequestBytes)
	var body clipboardRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	// Text is copied VERBATIM — leading/trailing whitespace and newlines
	// can be intentional (a code block, a trailing newline), so we don't
	// trim it. We only reject a genuinely empty payload; the CLI already
	// refuses a whitespace-only body so nothing meaningless reaches here.
	if body.Text == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "text is required (nothing to copy)")
		return
	}
	if len(body.Text) > maxClipboardBytes {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("text too long: %d bytes, max %d", len(body.Text), maxClipboardBytes))
		return
	}
	if err := clipboardWrite(body.Text); err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"failed to write to clipboard: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"copied": true, "bytes": len(body.Text)})
}

// clipboardTool names a platform copy command and the args that put it in
// "read stdin → set the clipboard" mode.
type clipboardTool struct {
	name string
	args []string
}

// writeToClipboard pipes text to the platform's clipboard tool. Returns a
// clear error when no tool is available so the daemon can surface it to
// the agent (and the human) rather than silently dropping the copy.
func writeToClipboard(text string) error {
	cmd, err := clipboardCommand()
	if err != nil {
		return err
	}
	cmd.Stdin = strings.NewReader(text)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s failed: %w (%s)", cmd.Path, err, msg)
		}
		return fmt.Errorf("%s failed: %w", cmd.Path, err)
	}
	return nil
}

// clipboardCommand resolves the platform copy tool to an *exec.Cmd whose
// stdin the caller fills with the clipboard text. The lookup mirrors
// openBrowser's runtime GOOS switch (rather than build-tagged files) so
// the whole clipboard path stays in one readable place.
func clipboardCommand() (*exec.Cmd, error) {
	switch runtime.GOOS {
	case "darwin":
		if p, err := exec.LookPath("pbcopy"); err == nil {
			return exec.Command(p), nil
		}
		return nil, fmt.Errorf("no clipboard tool found (pbcopy missing from PATH)")
	default: // linux and other unixes
		// Under WSL the real clipboard is Windows'; clip.exe reaches it
		// directly. (It reads the console codepage, so non-ASCII text can
		// mojibake — a known WSL limitation; ASCII snippets/commands, the
		// common case, are unaffected.)
		if wsl.IsWSL() {
			if c := findClipExe(); c != "" {
				return exec.Command(c), nil
			}
			// No clip.exe (unusual): fall through to the X11/Wayland tools,
			// which work under WSLg where a display is bridged in.
		}
		for _, t := range linuxClipboardTools() {
			if p, err := exec.LookPath(t.name); err == nil {
				return exec.Command(p, t.args...), nil
			}
		}
		return nil, fmt.Errorf("no clipboard tool found on PATH (install wl-clipboard, xclip, or xsel)")
	}
}

// linuxClipboardTools returns the copy tools to try, in preference order.
// A Wayland session advertises WAYLAND_DISPLAY, so wl-copy leads there;
// otherwise the X11 tools lead. Every tool is still listed as a fallback
// (a mixed WSLg/XWayland session may have any of them), so a session that
// misreports its display protocol can still find a working tool.
func linuxClipboardTools() []clipboardTool {
	wl := clipboardTool{name: "wl-copy"}
	xclip := clipboardTool{name: "xclip", args: []string{"-selection", "clipboard"}}
	xsel := clipboardTool{name: "xsel", args: []string{"--clipboard", "--input"}}
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		return []clipboardTool{wl, xclip, xsel}
	}
	return []clipboardTool{xclip, xsel, wl}
}

// findClipExe locates the Windows clipboard tool from inside WSL: PATH
// first (the usual /mnt/c interop entry), then the canonical System32
// path as a fallback for custom mount layouts. Returns "" if not found.
func findClipExe() string {
	if p, err := exec.LookPath("clip.exe"); err == nil {
		return p
	}
	if _, err := os.Stat("/mnt/c/Windows/System32/clip.exe"); err == nil {
		return "/mnt/c/Windows/System32/clip.exe"
	}
	return ""
}
