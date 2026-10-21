package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// jobControlPayload is the wire payload for the per-job control commands
// (CANCEL_JOB, PAUSE_JOB, RESUME_JOB).
type jobControlPayload struct {
	JobID string `json:"job_id"`
}

// decodedCommand is an inbound envelope decoded exactly once into the typed
// payload for its command type.
type decodedCommand struct {
	typ      string
	start    protocol.StartJobPayload
	control  jobControlPayload
	update   protocol.AgentUpdatePayload
	schedule protocol.ScheduleConfig
	scan           protocol.StartScanPayload
	rate           protocol.SetTelemetryRatePayload
	setLibraryPath protocol.SetLibraryPathPayload
}

// decodeCommand unmarshals the envelope payload into the typed command value
// for the envelope's type. Unknown types and malformed payloads are errors
// the dispatcher reports on the event stream; CANCEL_AGENT_UPDATE carries no
// payload. The legacy alias "job.start" maps to START_JOB.
func decodeCommand(env protocol.Envelope) (decodedCommand, error) {
	malformed := func(err error) (decodedCommand, error) {
		return decodedCommand{}, fmt.Errorf("malformed %s payload: %w", env.Type, err)
	}
	if len(env.Payload) > 0 {
		if err := protocol.CheckJSONDepth(env.Payload, protocol.DefaultMaxJSONDepth); err != nil {
			return malformed(err)
		}
	}
	switch env.Type {
	case protocol.TypeStartJob, "job.start":
		var p protocol.StartJobPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return malformed(err)
		}
		return decodedCommand{typ: protocol.TypeStartJob, start: p}, nil
	case protocol.TypeCancelJob, protocol.TypePauseJob, protocol.TypeResumeJob:
		var p jobControlPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return malformed(err)
		}
		return decodedCommand{typ: env.Type, control: p}, nil
	case protocol.TypeAgentUpdate:
		var p protocol.AgentUpdatePayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return malformed(err)
		}
		return decodedCommand{typ: env.Type, update: p}, nil
	case protocol.TypeCancelAgentUpdate:
		return decodedCommand{typ: env.Type}, nil
	case protocol.TypeUpdateSchedule:
		var p protocol.ScheduleConfig
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return malformed(err)
		}
		return decodedCommand{typ: env.Type, schedule: p}, nil
	case protocol.TypeStartScan:
		var p protocol.StartScanPayload
		if len(env.Payload) > 0 && string(env.Payload) != "null" && string(env.Payload) != "{}" {
			if err := json.Unmarshal(env.Payload, &p); err != nil {
				return malformed(err)
			}
		}
		return decodedCommand{typ: env.Type, scan: p}, nil
	case protocol.TypeSetTelemetryRate:
		var p protocol.SetTelemetryRatePayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return malformed(err)
		}
		return decodedCommand{typ: env.Type, rate: p}, nil
	case protocol.TypeSetLibraryPath:
		var p protocol.SetLibraryPathPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return malformed(err)
		}
		return decodedCommand{typ: env.Type, setLibraryPath: p}, nil
	default:
		return decodedCommand{}, fmt.Errorf("unknown command type %q", env.Type)
	}
}

// jobCommands is the supervisor surface the dispatcher drives.
// *JobSupervisor satisfies it; the dispatch test substitutes a fake.
type jobCommands interface {
	StartJob(ctx context.Context, payload protocol.StartJobPayload, rawPayload json.RawMessage) error
	CancelJob(ctx context.Context, jobID string)
	PauseJob(jobID string)
	ResumeJob(jobID string)
	UpdateSchedule(sched *protocol.ScheduleConfig)
	SetTelemetryRate(p protocol.SetTelemetryRatePayload)
}

// updateCommands is the updater surface the dispatcher drives; *Updater
// satisfies it.
type updateCommands interface {
	StartUpdate(payload protocol.AgentUpdatePayload)
	CancelUpdate()
}

// commandDispatcher decodes inbound envelopes once and dispatches the typed
// command to the supervisor or the updater. It contains no orchestration of
// its own: every job-start gate (confinement, quiet hours, stream safety,
// update deferral) lives behind jobCommands, and the update machinery behind
// updateCommands.
type commandDispatcher struct {
	ctx            context.Context
	jobs           func() jobCommands
	updater        updateCommands
	scanner        func(path, libraryID string)
	setLibraryPath func(p protocol.SetLibraryPathPayload)
	emit           func(Event)
}

// handle decodes one envelope and dispatches it to the module that owns it.
func (dsp *commandDispatcher) handle(env protocol.Envelope) {
	cmd, err := decodeCommand(env)
	if err != nil {
		log.Printf("Rejected inbound command %q: %v", env.Type, err)
		if dsp.emit != nil {
			dsp.emit(Event{Type: EventError, Message: err.Error()})
		}
		return
	}
	switch cmd.typ {
	case protocol.TypeStartJob:
		if err := dsp.jobs().StartJob(dsp.ctx, cmd.start, env.Payload); err != nil {
			log.Printf("Job %s not started: %v", cmd.start.JobID, err)
		}
	case protocol.TypeCancelJob:
		dsp.jobs().CancelJob(context.Background(), cmd.control.JobID)
	case protocol.TypePauseJob:
		dsp.jobs().PauseJob(cmd.control.JobID)
	case protocol.TypeResumeJob:
		dsp.jobs().ResumeJob(cmd.control.JobID)
	case protocol.TypeAgentUpdate:
		dsp.updater.StartUpdate(cmd.update)
	case protocol.TypeCancelAgentUpdate:
		dsp.updater.CancelUpdate()
	case protocol.TypeUpdateSchedule:
		dsp.jobs().UpdateSchedule(&cmd.schedule)
	case protocol.TypeSetTelemetryRate:
		dsp.jobs().SetTelemetryRate(cmd.rate)
	case protocol.TypeStartScan:
		if dsp.scanner != nil {
			go dsp.scanner(cmd.scan.Path, cmd.scan.LibraryID)
		}
	case protocol.TypeSetLibraryPath:
		if dsp.setLibraryPath != nil {
			dsp.setLibraryPath(cmd.setLibraryPath)
		}
	}
}
