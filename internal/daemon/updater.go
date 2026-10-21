package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// UpdaterDeps are the Updater's injected dependencies. The callbacks deliver
// the flow's outbound traffic to whoever owns the channels: Send and
// SetScheduled to the websocket client (nil disables the status envelopes and
// the scheduled flag), Emit to the daemon's event stream for local consumers
// (the tray, #79). ActiveJobs is the job-activity probe that gates the
// restart on a clean transcode stop.
type UpdaterDeps struct {
	// Send delivers an agent.update.status envelope to the server. nil
	// drops statuses.
	Send func(protocol.Envelope)
	// SetScheduled mirrors the update-scheduled flag onto the transport.
	// nil disables the mirror.
	SetScheduled func(bool)
	// APIURL is the resolved server URL the update binary is fetched from.
	APIURL string
	// ActiveJobs reports how many transcode jobs are in flight; the
	// update waits for a clean stop before restarting the agent.
	ActiveJobs func() int64
	// Emit mirrors update lifecycle events onto the daemon event stream.
	// nil drops events.
	Emit func(Event)
}

// Updater owns the server-dispatched self-update flow: duplicate refusal,
// cancellation, the consent gate, the job-activity wait, the
// download/verify/swap, and the status envelopes reported to the dashboard.
// Dependencies are injected at construction; consent may be registered later
// (and re-registered) via SetConsentHandler.
type Updater struct {
	send         func(protocol.Envelope)
	setScheduled func(bool)
	apiURL       string
	activeJobs   func() int64
	emit         func(Event)

	// isUpdating refuses duplicate AGENT_UPDATE commands and defers
	// incoming transcode jobs for the duration of the flow.
	isUpdating atomic.Bool

	// cancelMu guards cancel, the in-flight flow's cancellation func.
	cancelMu sync.Mutex
	cancel   context.CancelFunc

	// consentMu guards consent, the optional local consent gate. When
	// non-nil (the Windows tray app registered a handler) and the update
	// payload does not carry Auto, the flow pauses after
	// update_consent_requested until the user decides; nil or an Auto
	// payload means the dashboard's update push is both the request and the
	// grant.
	consentMu sync.Mutex
	consent   func(version string) bool
}

// NewUpdater constructs an updater with its dependencies injected. The
// returned updater has no consent handler; register one with
// SetConsentHandler before dispatching commands if the local user must
// approve updates.
func NewUpdater(deps UpdaterDeps) *Updater {
	if deps.ActiveJobs == nil {
		panic("NewUpdater: ActiveJobs is nil")
	}
	return &Updater{
		send:         deps.Send,
		setScheduled: deps.SetScheduled,
		apiURL:       deps.APIURL,
		activeJobs:   deps.ActiveJobs,
		emit:         deps.Emit,
	}
}

// SetConsentHandler registers the callback that decides whether a
// server-pushed AGENT_UPDATE may proceed. When non-nil, the update flow emits
// EventUpdateConsentRequested and blocks on the handler's decision before
// downloading anything; a false return denies the update and emits
// EventUpdateConsentDenied. Payloads with Auto set bypass the handler: the
// operator already consented on the dashboard, so the update installs without
// a local prompt. When nil (the CLI), the dashboard's update push is both the
// request and the grant, preserving the pre-tray behavior.
//
// The handler is called from the update goroutine and must be safe to call
// concurrently with other daemon operations; the tray marshals the decision
// onto its UI thread and blocks until the user answers.
func (u *Updater) SetConsentHandler(h func(version string) bool) {
	u.consentMu.Lock()
	u.consent = h
	u.consentMu.Unlock()
}

// consentHandler returns the registered consent handler, or nil.
func (u *Updater) consentHandler() func(version string) bool {
	u.consentMu.Lock()
	defer u.consentMu.Unlock()
	return u.consent
}

// Updating reports whether an agent update is scheduled or in progress; the
// job supervisor defers job starts while it returns true.
func (u *Updater) Updating() bool {
	return u.isUpdating.Load()
}

// StartUpdate handles an AGENT_UPDATE command already decoded by the command
// dispatcher: it refuses duplicate updates, marks the update scheduled on the
// websocket client, and runs the download/verify/swap flow in the background,
// reporting through the event stream.
func (u *Updater) StartUpdate(payload protocol.AgentUpdatePayload) {
	if !u.isUpdating.CompareAndSwap(false, true) {
		log.Printf("Agent update is already scheduled or in progress; ignoring duplicate command")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	u.cancelMu.Lock()
	u.cancel = cancel
	u.cancelMu.Unlock()
	u.markScheduled(true)
	go func() {
		defer u.isUpdating.Store(false)
		defer u.markScheduled(false)
		defer func() {
			u.cancelMu.Lock()
			u.cancel = nil
			u.cancelMu.Unlock()
		}()
		u.run(ctx, payload)
	}()
}

// CancelUpdate handles a CANCEL_AGENT_UPDATE command: it cancels the
// in-flight update context (if any) and clears the scheduled flag.
func (u *Updater) CancelUpdate() {
	u.cancelMu.Lock()
	if u.cancel != nil {
		u.cancel()
		u.cancel = nil
	}
	u.cancelMu.Unlock()
	u.markScheduled(false)
	log.Printf("Received CANCEL_AGENT_UPDATE; cancelled pending update")
}

// run processes a server-dispatched AGENT_UPDATE command: download the new
// binary, verify it, replace the running executable, and re-exec into the new
// binary (Unix) or hand off to the swap helper (Windows). Progress is
// reported to the dashboard as agent.update.status envelopes (relayed to
// /ws/browser by the server), and update lifecycle events flow to local
// consumers (the tray, #79) over the Emit callback.
//
// Payloads with Auto set (the operator clicked Update on the dashboard) skip
// the local consent prompt even when a handler is registered — the dashboard
// click is the consenting action. Otherwise, when consent is non-nil (the
// Windows tray app registered a consent handler), the flow pauses after
// update_consent_requested until the user decides; denial aborts before
// download. When consent is nil (CLI), the dashboard's update push is both
// the request and the grant, and consent_granted is emitted as the download
// starts.
//
// The update is refused while a transcode job is active: killing the process
// mid-transcode would leave the state machine to recover the job from its
// journal, but finishing the in-flight file first is cheaper and less
// surprising, and the user can retry from the dashboard once the agent is
// idle.
func (u *Updater) run(ctx context.Context, payload protocol.AgentUpdatePayload) {
	u.consentMu.Lock()
	consent := u.consent
	u.consentMu.Unlock()

	if payload.Version != "" && !isNewerVersion(payload.Version, Version) {
		log.Printf("Update to v%s ignored: already running v%s", payload.Version, Version)
		return
	}

	u.emitEvent(Event{Type: EventUpdateAvailable, Message: payload.Version, Detail: "server offered agent v" + payload.Version})

	// Consent gate. The dashboard's update push is the operator's consent,
	// so Auto payloads skip the local prompt even when a consent handler is
	// registered (the tray app). Otherwise, with a registered handler the
	// update waits for the local user's decision before anything is
	// downloaded; without one (CLI) the grant travels with the flow below.
	// The wait must be cancellable: CANCEL_AGENT_UPDATE and shutdown both
	// cancel ctx, and a user who walks away from the prompt must not leave
	// the update half-pending (isUpdating stuck set, jobs deferred) forever.
	promptForConsent := consent != nil && !payload.Auto
	if promptForConsent {
		u.emitEvent(Event{Type: EventUpdateConsentRequested, Message: payload.Version})
		consentCh := make(chan bool, 1)
		go func() { consentCh <- consent(payload.Version) }()
		select {
		case granted := <-consentCh:
			if !granted {
				log.Printf("Update to v%s denied by user", payload.Version)
				u.sendStatus(payload, protocol.UpdateStatusFailed, "update declined by user on this agent")
				u.emitEvent(Event{Type: EventUpdateConsentDenied, Message: payload.Version, Detail: "user declined the update"})
				return
			}
			u.emitEvent(Event{Type: EventUpdateConsentGranted, Message: payload.Version, Detail: "consent granted via tray prompt"})
		case <-ctx.Done():
			log.Printf("Update to v%s cancelled while awaiting user consent", payload.Version)
			return
		}
	}

	if u.activeJobs() > 0 {
		log.Printf("Update to v%s scheduled: waiting for current transcode to complete", payload.Version)
		u.sendStatus(payload, protocol.UpdateStatusScheduled, "Update scheduled: Agent will update as soon as the current transcode is complete.")
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for u.activeJobs() > 0 {
			select {
			case <-ctx.Done():
				log.Printf("Scheduled update to v%s cancelled", payload.Version)
				return
			case <-ticker.C:
			}
		}
		log.Printf("Current transcode completed; proceeding with update to v%s", payload.Version)
	}

	select {
	case <-ctx.Done():
		log.Printf("Update to v%s cancelled before download", payload.Version)
		return
	default:
	}

	log.Printf("Updating agent to v%s: downloading from %s", payload.Version, payload.DownloadPath)
	u.sendStatus(payload, protocol.UpdateStatusDownloading, "downloading new agent binary")
	if !promptForConsent {
		u.emitEvent(Event{Type: EventUpdateConsentGranted, Message: payload.Version, Detail: "consent granted via dashboard update push"})
	}

	apiKey := configuredAgentAPIKey()

	u.sendStatus(payload, protocol.UpdateStatusInstalling, "installing new agent binary")
	newPath, err := updateActivate(updateActivateParams{
		apiURL:       u.apiURL,
		downloadPath: payload.DownloadPath,
		checksum:     payload.Checksum,
		signature:    payload.Signature,
		apiKey:       apiKey,
		stateDir:     StateDir(),
		version:      payload.Version,
	})
	if err != nil {
		log.Printf("Agent update to v%s failed: %v", payload.Version, err)
		u.sendStatus(payload, protocol.UpdateStatusFailed, err.Error())
		return
	}

	log.Printf("Agent update to v%s installed; restarting", payload.Version)
	u.sendStatus(payload, protocol.UpdateStatusRestarting, "binary installed, restarting agent")

	if err := updateFinish(newPath, payload.Version, func() {
		u.emitEvent(Event{Type: EventUpdateApplied, Message: payload.Version, Detail: "agent updated to v" + payload.Version})
	}); err != nil {
		log.Printf("Failed to re-exec updated agent binary: %v", err)
		u.sendStatus(payload, protocol.UpdateStatusFailed, fmt.Sprintf("installed but restart failed: %v", err))
	}
}

// sendStatus reports one agent.update.status envelope to the dashboard.
func (u *Updater) sendStatus(payload protocol.AgentUpdatePayload, status, message string) {
	if u.send == nil {
		return
	}
	b, err := json.Marshal(protocol.AgentUpdateStatusPayload{
		Status:  status,
		Message: message,
		Version: payload.Version,
	})
	if err != nil {
		return
	}
	u.send(protocol.Envelope{
		Type:    protocol.TypeAgentUpdateStatus,
		ID:      fmt.Sprintf("update-%d", time.Now().UnixNano()),
		TS:      time.Now(),
		Payload: b,
	})
}

// markScheduled mirrors the update-scheduled flag onto the transport.
func (u *Updater) markScheduled(scheduled bool) {
	if u.setScheduled != nil {
		u.setScheduled(scheduled)
	}
}

// emitEvent mirrors one update lifecycle event onto the daemon event stream.
func (u *Updater) emitEvent(ev Event) {
	if u.emit != nil {
		u.emit(ev)
	}
}
