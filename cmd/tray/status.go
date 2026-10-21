//go:build windows

package main

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/lxn/walk"
	"github.com/lxn/win"

	"github.com/will25u/slimmedia-agent/internal/tray"
)

// statusWindow is the compact monitoring window: connection state, agent
// line, current/last job with progress, the last error in red, pending
// updates, and a rolling log tail. Closing it minimizes to the tray and the
// engine keeps running (ADR-009 §2); it is disposed only on quit.
type statusWindow struct {
	app *app
	mw  *walk.MainWindow
	th  *themeColors

	connLabel     *walk.TextLabel
	agentLabel    *walk.TextLabel
	activityLabel *walk.TextLabel
	jobBox        *walk.GroupBox
	jobLabel      *walk.TextLabel
	progress      *walk.ProgressBar
	errLabel      *walk.TextLabel
	updateLabel   *walk.TextLabel
	logEdit       *walk.TextEdit

	// startupErr holds an engine-start failure; the tray model has no
	// settable error field, so the window carries it directly.
	startupErr string
}

// newStatusWindow builds the status window on the UI thread.
func newStatusWindow(a *app) (*statusWindow, error) {
	mw, err := walk.NewMainWindow()
	if err != nil {
		return nil, err
	}
	th := currentTheme()
	sw := &statusWindow{app: a, mw: mw, th: th}
	if err := mw.SetTitle("SlimMedia Agent"); err != nil {
		return nil, err
	}
	if a.appIcon != nil {
		_ = mw.SetIcon(a.appIcon)
	}

	applyDWMTheme(mw.Handle(), th.isDark)
	mw.SetBackground(th.bgBrush)
	mw.WindowBase.SetBackground(th.bgBrush)

	baseFont, _ := walk.NewFont("Segoe UI", 9, 0)
	if baseFont != nil {
		mw.SetFont(baseFont)
	}

	if err := mw.SetLayout(walk.NewVBoxLayout()); err != nil {
		return nil, err
	}
	size := walk.Size{Width: 460, Height: 420}
	if err := mw.SetMinMaxSize(size, size); err != nil {
		return nil, err
	}
	if err := mw.SetSize(size); err != nil {
		return nil, err
	}

	if sw.connLabel, err = walk.NewTextLabel(mw); err != nil {
		return nil, err
	}
	bold, _ := walk.NewFont("Segoe UI", 10, walk.FontBold)
	if bold != nil {
		sw.connLabel.SetFont(bold)
	}
	sw.connLabel.SetTextColor(th.textStarting)

	if sw.agentLabel, err = walk.NewTextLabel(mw); err != nil {
		return nil, err
	}
	sw.agentLabel.SetTextColor(th.textPrimary)

	if sw.activityLabel, err = walk.NewTextLabel(mw); err != nil {
		return nil, err
	}
	sw.activityLabel.SetTextColor(th.textMuted)

	if sw.jobBox, err = walk.NewGroupBox(mw); err != nil {
		return nil, err
	}
	setControlTheme(sw.jobBox.Handle(), th.isDark)
	if err := sw.jobBox.SetLayout(walk.NewVBoxLayout()); err != nil {
		return nil, err
	}
	if sw.jobLabel, err = walk.NewTextLabel(sw.jobBox); err != nil {
		return nil, err
	}
	sw.jobLabel.SetTextColor(th.textPrimary)
	if sw.progress, err = walk.NewProgressBar(sw.jobBox); err != nil {
		return nil, err
	}
	setControlTheme(sw.progress.Handle(), th.isDark)
	sw.progress.SetRange(0, 100)
	sw.jobBox.SetVisible(false)

	if sw.errLabel, err = walk.NewTextLabel(mw); err != nil {
		return nil, err
	}
	sw.errLabel.SetTextColor(th.textError)

	if sw.updateLabel, err = walk.NewTextLabel(mw); err != nil {
		return nil, err
	}
	sw.updateLabel.SetTextColor(th.textPrimary)
	sw.updateLabel.SetVisible(false)

	if sw.logEdit, err = walk.NewTextEdit(mw); err != nil {
		return nil, err
	}
	if err := sw.logEdit.SetReadOnly(true); err != nil {
		return nil, err
	}
	sw.logEdit.SetBackground(th.editBgBrush)
	sw.logEdit.SetTextColor(th.textPrimary)
	setControlTheme(sw.logEdit.Handle(), th.isDark)
	if mono, _ := walk.NewFont("Consolas", 9, 0); mono != nil {
		sw.logEdit.SetFont(mono)
	}

	mw.Starting().Attach(func() {
		applyControlThemes(mw.Handle(), th.isDark)
	})

	// Every close reason (X button, Alt-F4, system menu) cancels the close
	// and hides to tray; only the explicit "Stop agent && quit" menu action
	// or unpairing from the server allows the window to dispose.
	mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		if a.isQuitting() || a.isUnpaired() {
			return
		}
		*canceled = true
		mw.Hide()
	})

	sw.render(a.model.View())
	return sw, nil
}

// show brings the status window to the front. Must run on the UI thread.
func (sw *statusWindow) show() {
	sw.mw.Show()
	sw.mw.Activate()
}

// setStartupError surfaces an engine-start failure in the error line; it
// outranks LastError because the agent is not running at all.
func (sw *statusWindow) setStartupError(err error) {
	sw.startupErr = err.Error()
	sw.render(sw.app.model.View())
}

// setStopping switches the window to the quitting state.
func (sw *statusWindow) setStopping() {
	sw.connLabel.SetText("● Stopping…")
	if sw.th != nil {
		sw.connLabel.SetTextColor(sw.th.connWarn)
	}
}

// render refreshes all widgets from a model snapshot. It runs on the UI
// thread, coalesced by requestRender to at most one paint per event burst
// plus the one-second status ticker.
func (sw *statusWindow) render(v tray.ViewState) {
	sw.renderConn(v)
	sw.agentLabel.SetText(fmt.Sprintf("Agent %s — %s (v%s)", v.Name, v.Server, v.Version))
	sw.renderActivity(v)
	sw.renderJob(v)
	sw.renderError(v)
	sw.renderUpdate(v)
	sw.renderLog(v)
}

// renderConn paints the connection line with a colored status bullet.
func (sw *statusWindow) renderConn(v tray.ViewState) {
	var text string
	var color walk.Color
	th := sw.th
	if th == nil {
		th = currentTheme()
	}
	switch v.Conn {
	case tray.ConnConnected:
		text = "● Connected"
		color = th.connOk
	case tray.ConnReconnecting:
		text = "● Reconnecting…"
		color = th.connWarn
	case tray.ConnOffline:
		text = "● Offline"
		color = th.connErr
	default:
		text = "● Starting…"
		color = th.textStarting
	}
	if v.ConnDetail != "" && v.Conn != tray.ConnConnected {
		text += " (" + v.ConnDetail + ")"
	}
	sw.connLabel.SetText(text)
	sw.connLabel.SetTextColor(color)
}

// renderActivity shows the agent status plus its current activity, e.g.
// "Working — pausing after chunk".
func (sw *statusWindow) renderActivity(v tray.ViewState) {
	status := titleStatus(v.AgentStatus)
	if v.Activity != "" {
		if v.Activity == "verifying" {
			status += " — verifying output"
		} else {
			status += " — " + v.Activity
		}
	}
	sw.activityLabel.SetText(status)
}

// renderJob shows the active job with its progress bar, or keeps the last
// finished job visible until a new progress event replaces it.
func (sw *statusWindow) renderJob(v tray.ViewState) {
	if v.Job == nil {
		sw.jobBox.SetVisible(false)
		return
	}
	job := v.Job
	sw.jobBox.SetVisible(true)
	if !job.Done {
		sw.jobBox.SetTitle("Current job")
		name := filepath.Base(job.InputPath)
		if job.Phase == "verifying" {
			sw.jobLabel.SetText(fmt.Sprintf("%s — verifying output…", name))
			sw.progress.SetVisible(true)
			sw.progress.SetValue(100)
			return
		}
		if (job.Phase == "replacing" || job.Phase == "placement") && (job.Activity != "" || job.SubPhase != "") {
			step := job.Activity
			if step == "" {
				step = "placing output"
			}
			if job.TotalBytes > 0 {
				sw.jobLabel.SetText(fmt.Sprintf("%s — %s: %s / %s (%.1f%%)",
					name,
					step,
					humanize.Bytes(uint64(job.TransferredBytes)),
					humanize.Bytes(uint64(job.TotalBytes)),
					job.ProgressPct,
				))
			} else {
				sw.jobLabel.SetText(fmt.Sprintf("%s — %s…", name, step))
			}
			sw.progress.SetVisible(true)
			sw.progress.SetValue(int(job.ProgressPct + 0.5))
			return
		}
		sw.jobLabel.SetText(fmt.Sprintf("%s — %.1f%% (speed %.1fx)", name, job.ProgressPct, job.Speed))
		sw.progress.SetVisible(true)
		sw.progress.SetValue(int(job.ProgressPct + 0.5))
		return
	}
	sw.progress.SetVisible(false)
	sw.jobBox.SetTitle("Last job")
	outcome := "completed"
	switch {
	case job.Cancelled:
		outcome = "cancelled"
	case !job.Success:
		outcome = "failed"
	}
	msg := job.Message
	// A cancellation's message is the fixed "job cancelled" string: appending
	// it to the "cancelled" outcome would read "abc cancelled: job cancelled".
	if msg != "" && !job.Cancelled {
		msg = ": " + tray.ClipForDisplay(msg, tray.MaxErrorLineLen)
	} else {
		msg = ""
	}
	sw.jobLabel.SetText(fmt.Sprintf("%s %s%s", job.ID, outcome, msg))
}

// renderError shows the last error (or a startup failure) in red, or clears
// the line when everything is fine.
func (sw *statusWindow) renderError(v tray.ViewState) {
	errText := v.LastError
	if sw.startupErr != "" {
		errText = sw.startupErr
	}
	if errText == "" {
		sw.errLabel.SetText("")
		return
	}
	sw.errLabel.SetText("⚠ " + tray.ClipForDisplay(errText, tray.MaxErrorLineLen))
}

// renderUpdate shows the update section only while update activity exists.
func (sw *statusWindow) renderUpdate(v tray.ViewState) {
	if v.Update == nil {
		sw.updateLabel.SetVisible(false)
		return
	}
	u := v.Update
	sw.updateLabel.SetVisible(true)
	stateText := map[tray.UpdateState]string{
		tray.UpdateAvailable:       "available",
		tray.UpdateAwaitingConsent: "waiting for your confirmation",
		tray.UpdateGranted:         "installing…",
		tray.UpdateDenied:          "declined",
		tray.UpdateApplied:         fmt.Sprintf("installed — running v%s", u.Version),
		tray.UpdateRolledBack:      "failed and rolled back",
	}[u.State]
	if stateText == "" {
		stateText = string(u.State)
	}
	sw.updateLabel.SetText(fmt.Sprintf("Update v%s: %s", u.Version, stateText))
}

// renderLog repaints the rolling log tail. The read-only edit is updated
// programmatically; the view is then scrolled to the last line. SetText
// (WM_SETTEXT) always resets the scroll position to the top, and EM_SETSEL /
// EM_SCROLLCARET do not reliably scroll a read-only multiline edit, so the
// scroll goes through EM_LINESCROLL, which does not depend on the caret or
// selection.
func (sw *statusWindow) renderLog(v tray.ViewState) {
	var b strings.Builder
	for _, line := range v.LogTail {
		fmt.Fprintf(&b, "%s  %s\r\n", line.Time.Format("15:04:05"), tray.ClipForDisplay(line.Text, tray.MaxLogLineLen))
	}
	text := b.String()
	sw.logEdit.SetText(text)
	lines := int(sw.logEdit.SendMessage(win.EM_GETLINECOUNT, 0, 0))
	sw.logEdit.SendMessage(win.EM_LINESCROLL, 0, uintptr(lines))
}

// runConsentDialog asks the user whether a server-pushed update may install.
// It runs modally over the status window on the UI thread and returns the
// decision. Must run on the UI thread.
func (a *app) runConsentDialog(version string) bool {
	owner := a.sw.mw
	dlg, err := walk.NewDialog(owner)
	if err != nil {
		log.Printf("Failed to create consent dialog, denying update: %v", err)
		return false
	}
	if err := dlg.SetTitle("SlimMedia Agent — Update"); err != nil {
		return false
	}
	if err := dlg.SetLayout(walk.NewVBoxLayout()); err != nil {
		return false
	}
	if err := dlg.SetSize(walk.Size{Width: 360, Height: 130}); err != nil {
		return false
	}

	th := currentTheme()
	applyDWMTheme(dlg.Handle(), th.isDark)
	dlg.SetBackground(th.bgBrush)
	dlg.WindowBase.SetBackground(th.bgBrush)

	label, err := walk.NewTextLabel(dlg)
	if err != nil {
		return false
	}
	label.SetTextColor(th.textPrimary)
	if err := label.SetText(fmt.Sprintf("Agent update to v%s is ready to install. The app will restart to apply it.", version)); err != nil {
		return false
	}

	btnRow, err := walk.NewComposite(dlg)
	if err != nil {
		return false
	}
	if err := btnRow.SetLayout(walk.NewHBoxLayout()); err != nil {
		return false
	}
	installBtn, err := walk.NewPushButton(btnRow)
	if err != nil {
		return false
	}
	installBtn.SetText("Install")
	setControlTheme(installBtn.Handle(), th.isDark)
	cancelBtn, err := walk.NewPushButton(btnRow)
	if err != nil {
		return false
	}
	cancelBtn.SetText("Cancel")
	setControlTheme(cancelBtn.Handle(), th.isDark)

	installBtn.Clicked().Attach(func() { dlg.Close(1) })
	cancelBtn.Clicked().Attach(func() { dlg.Close(0) })

	dlg.Starting().Attach(func() {
		applyControlThemes(dlg.Handle(), th.isDark)
	})

	// Track the open dialog so quit() can close it; an owned window would
	// otherwise outlive its owner and stall the exit path.
	a.consentDlg = dlg
	defer func() { a.consentDlg = nil }()

	dlg.Run()
	return dlg.Result() == 1
}

// titleStatus prettifies the raw daemon status for display: "WORKING" ->
// "Working". Unknown values pass through untouched.
func titleStatus(s string) string {
	switch strings.ToUpper(s) {
	case "IDLE":
		return "Idle"
	case "WORKING":
		return "Working"
	case "PAUSED":
		return "Paused"
	case "SCANNING":
		return "Scanning"
	default:
		return s
	}
}
