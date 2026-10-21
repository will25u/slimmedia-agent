//go:build windows

package main

import (
	"log"
	"os/exec"
	"strings"

	"fyne.io/systray"

	"github.com/will25u/slimmedia-agent/internal/daemon"
	"github.com/will25u/slimmedia-agent/internal/tray"
)

// trayMenuItems keeps the tray menu handles that render() and the menu
// callbacks need to touch after onReady returns.
type trayMenuItems struct {
	title     *systray.MenuItem
	autostart *systray.MenuItem
}

// traySetState refreshes the disabled title item in the tray menu. It is
// called from render() on the UI thread; systray menu APIs are goroutine-
// safe, so any thread may call them.
func (a *app) traySetState(conn tray.ConnState) {
	m := a.menu.Load()
	if m == nil {
		return
	}
	text := map[tray.ConnState]string{
		tray.ConnStarting:     "SlimMedia Agent — starting…",
		tray.ConnConnected:    "SlimMedia Agent — connected",
		tray.ConnReconnecting: "SlimMedia Agent — reconnecting…",
		tray.ConnOffline:      "SlimMedia Agent — offline",
	}[conn]
	if text == "" {
		text = "SlimMedia Agent"
	}
	m.title.SetTitle(text)
}

func (a *app) licensesURL() string {
	srv := ""
	if a.model != nil {
		srv = a.model.View().Server
	}
	if srv == "" {
		if cfg, err := daemon.LoadConfig(a.configPath); err == nil && cfg.Server != "" {
			srv = cfg.Server
		}
	}
	srv = strings.TrimSpace(strings.TrimRight(srv, "/"))
	if !strings.HasPrefix(srv, "http://") && !strings.HasPrefix(srv, "https://") {
		return "https://slimmedia.app/licenses"
	}
	return srv + "/licenses"
}

// trayReady builds the tray icon and menu. systray runs it on the dedicated
// locked goroutine; every callback that needs the UI marshals through the
// bridge instead of touching walk widgets directly.
func (a *app) trayReady() {
	systray.SetIcon(tray.StatusIcon(tray.ConnStarting))
	systray.SetTitle("SlimMedia Agent")
	systray.SetTooltip("SlimMedia Agent — starting…")

	title := systray.AddMenuItem("SlimMedia Agent — starting…", "")
	title.Disable()
	open := systray.AddMenuItem("Open status window", "Show the SlimMedia Agent window")
	systray.AddSeparator()
	licenses := systray.AddMenuItem("Open Source Licenses", "View GPLv3 and open-source licenses")
	systray.AddSeparator()
	autostart := systray.AddMenuItemCheckbox("Start on login", "Launch SlimMedia Agent when you sign in", autostartEnabled())
	systray.AddSeparator()
	quit := systray.AddMenuItem("Stop agent && quit", "Stop the agent and exit SlimMedia Agent")

	a.menu.Store(&trayMenuItems{title: title, autostart: autostart})

	// Left-click on the tray icon opens the window, like the menu item.
	systray.SetOnTapped(func() {
		a.bridge.Invoke(func() { a.showMainWindow() })
	})

	// Menu item callbacks arrive on the systray pump goroutine. Each
	// handler loops on its ClickedCh because a clicked checkbox stays
	// clickable; systray delivers one send per click.
	go func() {
		for range open.ClickedCh {
			a.bridge.Invoke(func() { a.showMainWindow() })
		}
	}()

	go func() {
		for range licenses.ClickedCh {
			url := a.licensesURL()
			go func(target string) {
				_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
			}(url)
		}
	}()

	go func() {
		for range autostart.ClickedCh {
			enabled := autostart.Checked()
			// COM is STA: each toggle runs the call on a fresh goroutine
			// that CoInitializes itself, never on the systray pump thread.
			go func() {
				var err error
				if enabled {
					err = enableAutostart()
				} else {
					err = disableAutostart()
				}
				if err != nil {
					log.Printf("Autostart toggle failed: %v", err)
				}
				// Reflect reality (the toggle may have failed): recheck
				// the box from the filesystem state.
				if autostartEnabled() {
					autostart.Check()
				} else {
					autostart.Uncheck()
				}
			}()
		}
	}()

	go func() {
		for range quit.ClickedCh {
			a.bridge.Invoke(a.quit)
		}
	}()
}
