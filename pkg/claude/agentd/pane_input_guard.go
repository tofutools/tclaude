package agentd

import (
	"errors"
	"github.com/tofutools/tclaude/pkg/claude/common/paneinput"
	"time"
)

func captureInputPane(target string) (string, error) {
	out, err := tmuxOutputWithTimeout("capture-pane", "-p", "-J", "-t", target)
	return string(out), err
}

func renameInputBlocked(convID string) bool {
	sess := aliveSessionForConv(convID)
	if sess == nil {
		return false
	}
	if err := paneinput.CheckHumanInput(sess.TmuxSession + ":0.0"); err != nil {
		return true
	}
	screen, err := captureInputPane(sess.TmuxSession + ":0.0")
	return err != nil || paneinput.ScreenAwaitsHumanInput(screen)
}

// Async post-init callers hold a rename until the same conversation is safe.
// The sink checks again after acquiring its locks and before every submit.
func deliverRenameWhenReady(convID, title string) bool {
	for {
		if aliveSessionForConv(convID) == nil {
			return false
		}
		if !renameInputBlocked(convID) {
			if deliverRename(convID, title) {
				return true
			}
			if !renameInputBlocked(convID) {
				return false
			}
		}
		waitPaneInputRetry()
	}
}

func injectTextWhenReady(convID, text string) error {
	for {
		sess := aliveSessionForConv(convID)
		if sess == nil {
			return errors.New("target pane exited before pending input could be delivered")
		}
		err := injectTextAndSubmit(sess.TmuxSession+":0.0", text)
		if !errors.Is(err, paneinput.ErrAwaitingHumanInput) {
			return err
		}
		waitPaneInputRetry()
	}
}

var waitPaneInputRetry = func() { time.Sleep(time.Second) }
