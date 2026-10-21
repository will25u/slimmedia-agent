//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lxn/walk"
	"github.com/lxn/win"

	"github.com/will25u/slimmedia-agent/internal/daemon"
)

// setupWindow is the first-run pairing dialog, shown when no saved API key
// exists. Closing it hides to the tray (the user can reopen it from the tray
// menu or quit); it is disposed only on pairing success or on app quit.
type setupWindow struct {
	app *app
	mw  *walk.MainWindow
	th  *themeColors

	serverEdit  *walk.LineEdit
	nameEdit    *walk.LineEdit
	codeEdit    *walk.LineEdit
	dirEdit     *walk.LineEdit
	tempDirEdit *walk.LineEdit
	errLabel    *walk.TextLabel
	startBtn    *walk.PushButton
}

// newSetupWindow builds the setup window on the UI thread. walk lays
// children out in creation order, so each captioned field is a caption label
// followed by its editor.
func newSetupWindow(a *app) (*setupWindow, error) {
	mw, err := walk.NewMainWindow()
	if err != nil {
		return nil, err
	}
	th := currentTheme()
	su := &setupWindow{app: a, mw: mw, th: th}
	if err := mw.SetTitle("SlimMedia Agent — Setup"); err != nil {
		return nil, err
	}
	if a.appIcon != nil {
		_ = mw.SetIcon(a.appIcon)
	}

	applyDWMTheme(mw.Handle(), th.isDark)
	mw.SetBackground(th.bgBrush)
	mw.WindowBase.SetBackground(th.bgBrush)

	baseFont, _ := walk.NewFont("Segoe UI", 9, 0)
	titleFont, _ := walk.NewFont("Segoe UI", 12, walk.FontBold)
	subtitleFont, _ := walk.NewFont("Segoe UI", 9, 0)
	boldFont, _ := walk.NewFont("Segoe UI", 9, walk.FontBold)

	if baseFont != nil {
		mw.SetFont(baseFont)
	}

	mainLayout := walk.NewVBoxLayout()
	_ = mainLayout.SetMargins(walk.Margins{HNear: 20, VNear: 16, HFar: 20, VFar: 16})
	_ = mainLayout.SetSpacing(12)
	if err := mw.SetLayout(mainLayout); err != nil {
		return nil, err
	}

	size := walk.Size{Width: 480, Height: 580}
	if err := mw.SetMinMaxSize(size, walk.Size{}); err != nil {
		return nil, err
	}
	if err := mw.SetSize(size); err != nil {
		return nil, err
	}

	titleLbl, err := walk.NewTextLabel(mw)
	if err != nil {
		return nil, err
	}
	if titleFont != nil {
		titleLbl.SetFont(titleFont)
	}
	titleLbl.SetTextColor(th.textPrimary)
	_ = titleLbl.SetText("Welcome to SlimMedia Agent")

	subLbl, err := walk.NewTextLabel(mw)
	if err != nil {
		return nil, err
	}
	if subtitleFont != nil {
		subLbl.SetFont(subtitleFont)
	}
	subLbl.SetTextColor(th.textMuted)
	_ = subLbl.SetText("Pair this machine with your SlimMedia server")

	if su.serverEdit, err = su.addField("Server URL"); err != nil {
		return nil, err
	}
	_ = su.serverEdit.SetText("https://slimmedia.app")
	su.serverEdit.SetBackground(th.editBgBrush)
	su.serverEdit.SetTextColor(th.textMuted)
	setControlTheme(su.serverEdit.Handle(), th.isDark)
	if err := su.serverEdit.SetReadOnly(true); err != nil {
		return nil, err
	}

	if su.nameEdit, err = su.addField("Agent name"); err != nil {
		return nil, err
	}
	hostname, _ := os.Hostname()
	_ = su.nameEdit.SetText(hostname)

	if su.codeEdit, err = su.addField("Pairing code"); err != nil {
		return nil, err
	}

	dirGroup, _, err := su.addFormGroup("Media directory")
	if err != nil {
		return nil, err
	}
	dirRow, err := walk.NewComposite(dirGroup)
	if err != nil {
		return nil, err
	}
	dirRowLayout := walk.NewHBoxLayout()
	_ = dirRowLayout.SetMargins(walk.Margins{})
	_ = dirRowLayout.SetSpacing(8)
	if err := dirRow.SetLayout(dirRowLayout); err != nil {
		return nil, err
	}
	if su.dirEdit, err = walk.NewLineEdit(dirRow); err != nil {
		return nil, err
	}
	_ = su.dirEdit.SetMinMaxSize(walk.Size{Height: 30}, walk.Size{Height: 30})
	su.dirEdit.SetBackground(th.editBgBrush)
	su.dirEdit.SetTextColor(th.textPrimary)
	setControlTheme(su.dirEdit.Handle(), th.isDark)
	browseBtn, err := walk.NewPushButton(dirRow)
	if err != nil {
		return nil, err
	}
	_ = browseBtn.SetMinMaxSize(walk.Size{Width: 90, Height: 30}, walk.Size{Width: 90, Height: 30})
	browseBtn.SetText("Browse…")
	setControlTheme(browseBtn.Handle(), th.isDark)
	browseBtn.Clicked().Attach(func() {
		dlg := &walk.FileDialog{Title: "Choose your media library folder"}
		ok, err := dlg.ShowBrowseFolder(mw)
		switch {
		case err != nil:
			su.showError(fmt.Sprintf("Folder picker failed: %v", err))
		case ok:
			su.dirEdit.SetText(dlg.FilePath)
		}
	})

	tempGroup, _, err := su.addFormGroup("Transcode temp directory (optional)")
	if err != nil {
		return nil, err
	}
	defaultTempLbl, err := walk.NewTextLabel(tempGroup)
	if err != nil {
		return nil, err
	}
	if subtitleFont != nil {
		defaultTempLbl.SetFont(subtitleFont)
	}
	defaultTempLbl.SetTextColor(th.textMuted)
	_ = defaultTempLbl.SetText("Default: " + filepath.Join(daemon.StateDir(), "scratch"))

	tempDirRow, err := walk.NewComposite(tempGroup)
	if err != nil {
		return nil, err
	}
	tempDirRowLayout := walk.NewHBoxLayout()
	_ = tempDirRowLayout.SetMargins(walk.Margins{})
	_ = tempDirRowLayout.SetSpacing(8)
	if err := tempDirRow.SetLayout(tempDirRowLayout); err != nil {
		return nil, err
	}
	if su.tempDirEdit, err = walk.NewLineEdit(tempDirRow); err != nil {
		return nil, err
	}
	_ = su.tempDirEdit.SetMinMaxSize(walk.Size{Height: 30}, walk.Size{Height: 30})
	su.tempDirEdit.SetBackground(th.editBgBrush)
	su.tempDirEdit.SetTextColor(th.textPrimary)
	setControlTheme(su.tempDirEdit.Handle(), th.isDark)
	tempBrowseBtn, err := walk.NewPushButton(tempDirRow)
	if err != nil {
		return nil, err
	}
	_ = tempBrowseBtn.SetMinMaxSize(walk.Size{Width: 90, Height: 30}, walk.Size{Width: 90, Height: 30})
	tempBrowseBtn.SetText("Browse…")
	setControlTheme(tempBrowseBtn.Handle(), th.isDark)
	tempBrowseBtn.Clicked().Attach(func() {
		dlg := &walk.FileDialog{Title: "Choose your transcode temp folder"}
		ok, err := dlg.ShowBrowseFolder(mw)
		switch {
		case err != nil:
			su.showError(fmt.Sprintf("Folder picker failed: %v", err))
		case ok:
			su.tempDirEdit.SetText(dlg.FilePath)
		}
	})

	if su.errLabel, err = walk.NewTextLabel(mw); err != nil {
		return nil, err
	}
	su.errLabel.SetTextColor(th.textError)

	btnRow, err := walk.NewComposite(mw)
	if err != nil {
		return nil, err
	}
	btnRowLayout := walk.NewHBoxLayout()
	_ = btnRowLayout.SetMargins(walk.Margins{VNear: 8})
	_ = btnRowLayout.SetSpacing(0)
	if err := btnRow.SetLayout(btnRowLayout); err != nil {
		return nil, err
	}
	if _, err := walk.NewHSpacer(btnRow); err != nil {
		return nil, err
	}
	if su.startBtn, err = walk.NewPushButton(btnRow); err != nil {
		return nil, err
	}
	su.startBtn.SetText("Pair && Start")
	_ = su.startBtn.SetMinMaxSize(walk.Size{Width: 140, Height: 34}, walk.Size{Width: 140, Height: 34})
	if boldFont != nil {
		su.startBtn.SetFont(boldFont)
	}
	setControlTheme(su.startBtn.Handle(), th.isDark)
	win.SendMessage(su.startBtn.Handle(), win.BM_SETSTYLE, uintptr(win.BS_DEFPUSHBUTTON), 1)
	su.startBtn.Clicked().Attach(su.startPairing)

	mw.Starting().Attach(func() {
		applyControlThemes(mw.Handle(), th.isDark)
		if su.codeEdit != nil {
			su.codeEdit.SetFocus()
		}
	})

	mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		if a.isQuitting() || a.setupRC != nil {
			// Real close: quitting, or pairing succeeded and main() is
			// moving on to the status window.
			return
		}
		// Any other close request hides to tray; the engine may not even be
		// running yet, so there is nothing to stop.
		*canceled = true
		mw.Hide()
	})

	return su, nil
}

// addFormGroup creates a vertical composite with standard spacing between caption and input.
func (su *setupWindow) addFormGroup(caption string) (*walk.Composite, *walk.TextLabel, error) {
	group, err := walk.NewComposite(su.mw)
	if err != nil {
		return nil, nil, err
	}
	layout := walk.NewVBoxLayout()
	_ = layout.SetMargins(walk.Margins{HNear: 0, VNear: 0, HFar: 0, VFar: 0})
	_ = layout.SetSpacing(8)
	if err := group.SetLayout(layout); err != nil {
		return nil, nil, err
	}
	lbl, err := walk.NewTextLabel(group)
	if err != nil {
		return nil, nil, err
	}
	lbl.SetTextColor(su.th.textPrimary)
	if err := lbl.SetText(caption); err != nil {
		return nil, nil, err
	}
	return group, lbl, nil
}

// addField appends a captioned single-line editor with modern dimensions and returns it.
func (su *setupWindow) addField(caption string) (*walk.LineEdit, error) {
	group, _, err := su.addFormGroup(caption)
	if err != nil {
		return nil, err
	}
	edit, err := walk.NewLineEdit(group)
	if err != nil {
		return nil, err
	}
	_ = edit.SetMinMaxSize(walk.Size{Height: 30}, walk.Size{Height: 30})
	edit.SetBackground(su.th.editBgBrush)
	edit.SetTextColor(su.th.textPrimary)
	setControlTheme(edit.Handle(), su.th.isDark)
	return edit, nil
}

// show brings the setup window to the front and sets focus on the pairing code. Must run on the UI thread.
func (su *setupWindow) show() {
	su.mw.Show()
	su.mw.Activate()
	if su.codeEdit != nil {
		su.codeEdit.SetFocus()
	}
}

// showError displays a message in the window's red error line.
func (su *setupWindow) showError(msg string) {
	su.errLabel.SetText(msg)
}

// startPairing validates the fields and, when valid, runs pairing on a
// background goroutine so the UI stays responsive. The pairing code is a
// credential: it is handed to the daemon but never logged or displayed.
func (su *setupWindow) startPairing() {
	server := strings.TrimSpace(su.serverEdit.Text())
	if server == "" {
		server = "https://slimmedia.app"
	}
	name := strings.TrimSpace(su.nameEdit.Text())
	code := strings.TrimSpace(su.codeEdit.Text())
	dir := strings.TrimSpace(su.dirEdit.Text())
	tempDir := strings.TrimSpace(su.tempDirEdit.Text())

	switch {
	case server == "":
		su.showError("Server URL is required.")
		return
	case code == "":
		su.showError("Pairing code is required.")
		return
	case dir == "":
		su.showError("Media directory is required.")
		return
	}

	su.errLabel.SetText("")
	su.startBtn.SetEnabled(false)
	su.startBtn.SetText("Pairing…")

	appRef := su.app
	go func() {
		rc, err := daemon.ResolveConfig(daemon.Config{
			Server:      server,
			Name:        name,
			MediaDir:    dir,
			TempDir:     tempDir,
			PairingCode: code,
		})
		if err == nil {
			var key string
			key, err = daemon.Pair(rc.Server, code, rc.Name, rc.ConfigPath, rc.MediaDir, tempDir)
			if err == nil {
				rc.APIKey = key
				rc.MediaDir = dir
				rc.TempDir = tempDir
				rc.UpdateConsentHandler = appRef.consentHandler
				rc.UIAttached = true
				if loadedCfg, err := daemon.LoadConfig(rc.ConfigPath); err == nil && loadedCfg != nil {
					rc.LibraryID = loadedCfg.LibraryID
					rc.Libraries = loadedCfg.Libraries
				}
			}
		}
		appRef.bridge.Invoke(func() {
			if err != nil {
				su.startBtn.SetEnabled(true)
				su.startBtn.SetText("Pair && Start")
				su.showError(fmt.Sprintf("Pairing failed: %v", err))
				return
			}
			// Success: hand the config to main() and dispose for real.
			appRef.setupRC = rc
			su.mw.Close()
		})
	}()
}
