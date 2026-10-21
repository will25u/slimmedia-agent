//go:build windows

// Command tray is the SlimMedia Agent desktop app for Windows (ADR-009): a
// system-tray resident that embeds the full agent engine, shows a compact
// status window, and performs first-run pairing in a native GUI. There is no
// CLI and no web UI; a user installs one .exe and never sees a terminal.
//
// The tray icon and menu come from fyne.io/systray. ADR-009 names
// getlantern/systray, but that upstream is unmaintained and its Windows port
// has no left-click (tapped) handler; fyne.io/systray is the maintained fork
// with the same pure-Go, CGO-free guarantees, plus SetOnTapped for opening
// the status window from the icon.
//
// The binary builds as a GUI-subsystem PE (-ldflags -H=windowsgui); walk
// does not require WinMain, and the GUI subsystem ensures Windows never
// allocates a console window when launched from Explorer or shortcuts.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/systray"
	"github.com/lxn/walk"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"

	"github.com/will25u/slimmedia-agent/internal/daemon"
	"github.com/will25u/slimmedia-agent/internal/singleinstance"
	"github.com/will25u/slimmedia-agent/internal/tray"
	"github.com/will25u/slimmedia-agent/internal/updater"
	"github.com/will25u/slimmedia-agent/internal/logging"
)

// syncTarget is the surface uiBridge needs from a walk window: the native
// handle used for the wake-up post and the per-thread queue marshal.
type syncTarget interface {
	Handle() win.HWND
	Synchronize(func())
}

// uiBridge marshals work onto the walk UI thread. walk's WindowGroup queue
// is only drained when a message arrives at the owning thread's message loop
// (see windowgroup.go RunSynchronized and mainloop_default.go), so every
// queued func is followed by a WM_NULL post to wake GetMessage; an idle
// window would otherwise sit on queued updates until unrelated input.
type uiBridge struct {
	mu sync.RWMutex

	// hwnd is the handle of the window whose Synchronize queue is in use.
	// Both walk windows live on the main goroutine's thread and therefore
	// share one WindowGroup; hwnd only decides which queue gets the poke.
	hwnd win.HWND

	// syncFn is the current window's Synchronize method value.
	syncFn func(func())

	// pending buffers Invoke calls that arrive before the first window
	// exists (engine events can race window creation); they flush, in
	// order, when a target is installed.
	pending []func()
}

// setTarget installs the window that Invoke marshals against and flushes any
// buffered calls. It must be called on the UI thread.
func (b *uiBridge) setTarget(w syncTarget) {
	b.mu.Lock()
	b.hwnd = w.Handle()
	b.syncFn = w.Synchronize
	pending := b.pending
	b.pending = nil
	b.mu.Unlock()

	for _, f := range pending {
		b.syncFn(f)
	}
	win.PostMessage(b.hwnd, win.WM_NULL, 0, 0)
}

// Invoke runs f on the UI thread. It is safe from any goroutine. Calls made
// before any window exists are buffered and run once a target is set.
func (b *uiBridge) Invoke(f func()) {
	b.mu.RLock()
	hwnd, syncFn := b.hwnd, b.syncFn
	pending := b.pending
	b.mu.RUnlock()

	if syncFn == nil {
		b.mu.Lock()
		if b.syncFn == nil {
			b.pending = append(b.pending, f)
			b.mu.Unlock()
			return
		}
		hwnd, syncFn = b.hwnd, b.syncFn
		pending = b.pending
		b.pending = nil
		b.mu.Unlock()
		for _, pf := range pending {
			syncFn(pf)
		}
	}

	syncFn(f)
	win.PostMessage(hwnd, win.WM_NULL, 0, 0)
}

// app holds the shared state that ties the tray, the walk windows, and the
// daemon engine together. All walk values are only touched on the UI thread
// (main goroutine, or via uiBridge.Invoke); the daemon and systray callbacks
// run on their own goroutines.
type app struct {
	model  *tray.Model
	bridge *uiBridge

	configPath string

	// setupRC carries a successfully paired configuration from the setup
	// window to the engine start. It is written on the UI thread inside a
	// bridge.Invoke and read on the UI thread after the setup window's Run
	// returns, so it needs no lock.
	setupRC *daemon.ResolvedConfig

	// mu guards the fields below, which are written on the UI thread or
	// engine goroutines and read from either.
	mu             sync.Mutex
	d              *daemon.Daemon
	quitting       bool
	unpaired       bool
	unpairedReason string

	// sw and setup are the live windows; assigned only on the UI thread.
	sw    *statusWindow
	setup *setupWindow
	// menu is written by trayReady on the systray pump goroutine and read by
	// render on the UI thread; the atomic pointer provides the ordering.
	menu atomic.Pointer[trayMenuItems]

	// renderPending coalesces render requests: engine events can arrive
	// faster than the UI can paint, and every render reads the latest model
	// anyway, so at most one render may sit queued at a time.
	renderPending atomic.Bool

	// lastConn is the connection state the tray icon/menu were last painted
	// with; SetIcon and the menu title are skipped while it is unchanged.
	lastConn tray.ConnState
	// lastTooltip is the tooltip text last pushed to systray; unchanged
	// text is not re-pushed.
	lastTooltip string

	// consentDlg is the open update-consent dialog, if any. UI-thread only:
	// quit() closes it so a modal left open cannot block process exit.
	consentDlg *walk.Dialog

	appIcon walk.Image
}

func main() {
	// The hidden update-helper subcommand must win before any GUI work: a
	// self-update (agent/internal/updater/helper.go) re-execs THIS binary
	// with "update-helper" args to swap a staged build in for the locked
	// running exe, then relaunch it. Without this dispatch the swapped-in
	// tray would just start a second tray instance and the update would
	// never apply.
	if len(os.Args) > 1 && os.Args[1] == "update-helper" {
		if err := updater.RunHelper(os.Args[2:], os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "update-helper: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Hide the console window if one was allocated or attached, ensuring
	// the command prompt window never remains visible behind the tray app.
	if hwnd := win.GetConsoleWindow(); hwnd != 0 {
		win.ShowWindow(hwnd, win.SW_HIDE)
	}

	instanceLock, err := singleinstance.Acquire(daemon.StateDir())
	if errors.Is(err, singleinstance.ErrAlreadyRunning) {
		win.MessageBox(
			0,
			windows.StringToUTF16Ptr("SlimMedia Agent is already running"),
			windows.StringToUTF16Ptr("SlimMedia Agent"),
			win.MB_OK|win.MB_ICONINFORMATION,
		)
		os.Exit(0)
	}
	if err != nil {
		log.Printf("Single instance guard error: %v", err)
	} else {
		defer instanceLock.Close()
	}

	// Windows self-update relaunches this exe with argv ["run"]; the tray
	// app takes no command-line input at all, so other args are ignored.
	// (daemon update_apply_windows.go spawns the swap helper which re-execs
	// us; the engine reports the outcome on the next start.)

	logDir := filepath.Join(daemon.StateDir(), "logs")
	_, logCloser, err := logging.SetupLogger("slimmedia-agent-tray", logDir)
	if err != nil {
		log.Printf("Logging setup failed, continuing with defaults: %v", err)
	}

	initThemeSupport()
	ensureAutostart()

	var appIcon walk.Image
	if img, err := tray.MascotImage(); err == nil {
		if ico, err := walk.NewIconFromImage(img); err == nil {
			appIcon = ico
		} else {
			log.Printf("Failed to create walk app icon: %v", err)
		}
	} else {
		log.Printf("Failed to decode mascot image: %v", err)
	}

	a := &app{
		model:      tray.NewModel(daemon.Version),
		bridge:     &uiBridge{},
		configPath: filepath.Join(daemon.StateDir(), "config.json"),
		appIcon:    appIcon,
	}

	// systray creates its notify window AND pumps messages on the goroutine
	// that calls Run; LockOSThread pins that goroutine to one OS thread so
	// creation and pumping cannot be split across threads by the scheduler.
	go func() {
		runtime.LockOSThread()
		systray.Run(a.trayReady, func() {})
	}()

	for !a.isQuitting() {
		paired := false
		if cfg, err := daemon.LoadConfig(a.configPath); err == nil && cfg.APIKey != "" {
			paired = true
		}

		if paired && !a.isUnpaired() {
			// Configured: status window immediately in "starting…" state; the
			// engine starts on a background goroutine. The bridge target is
			// installed before startEngine so consent/updates always have a
			// live window to marshal against.
			a.runStatusWindow(nil)
		} else {
			a.setUnpaired(false)
			a.setupRC = nil
			if rc := a.runSetupWindow(); rc != nil {
				a.runStatusWindow(rc)
			} else {
				break
			}
		}
	}
	// Reaching here means a window was really closed (quit): user closes
	// only hide to tray, they never unwind Run.

	a.finalize()
	if logCloser != nil {
		// os.Exit skips deferred calls, so close the log sink explicitly.
		logCloser.Close()
	}
	os.Exit(0)
}

// runStatusWindow creates the status window on the UI thread, points the
// bridge at it, starts the engine on a background goroutine (a nil rc means
// the saved config already has an API key and ResolveConfig will find it),
// and pumps messages until the window is truly closed (quit). It never
// returns for the hide-to-tray close path.
func (a *app) runStatusWindow(rc *daemon.ResolvedConfig) {
	name := ""
	server := ""
	if rc != nil {
		name = rc.Name
		server = rc.Server
	} else if cfg, err := daemon.LoadConfig(a.configPath); err == nil {
		name = cfg.Name
		server = cfg.Server
	}
	a.model.ApplyStatus(daemon.Status{
		State:       daemon.StateStarting,
		AgentStatus: "Starting",
		Name:        name,
		Server:      server,
	})

	sw, err := newStatusWindow(a)
	if err != nil {
		log.Fatalf("Failed to create status window: %v", err)
	}
	a.sw = sw
	a.bridge.setTarget(sw.mw)
	// walk's Run only pumps messages; without Show the window exists but
	// is never visible and this call blocks forever.
	sw.show()
	go a.startEngine(rc)
	sw.mw.Run()
	a.sw = nil
}

// runSetupWindow creates the setup window and pumps until pairing succeeds
// (returns the resolved config) or the app is quitting (returns nil). The
// user closing the window only hides it to the tray.
func (a *app) runSetupWindow() *daemon.ResolvedConfig {
	su, err := newSetupWindow(a)
	if err != nil {
		log.Fatalf("Failed to create setup window: %v", err)
	}
	a.setup = su
	a.bridge.setTarget(su.mw)
	a.mu.Lock()
	reason := a.unpairedReason
	a.unpairedReason = ""
	a.mu.Unlock()
	if reason != "" {
		su.showError(reason)
	}
	// Run only pumps the message loop; Show makes the window visible.
	su.show()
	su.mw.Run()
	a.setup = nil
	return a.setupRC
}

// showMainWindow reveals whichever window is meaningful: the status window
// once the engine exists, otherwise the setup window again. Must run on the
// UI thread.
func (a *app) showMainWindow() {
	switch {
	case a.sw != nil:
		a.sw.show()
	case a.setup != nil:
		a.setup.show()
	}
}

// startEngine resolves the configuration (rc may already carry a fresh
// pairing key), starts the daemon, and wires up event consumption, the
// status refresh ticker, and the update-consent handler. It runs on its own
// goroutine so the UI never blocks on engine startup.
func (a *app) startEngine(rc *daemon.ResolvedConfig) {
	var err error
	if rc == nil {
		// The consent handler travels in the config so the daemon registers
		// it before it connects: an AGENT_UPDATE dispatched on connect must
		// never see a nil handler and skip the consent gate.
		rc, err = daemon.ResolveConfig(daemon.Config{
			UpdateConsentHandler: a.consentHandler,
			UIAttached:           true,
		})
		if err != nil {
			a.engineFailed(err)
			return
		}
	} else {
		rc.UIAttached = true
	}

	d, err := daemon.Start(context.Background(), rc)
	if err != nil {
		a.engineFailed(err)
		return
	}

	a.mu.Lock()
	a.d = d
	a.mu.Unlock()

	// The consent callback runs on a daemon goroutine and must block until
	// the user decides; it is marshaled onto the UI thread and waits.
	d.SetUpdateConsentHandler(a.consentHandler)

	a.model.ApplyStatus(d.Status())
	a.requestRender()

	go a.consumeEvents(d)
	go a.refreshStatus(d)
}

// engineFailed surfaces a daemon startup failure in the status window (the
// model has no settable error field, so the window gets it directly) and in
// the tray tooltip.
func (a *app) engineFailed(err error) {
	log.Printf("Agent engine failed to start: %v", err)
	a.bridge.Invoke(func() {
		if a.sw != nil {
			a.sw.setStartupError(err)
		}
	})
	systray.SetTooltip(fmt.Sprintf("%s — error: %s", trayName(a.model.View()), err))
}

// consumeEvents folds the daemon event stream into the model and requests a
// coalesced re-render after every event. The channel closes on Shutdown,
// which ends this goroutine.
func (a *app) consumeEvents(d *daemon.Daemon) {
	for ev := range d.Events() {
		a.model.ApplyEvent(ev)
		a.requestRender()
		if ev.Type == daemon.EventUnpaired || ev.Status == "UNPAIRED" {
			reason := ev.Detail
			if reason == "" {
				reason = "Agent deleted by server"
			}
			a.handleUnpaired(reason)
			return
		}
	}
}

// isUnpaired reports whether the daemon signaled that it has been unpaired/deleted.
func (a *app) isUnpaired() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.unpaired
}

// setUnpaired mutates the unpaired flag.
func (a *app) setUnpaired(val bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.unpaired = val
}

// handleUnpaired drops the current status view and triggers recovery back to the setup view.
func (a *app) handleUnpaired(reason string) {
	a.mu.Lock()
	a.unpaired = true
	a.unpairedReason = reason
	d := a.d
	a.d = nil
	a.mu.Unlock()

	if d != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = d.Shutdown(ctx)
		}()
	}

	a.bridge.Invoke(func() {
		if a.sw != nil {
			a.sw.mw.Close()
		}
	})
}

// refreshStatus keeps ActiveJobs and the connection state fresh even when no
// events flow; a one-second ticker re-applies the status snapshot.
func (a *app) refreshStatus(d *daemon.Daemon) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			st := d.Status()
			a.model.ApplyStatus(st)
			a.requestRender()
			if st.State == daemon.StateStopped || st.State == daemon.StateUnpaired {
				return
			}
		}
	}
}

// requestRender schedules a single coalesced render on the UI thread. Events
// arriving while a render is already queued are folded into that render,
// which reads the model at execution time.
func (a *app) requestRender() {
	if !a.renderPending.CompareAndSwap(false, true) {
		return
	}
	a.bridge.Invoke(func() {
		a.renderPending.Store(false)
		a.render()
	})
}

// consentHandler decides a server-pushed AGENT_UPDATE. It must show the
// status window and ask the user, so the decision is marshaled onto the UI
// thread and this goroutine blocks until the dialog closes.
func (a *app) consentHandler(version string) bool {
	ch := make(chan bool, 1)
	a.bridge.Invoke(func() {
		if a.sw != nil {
			a.sw.show()
		}
		ch <- a.runConsentDialog(version)
	})
	return <-ch
}

// quit begins the shutdown sequence: mark quitting so Closing handlers allow
// the windows to close, stop the engine with a bounded drain, then close the
// windows and the tray. Runs on the UI thread (invoked from the tray menu).
func (a *app) quit() {
	a.mu.Lock()
	if a.quitting {
		a.mu.Unlock()
		return
	}
	a.quitting = true
	a.mu.Unlock()

	systray.SetTooltip("SlimMedia Agent — stopping…")
	if a.sw != nil {
		a.sw.setStopping()
	}
	// An orphaned consent modal would keep its nested dialog loop alive and
	// delay process exit; closing it also answers the pending consent
	// handler (deny), unwinding the blocked update goroutine. quit runs on
	// the UI thread, so touching the dialog here is safe.
	if a.consentDlg != nil {
		a.consentDlg.Close(0)
	}

	// Shutdown can block on draining transcode jobs; keep the UI and the
	// systray pump thread free while it runs.
	go func() {
		a.mu.Lock()
		d := a.d
		a.mu.Unlock()
		if d != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := d.Shutdown(ctx); err != nil {
				log.Printf("Agent shutdown incomplete: %v", err)
			}
		}
		a.bridge.Invoke(func() {
			if a.sw != nil {
				a.sw.mw.Close()
			}
			if a.setup != nil {
				a.setup.mw.Close()
			}
			systray.Quit()
		})
	}()
}

// isQuitting reports whether the app is shutting down for real.
func (a *app) isQuitting() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.quitting
}

// finalize runs after the last window's message loop has ended. Shutdown is
// idempotent, so this is a no-op when quit() already drained the engine.
func (a *app) finalize() {
	a.mu.Lock()
	d := a.d
	a.mu.Unlock()
	if d != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := d.Shutdown(ctx); err != nil {
			log.Printf("Agent shutdown incomplete: %v", err)
		}
	}
	systray.Quit()
}

// render refreshes every UI surface from the current model view. It always
// runs on the UI thread (via uiBridge.Invoke). The tray icon and menu title
// change only with the connection state, and the tooltip only when its text
// changes; unchanged values are not re-pushed to systray.
func (a *app) render() {
	v := a.model.View()
	if a.sw != nil {
		a.sw.render(v)
	}
	if v.Conn != a.lastConn {
		a.lastConn = v.Conn
		systray.SetIcon(tray.StatusIcon(v.Conn))
		a.traySetState(v.Conn)
	}
	tip := fmt.Sprintf("%s — %s", trayName(v), tooltipText(v))
	if tip != a.lastTooltip {
		a.lastTooltip = tip
		systray.SetTooltip(tip)
	}
}

// trayName returns the agent name for tooltips, with a sane pre-pairing
// fallback (the model's name is empty until the first status snapshot).
func trayName(v tray.ViewState) string {
	if v.Name != "" {
		return v.Name
	}
	return "SlimMedia Agent"
}

// tooltipText condenses the view state into one tray-tooltip line. A pending
// consent request outranks everything: the update is blocked on the user.
func tooltipText(v tray.ViewState) string {
	if v.Update != nil && v.Update.State == tray.UpdateAwaitingConsent {
		return "update available v" + v.Update.Version
	}
	if v.Job != nil && !v.Job.Done {
		return fmt.Sprintf("working: %.0f%%", v.Job.ProgressPct)
	}
	switch v.Conn {
	case tray.ConnConnected:
		return "connected"
	case tray.ConnReconnecting:
		return "reconnecting…"
	case tray.ConnOffline:
		return "offline"
	default:
		return "starting…"
	}
}
