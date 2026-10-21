package protocol

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// IdleTelemetry represents the payload sent during IDLE mode.
type IdleTelemetry struct {
	QueueSize int    `json:"queue_size"`
	DiskSpace uint64 `json:"disk_space"`
	Status    string `json:"status"` // e.g., "idle"
}

// WorkingTelemetry represents the payload sent during WORKING mode for file completion.
type WorkingTelemetry struct {
	FileID          string  `json:"file_id"`
	OriginalSize    int64   `json:"original_size"`
	NewSize         int64   `json:"new_size"`
	OriginalCodec   string  `json:"original_codec"`
	NewCodec        string  `json:"new_codec"`
	DurationSeconds float64 `json:"duration_seconds"`
	AvgSpeed        float64 `json:"avg_speed"` // e.g., multiplier like 2.5x
}

// WorkingHeartbeat represents the periodic heartbeat during WORKING mode.
type WorkingHeartbeat struct {
	QueueSize int    `json:"queue_size"`
	Status    string `json:"status"` // e.g., "working"
}

// LiveTelemetry represents the high-frequency telemetry sent during LIVE mode.
type LiveTelemetry struct {
	JobID        string  `json:"job_id"`
	FPS          float64 `json:"fps"`
	ETASeconds   int     `json:"eta_seconds"`
	Bitrate      int     `json:"bitrate"`      // kbps
	ProgressPct  float64 `json:"progress_pct"` // 0.0 to 100.0
	GPUUtilPct   float64 `json:"gpu_util_pct"` // 0.0 to 100.0
	CurrentFrame int     `json:"current_frame,omitempty"`
	TotalFrames  int     `json:"total_frames,omitempty"`
	SourceCodec  string  `json:"source_codec,omitempty"`
	TargetCodec  string  `json:"target_codec,omitempty"`
	Phase            string  `json:"phase,omitempty"`
	SubPhase         string  `json:"sub_phase,omitempty"`
	Activity         string  `json:"activity,omitempty"`
	CurrentIndex     int     `json:"current_index,omitempty"`
	TotalCount       int     `json:"total_count,omitempty"`
	ElapsedSeconds   float64 `json:"elapsed_seconds,omitempty"`
	Speed            float64 `json:"speed,omitempty"`
	InputPath        string  `json:"input_path,omitempty"`
	TransferredBytes int64   `json:"transferred_bytes,omitempty"`
	TotalBytes       int64   `json:"total_bytes,omitempty"`
}

// HeartbeatPayload represents periodic heartbeats sent by the agent.
type HeartbeatPayload struct {
	AgentName       string `json:"agent_name"`
	Status          string `json:"status"` // e.g., "IDLE", "WORKING"
	Version         string `json:"version,omitempty"`
	Platform        string `json:"platform,omitempty"`
	QueueSize       int    `json:"queue_size,omitempty"`
	DiskSpace       uint64 `json:"disk_space,omitempty"`
	Activity        string `json:"activity,omitempty"`
	UpdateScheduled bool   `json:"update_scheduled"`
}

// ScanProgressPayload represents real-time progress during media library discovery and scanning.
type ScanProgressPayload struct {
	AgentName      string  `json:"agent_name,omitempty"`
	Status         string  `json:"status,omitempty"`
	Completed      int     `json:"completed"`
	Total          int     `json:"total"`
	Percent        float64 `json:"percent"`
	ProgressPct    float64 `json:"progress_pct"`
	CurrentWorkers int     `json:"current_workers,omitempty"`
	MaxWorkers     int     `json:"max_workers,omitempty"`
	CurrentFile    string  `json:"current_file,omitempty"`
}

// SyncProgressPayload represents progress of syncing media library to the server.
type SyncProgressPayload struct {
	LibraryID   string  `json:"library_id,omitempty"`
	Chunk       int     `json:"chunk"`
	TotalChunks int     `json:"total_chunks"`
	ItemsSynced int     `json:"items_synced"`
	TotalItems  int     `json:"total_items"`
	Percent     float64 `json:"percent"`
	Stage       string  `json:"stage,omitempty"`
	Message     string  `json:"message,omitempty"`
}

const (
	TypeStartJob          = "START_JOB"
	TypeJobProgress       = "transcode.progress"
	TypeJobTrace          = "transcode.trace"
	TypeFileComplete      = "FILE_COMPLETE"
	TypeCancelJob         = "CANCEL_JOB"
	TypePauseJob          = "PAUSE_JOB"
	TypeResumeJob         = "RESUME_JOB"
	TypeHeartbeat         = "heartbeat"
	TypeScanProgress      = "scan.progress"
	TypeScanComplete      = "scan.complete"
	TypeSyncProgress      = "sync.progress"
	TypeAgentUpdate       = "AGENT_UPDATE"
	TypeAgentUpdateStatus = "agent.update.status"
	TypeCancelAgentUpdate = "CANCEL_AGENT_UPDATE"
	TypeFileCompleteAck   = "FILE_COMPLETE_ACK"
	TypeUpdateSchedule    = "UPDATE_SCHEDULE"
	TypeStartScan         = "START_SCAN"
	TypeSetTelemetryRate  = "SET_TELEMETRY_RATE"
	TypeSetLibraryPath    = "SET_LIBRARY_PATH"
	TypeLibraryPathValid  = "LIBRARY_PATH_VALID"
	TypeLibraryPathError  = "LIBRARY_PATH_ERROR"
)

// LibraryPathStatusPayload represents the bidirectional acknowledgment or error
// reporting payload for library local mount path validation.
type LibraryPathStatusPayload struct {
	LibraryID string `json:"library_id"`
	Path      string `json:"path"`
	Status    string `json:"status"` // "mounted" or "error"
	Valid     bool   `json:"valid"`
	Reason    string `json:"reason,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Streaming mode constants for telemetry pacing.
const (
	TelemetryModeLive       = "LIVE"
	TelemetryModeBackground = "BACKGROUND"
	TelemetryModeIdle       = "IDLE"
)

// ValidTelemetryModes defines the allowlist for telemetry streaming modes.
var ValidTelemetryModes = map[string]bool{
	TelemetryModeLive:       true,
	TelemetryModeBackground: true,
	TelemetryModeIdle:       true,
}

// IsValidTelemetryMode reports whether mode is a recognized telemetry streaming mode.
func IsValidTelemetryMode(mode string) bool {
	return ValidTelemetryModes[mode]
}

// SetTelemetryRatePayload represents the control frame payload to dynamically
// adjust agent progress verbosity based on dashboard subscriber presence.
type SetTelemetryRatePayload struct {
	Mode   string `json:"mode"`
	RateMS int    `json:"rate_ms,omitempty"`
}

// UnmarshalJSON validates depth and allowable modes when decoding SetTelemetryRatePayload.
func (p *SetTelemetryRatePayload) UnmarshalJSON(data []byte) error {
	if err := CheckJSONDepth(data, DefaultMaxJSONDepth); err != nil {
		return err
	}
	type wire SetTelemetryRatePayload
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	if !IsValidTelemetryMode(w.Mode) {
		return fmt.Errorf("invalid telemetry mode %q: must be one of 'LIVE', 'BACKGROUND', 'IDLE'", w.Mode)
	}
	p.Mode = w.Mode
	p.RateMS = w.RateMS
	return nil
}

// SetLibraryPathPayload represents the control frame payload to remotely configure
// an agent's target library ID and local filesystem root path.
type SetLibraryPathPayload struct {
	LibraryID string `json:"library_id"`
	LocalPath string `json:"local_path"`
}


// StartScanPayload represents the payload dispatched to command an agent to scan a library.
type StartScanPayload struct {
	LibraryID string `json:"library_id,omitempty"`
	Path      string `json:"path,omitempty"`
}

// AgentUpdatePayload is dispatched by the server to command an agent to
// self-update. The agent resolves the download URL against its own configured
// server URL (DownloadPath) rather than trusting an absolute URL, and refuses
// to install a binary whose SHA-256 does not match Checksum.
type AgentUpdatePayload struct {
	Version      string `json:"version"`
	Checksum     string `json:"checksum"`
	DownloadPath string `json:"download_path"`
	// Auto is set when the server has already obtained the operator's
	// consent (the dashboard's Update action): the agent installs without a
	// local consent prompt even when a consent handler is registered.
	Auto bool `json:"auto,omitempty"`
	// Signature is the hex-encoded Ed25519 signature over the binary bytes,
	// produced by the official release private key. The agent verifies this
	// against its embedded public key before replacing the running executable.
	// Verification is enforced: an absent or invalid Signature, or an embedded
	// all-zero dev-sentinel key, refuses the update.
	Signature string `json:"signature,omitempty"`
}

// Agent update status values reported back to the dashboard.
const (
	UpdateStatusBusy        = "busy"        // update skipped: transcode pipeline active
	UpdateStatusScheduled   = "scheduled"   // update scheduled: waiting for active transcode to complete
	UpdateStatusDownloading = "downloading" // downloading the new binary
	UpdateStatusInstalling  = "installing"  // checksum verified, swapping binary
	UpdateStatusRestarting  = "restarting"  // binary replaced, process re-executing
	UpdateStatusCompleted   = "completed"   // binary installed and agent reconnected with new version
	UpdateStatusFailed      = "failed"      // update aborted; Message has the reason
)

// AgentUpdateStatusPayload is reported by the agent while processing an
// AGENT_UPDATE command so the dashboard can show live update progress.
type AgentUpdateStatusPayload struct {
	Status    string `json:"status"`
	Message   string `json:"message,omitempty"`
	Version   string `json:"version,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
	AgentName string `json:"agent_name,omitempty"`
}

// WeeklySchedule encapsulates a 168-hour (7 days * 24 hours) schedule grid.
// Each character is '0' (inactive) or '1' (active).
type WeeklySchedule string

// IsActiveAt determines if the given local time falls within an active hour block.
func (w WeeklySchedule) IsActiveAt(localT time.Time) bool {
	if len(w) != 168 {
		return false
	}
	day := int(localT.Weekday()) // Sunday = 0
	hour := localT.Hour()
	index := day*24 + hour
	return w[index] == '1'
}

// ScheduleConfig represents quiet-hours schedule settings for an agent.
type ScheduleConfig struct {
	QuietHoursEnabled bool           `json:"quiet_hours_enabled"`
	ScheduleGrid      WeeklySchedule `json:"schedule_grid"`
	Timezone          string         `json:"timezone"`
}

// IsQuietHoursActive determines if current time is within the configured quiet hours window.
func (s *ScheduleConfig) IsQuietHoursActive(t time.Time) bool {
	if s == nil || !s.QuietHoursEnabled {
		return false
	}
	loc := t.Location()
	if s.Timezone != "" {
		if parsedLoc, err := time.LoadLocation(s.Timezone); err == nil {
			loc = parsedLoc
		}
	}
	localT := t.In(loc)
	return s.ScheduleGrid.IsActiveAt(localT)
}

// CurrentJobSpecVersion defines the active transcode payload contract generation.
const CurrentJobSpecVersion = 2

// StreamHygienePolicy defines Tier 2 global audio, subtitle, and metadata stream hygiene settings.
type StreamHygienePolicy struct {
	PreferredAudioLanguages    []string `json:"preferred_audio_languages,omitempty"`
	AudioLanguageMode          string   `json:"audio_language_mode,omitempty"`
	SubtitleFormatPolicy       string   `json:"subtitle_format_policy,omitempty"`
	ForcedSubtitlePolicy       string   `json:"forced_subtitle_policy,omitempty"`
	PreferredSubtitleLanguages []string `json:"preferred_subtitle_languages,omitempty"`
	SubtitleLanguageMode       string   `json:"subtitle_language_mode,omitempty"`
	StripMetadata              bool     `json:"strip_metadata,omitempty"`
}

// QualityTierArchivist marks a job whose vendor encoding parameters are
// code-owned on the agent (Archivist grade at every hardware fallback step),
// not user-editable through preset tuning fields. See the agent's ffmpeg
// package for the per-vendor parameter table.
const QualityTierArchivist = "archivist"

// VideoConfig represents Tier 1/3 resolved video encoding parameters in wire contract v2.
type VideoConfig struct {
	TargetCodec   string `json:"target_codec,omitempty"`
	EncoderEngine string `json:"encoder_engine,omitempty"`
	CRF           int    `json:"crf,omitempty"`
	PresetSpeed   string `json:"preset_speed,omitempty"`
	QualityTarget int    `json:"quality_target,omitempty"`
	ColorDepth    string `json:"color_depth,omitempty"`
	FilmGrainSao  bool   `json:"film_grain_sao,omitempty"`
	AqMode        int    `json:"aq_mode,omitempty"`
	ContentTuning string `json:"content_tuning,omitempty"`
	// VendorTuning carries user-selected hardware encoder tuning; nil means
	// the agent's default vendor package applies (issue #195).
	VendorTuning *VendorTuning `json:"vendor_tuning,omitempty"`
	// QualityTier selects a code-owned per-vendor quality package
	// (e.g. "archivist"); empty means plain preset values apply.
	QualityTier string `json:"quality_tier,omitempty"`
}

// AudioConfig represents Tier 2/3 resolved audio parameters in wire contract v2.
type AudioConfig struct {
	Codec              string   `json:"codec,omitempty"`
	CodecPolicy        string   `json:"codec_policy,omitempty"`
	LanguageMode       string   `json:"language_mode,omitempty"`
	PreferredLanguages []string `json:"preferred_languages,omitempty"`
}

// SubtitlesConfig represents Tier 2/3 resolved subtitle parameters in wire contract v2.
type SubtitlesConfig struct {
	FormatPolicy       string   `json:"format_policy,omitempty"`
	LanguageMode       string   `json:"language_mode,omitempty"`
	PreferredLanguages []string `json:"preferred_languages,omitempty"`
	ForcedPolicy       string   `json:"forced_policy,omitempty"`
	DeliveryMode       string   `json:"delivery_mode,omitempty"`
}

// ContainerConfig represents Tier 2/3 container disposition parameters in wire contract v2.
type ContainerConfig struct {
	Format             string `json:"format,omitempty"`
	StripMetadata      bool   `json:"strip_metadata,omitempty"`
	OutputFileStrategy string `json:"output_file_strategy,omitempty"`
	ActionOnSuccess    string `json:"action_on_success,omitempty"`
}

// ValidOutputFileStrategies defines the allowed values for output_file_strategy.
var ValidOutputFileStrategies = map[string]bool{
	"replace":   true,
	"alongside": true,
	"alternate": true,
}

// ValidActionsOnSuccess defines the allowed values for action_on_success.
var ValidActionsOnSuccess = map[string]bool{
	"keep":    true,
	"trash":   true,
	"delete":  true,
	"archive": true,
}

// IsValidOutputFileStrategy checks if the output_file_strategy is in the allowlist.
func IsValidOutputFileStrategy(strategy string) bool {
	return ValidOutputFileStrategies[strategy]
}

// IsValidActionOnSuccess checks if the action_on_success is in the allowlist.
func IsValidActionOnSuccess(action string) bool {
	return ValidActionsOnSuccess[action]
}

// ExecutionConfig represents Tier 1/4 execution and safety parameters in wire contract v2.
type ExecutionConfig struct {
	CustomFlags               []string `json:"custom_flags,omitempty"`
	ResolutionWarnings        []string `json:"resolution_warnings,omitempty"`
	PostTranscodeVerification bool     `json:"post_transcode_verification,omitempty"`
}

// StartJobPayload represents command dispatched to the agent to start transcoding.
// Supports both spec_version 1 (flat legacy) and spec_version 2 (structured cascade).
type StartJobPayload struct {
	JobID        string          `json:"job_id"`
	MediaItemID  string          `json:"media_item_id,omitempty"`
	InputPath    string          `json:"input_path"`
	RelativePath string          `json:"relative_path,omitempty"`
	LocalPath    string          `json:"local_path,omitempty"`
	TargetCodec  string          `json:"target_codec,omitempty"`
	PresetName   string          `json:"preset_name,omitempty"`
	Container    string          `json:"container,omitempty"`
	CRF          int             `json:"crf,omitempty"`
	PresetSpeed  string          `json:"preset_speed,omitempty"`
	AudioCodec   string          `json:"audio_codec,omitempty"`
	Schedule     *ScheduleConfig `json:"schedule,omitempty"`

	// SpecVersion identifies the shape of the tuning block.
	SpecVersion int `json:"spec_version,omitempty"`

	EncoderEngine    string `json:"encoder_engine,omitempty"`
	QualityTarget    int    `json:"quality_target,omitempty"`
	AudioHandling    string `json:"audio_handling,omitempty"`
	SubtitleHandling string `json:"subtitle_handling,omitempty"`
	ColorDepth       string `json:"color_depth,omitempty"`
	FilmGrainSao     bool   `json:"film_grain_sao,omitempty"`
	AqMode           int    `json:"aq_mode,omitempty"`
	ContentTuning    string `json:"content_tuning,omitempty"`
	CustomFlags      string `json:"custom_flags,omitempty"`
	// VendorTuning mirrors Video.VendorTuning for flat (v1-style) consumers;
	// nil means the agent's default vendor package applies (issue #195).
	VendorTuning *VendorTuning `json:"vendor_tuning,omitempty"`
	// QualityTier mirrors Video.QualityTier for flat (v1-style) consumers;
	// agents read the tier through the Video section when present.
	QualityTier string `json:"quality_tier,omitempty"`

	// Tier 2 User Global Hygiene
	StreamHygienePolicy

	GlobalSettings map[string]interface{} `json:"global_settings,omitempty"`

	OutputFileStrategy string `json:"output_file_strategy,omitempty"`
	ActionOnSuccess    string `json:"action_on_success,omitempty"`

	// Wire contract v2 structured sections
	Video           *VideoConfig     `json:"video,omitempty"`
	Audio           *AudioConfig     `json:"audio,omitempty"`
	Subtitles       *SubtitlesConfig `json:"subtitles,omitempty"`
	ContainerConfig *ContainerConfig `json:"container_config,omitempty"`
	Execution       *ExecutionConfig `json:"execution,omitempty"`
}

func (p StartJobPayload) MarshalJSON() ([]byte, error) {
	if p.SpecVersion == 2 {
		video := p.Video
		if video == nil {
			video = &VideoConfig{
				TargetCodec:   p.TargetCodec,
				EncoderEngine: p.EncoderEngine,
				CRF:           p.CRF,
				PresetSpeed:   p.PresetSpeed,
				QualityTarget: p.QualityTarget,
				ColorDepth:    p.ColorDepth,
				FilmGrainSao:  p.FilmGrainSao,
				AqMode:        p.AqMode,
				ContentTuning: p.ContentTuning,
				VendorTuning:  p.VendorTuning,
				QualityTier:   p.QualityTier,
			}
		} else if video.QualityTier == "" && p.QualityTier != "" {
			// Callers may populate only the flat field: backfill the Video
			// section so the tier survives the wire instead of being dropped.
			clone := *video
			clone.QualityTier = p.QualityTier
			video = &clone
		}
		if video.VendorTuning == nil && p.VendorTuning != nil {
			// Same flat-field backfill as the quality tier above.
			clone := *video
			clone.VendorTuning = p.VendorTuning
			video = &clone
		}
		audio := p.Audio
		if audio == nil {
			audio = &AudioConfig{
				Codec:              p.AudioCodec,
				CodecPolicy:        p.AudioHandling,
				LanguageMode:       p.AudioLanguageMode,
				PreferredLanguages: p.PreferredAudioLanguages,
			}
		}
		subtitles := p.Subtitles
		if subtitles == nil {
			subtitles = &SubtitlesConfig{
				FormatPolicy:       p.SubtitleFormatPolicy,
				LanguageMode:       p.SubtitleLanguageMode,
				PreferredLanguages: p.PreferredSubtitleLanguages,
				ForcedPolicy:       p.ForcedSubtitlePolicy,
				DeliveryMode:       "soft_stream",
			}
		}
		containerCfg := p.ContainerConfig
		if containerCfg == nil {
			outputStrategy := p.OutputFileStrategy
			if outputStrategy == "" {
				outputStrategy = "alongside"
			}
			actionSuccess := p.ActionOnSuccess
			if actionSuccess == "" {
				actionSuccess = "keep"
			}
			containerCfg = &ContainerConfig{
				Format:             p.Container,
				StripMetadata:      p.StripMetadata,
				OutputFileStrategy: outputStrategy,
				ActionOnSuccess:    actionSuccess,
			}
		} else {
			clone := *containerCfg
			if clone.OutputFileStrategy == "" && p.OutputFileStrategy != "" {
				clone.OutputFileStrategy = p.OutputFileStrategy
			}
			if clone.ActionOnSuccess == "" && p.ActionOnSuccess != "" {
				clone.ActionOnSuccess = p.ActionOnSuccess
			}
			containerCfg = &clone
		}
		exec := p.Execution
		if exec == nil {
			var flags []string
			if p.CustomFlags != "" {
				flags = strings.Fields(p.CustomFlags)
			}
			exec = &ExecutionConfig{
				CustomFlags:               flags,
				ResolutionWarnings:        []string{},
				PostTranscodeVerification: true,
			}
		}
		type v2Wire struct {
			JobID        string           `json:"job_id"`
			MediaItemID  string           `json:"media_item_id,omitempty"`
			InputPath    string           `json:"input_path"`
			RelativePath string           `json:"relative_path,omitempty"`
			LocalPath    string           `json:"local_path,omitempty"`
			SpecVersion  int              `json:"spec_version"`
			Video        *VideoConfig     `json:"video,omitempty"`
			Audio        *AudioConfig     `json:"audio,omitempty"`
			Subtitles    *SubtitlesConfig `json:"subtitles,omitempty"`
			Container    *ContainerConfig `json:"container,omitempty"`
			Execution    *ExecutionConfig `json:"execution,omitempty"`
			Schedule     *ScheduleConfig  `json:"schedule,omitempty"`
		}
		return json.Marshal(v2Wire{
			JobID:        p.JobID,
			MediaItemID:  p.MediaItemID,
			InputPath:    p.InputPath,
			RelativePath: p.RelativePath,
			LocalPath:    p.LocalPath,
			SpecVersion:  2,
			Video:        video,
			Audio:        audio,
			Subtitles:    subtitles,
			Container:    containerCfg,
			Execution:    exec,
			Schedule:     p.Schedule,
		})
	}

	type v1Wire struct {
		JobID               string                 `json:"job_id"`
		MediaItemID         string                 `json:"media_item_id,omitempty"`
		InputPath           string                 `json:"input_path"`
		RelativePath        string                 `json:"relative_path,omitempty"`
		LocalPath           string                 `json:"local_path,omitempty"`
		TargetCodec         string                 `json:"target_codec,omitempty"`
		PresetName          string                 `json:"preset_name,omitempty"`
		Container           string                 `json:"container,omitempty"`
		CRF                 int                    `json:"crf,omitempty"`
		PresetSpeed         string                 `json:"preset_speed,omitempty"`
		AudioCodec          string                 `json:"audio_codec,omitempty"`
		Schedule            *ScheduleConfig        `json:"schedule,omitempty"`
		SpecVersion         int                    `json:"spec_version,omitempty"`
		EncoderEngine       string                 `json:"encoder_engine,omitempty"`
		QualityTarget       int                    `json:"quality_target,omitempty"`
		AudioHandling       string                 `json:"audio_handling,omitempty"`
		SubtitleHandling    string                 `json:"subtitle_handling,omitempty"`
		ColorDepth          string                 `json:"color_depth,omitempty"`
		FilmGrainSao        bool                   `json:"film_grain_sao,omitempty"`
		AqMode              int                    `json:"aq_mode,omitempty"`
		ContentTuning       string                 `json:"content_tuning,omitempty"`
		CustomFlags         string                 `json:"custom_flags,omitempty"`
		VendorTuning        *VendorTuning          `json:"vendor_tuning,omitempty"`
		QualityTier         string                 `json:"quality_tier,omitempty"`
		StreamHygienePolicy
		GlobalSettings      map[string]interface{} `json:"global_settings,omitempty"`
		OutputFileStrategy  string                 `json:"output_file_strategy,omitempty"`
		ActionOnSuccess     string                 `json:"action_on_success,omitempty"`
	}
	return json.Marshal(v1Wire{
		JobID:               p.JobID,
		MediaItemID:         p.MediaItemID,
		InputPath:           p.InputPath,
		RelativePath:        p.RelativePath,
		LocalPath:           p.LocalPath,
		TargetCodec:         p.TargetCodec,
		PresetName:          p.PresetName,
		Container:           p.Container,
		CRF:                 p.CRF,
		PresetSpeed:         p.PresetSpeed,
		AudioCodec:          p.AudioCodec,
		Schedule:            p.Schedule,
		SpecVersion:         p.SpecVersion,
		EncoderEngine:       p.EncoderEngine,
		QualityTarget:       p.QualityTarget,
		AudioHandling:       p.AudioHandling,
		SubtitleHandling:    p.SubtitleHandling,
		ColorDepth:          p.ColorDepth,
		FilmGrainSao:        p.FilmGrainSao,
		AqMode:              p.AqMode,
		ContentTuning:       p.ContentTuning,
		CustomFlags:         p.CustomFlags,
		VendorTuning:        p.VendorTuning,
		QualityTier:         p.QualityTier,
		StreamHygienePolicy: p.StreamHygienePolicy,
		GlobalSettings:      p.GlobalSettings,
		OutputFileStrategy:  p.OutputFileStrategy,
		ActionOnSuccess:     p.ActionOnSuccess,
	})
}

func (p *StartJobPayload) UnmarshalJSON(data []byte) error {
	if err := CheckJSONDepth(data, DefaultMaxJSONDepth); err != nil {
		return err
	}
	var peek struct {
		SpecVersion int             `json:"spec_version"`
		Container   json.RawMessage `json:"container"`
	}
	if err := json.Unmarshal(data, &peek); err != nil {
		return err
	}

	isV2 := peek.SpecVersion == 2
	if !isV2 && len(peek.Container) > 0 {
		trimmed := strings.TrimSpace(string(peek.Container))
		if strings.HasPrefix(trimmed, "{") {
			isV2 = true
		}
	}

	if isV2 {
		type v2Wire struct {
			JobID        string           `json:"job_id"`
			MediaItemID  string           `json:"media_item_id,omitempty"`
			InputPath    string           `json:"input_path"`
			RelativePath string           `json:"relative_path,omitempty"`
			LocalPath    string           `json:"local_path,omitempty"`
			SpecVersion  int              `json:"spec_version"`
			Video        *VideoConfig     `json:"video,omitempty"`
			Audio        *AudioConfig     `json:"audio,omitempty"`
			Subtitles    *SubtitlesConfig `json:"subtitles,omitempty"`
			Container    *ContainerConfig `json:"container,omitempty"`
			Execution    *ExecutionConfig `json:"execution,omitempty"`
			Schedule     *ScheduleConfig  `json:"schedule,omitempty"`
		}
		var wire v2Wire
		if err := json.Unmarshal(data, &wire); err != nil {
			return err
		}
		p.JobID = wire.JobID
		p.MediaItemID = wire.MediaItemID
		p.InputPath = wire.InputPath
		p.RelativePath = wire.RelativePath
		p.LocalPath = wire.LocalPath
		p.SpecVersion = 2
		p.Schedule = wire.Schedule
		p.Video = wire.Video
		p.Audio = wire.Audio
		p.Subtitles = wire.Subtitles
		p.ContainerConfig = wire.Container
		p.Execution = wire.Execution

		if wire.Video != nil {
			p.TargetCodec = wire.Video.TargetCodec
			p.EncoderEngine = wire.Video.EncoderEngine
			p.CRF = wire.Video.CRF
			p.PresetSpeed = wire.Video.PresetSpeed
			p.QualityTarget = wire.Video.QualityTarget
			p.ColorDepth = wire.Video.ColorDepth
			p.FilmGrainSao = wire.Video.FilmGrainSao
			p.AqMode = wire.Video.AqMode
			p.ContentTuning = wire.Video.ContentTuning
			p.VendorTuning = wire.Video.VendorTuning
			p.QualityTier = wire.Video.QualityTier
		}
		if wire.Audio != nil {
			p.AudioCodec = wire.Audio.Codec
			p.AudioHandling = wire.Audio.CodecPolicy
			p.AudioLanguageMode = wire.Audio.LanguageMode
			p.PreferredAudioLanguages = append([]string{}, wire.Audio.PreferredLanguages...)
		}
		if wire.Subtitles != nil {
			p.SubtitleHandling = wire.Subtitles.FormatPolicy
			p.SubtitleFormatPolicy = wire.Subtitles.FormatPolicy
			p.SubtitleLanguageMode = wire.Subtitles.LanguageMode
			p.PreferredSubtitleLanguages = append([]string{}, wire.Subtitles.PreferredLanguages...)
			p.ForcedSubtitlePolicy = wire.Subtitles.ForcedPolicy
		}
		if wire.Container != nil {
			p.Container = wire.Container.Format
			p.StripMetadata = wire.Container.StripMetadata
			p.OutputFileStrategy = wire.Container.OutputFileStrategy
			p.ActionOnSuccess = wire.Container.ActionOnSuccess
		}
		if p.OutputFileStrategy == "" {
			p.OutputFileStrategy = "alongside"
		}
		if p.ActionOnSuccess == "" {
			p.ActionOnSuccess = "keep"
		}
		if !IsValidOutputFileStrategy(p.OutputFileStrategy) {
			return fmt.Errorf("invalid output_file_strategy %q: must be one of 'alongside', 'replace', 'alternate'", p.OutputFileStrategy)
		}
		if !IsValidActionOnSuccess(p.ActionOnSuccess) {
			return fmt.Errorf("invalid action_on_success %q: must be one of 'keep', 'trash', 'delete', 'archive'", p.ActionOnSuccess)
		}
		if wire.Execution != nil {
			p.CustomFlags = strings.Join(wire.Execution.CustomFlags, " ")
		}
		return nil
	}

	type v1Wire struct {
		JobID               string                 `json:"job_id"`
		MediaItemID         string                 `json:"media_item_id,omitempty"`
		InputPath           string                 `json:"input_path"`
		RelativePath        string                 `json:"relative_path,omitempty"`
		LocalPath           string                 `json:"local_path,omitempty"`
		TargetCodec         string                 `json:"target_codec,omitempty"`
		PresetName          string                 `json:"preset_name,omitempty"`
		Container           string                 `json:"container,omitempty"`
		CRF                 int                    `json:"crf,omitempty"`
		PresetSpeed         string                 `json:"preset_speed,omitempty"`
		AudioCodec          string                 `json:"audio_codec,omitempty"`
		Schedule            *ScheduleConfig        `json:"schedule,omitempty"`
		SpecVersion         int                    `json:"spec_version,omitempty"`
		EncoderEngine       string                 `json:"encoder_engine,omitempty"`
		QualityTarget       int                    `json:"quality_target,omitempty"`
		AudioHandling       string                 `json:"audio_handling,omitempty"`
		SubtitleHandling    string                 `json:"subtitle_handling,omitempty"`
		ColorDepth          string                 `json:"color_depth,omitempty"`
		FilmGrainSao        bool                   `json:"film_grain_sao,omitempty"`
		AqMode              int                    `json:"aq_mode,omitempty"`
		ContentTuning       string                 `json:"content_tuning,omitempty"`
		CustomFlags         string                 `json:"custom_flags,omitempty"`
		VendorTuning        *VendorTuning          `json:"vendor_tuning,omitempty"`
		QualityTier         string                 `json:"quality_tier,omitempty"`
		StreamHygienePolicy
		GlobalSettings      map[string]interface{} `json:"global_settings,omitempty"`
		OutputFileStrategy  string                 `json:"output_file_strategy,omitempty"`
		ActionOnSuccess     string                 `json:"action_on_success,omitempty"`
	}
	var wire v1Wire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	p.JobID = wire.JobID
	p.MediaItemID = wire.MediaItemID
	p.InputPath = wire.InputPath
	p.RelativePath = wire.RelativePath
	p.LocalPath = wire.LocalPath
	p.TargetCodec = wire.TargetCodec
	p.PresetName = wire.PresetName
	p.Container = wire.Container
	p.CRF = wire.CRF
	p.PresetSpeed = wire.PresetSpeed
	p.AudioCodec = wire.AudioCodec
	p.Schedule = wire.Schedule
	p.SpecVersion = wire.SpecVersion
	p.EncoderEngine = wire.EncoderEngine
	p.QualityTarget = wire.QualityTarget
	p.AudioHandling = wire.AudioHandling
	p.SubtitleHandling = wire.SubtitleHandling
	p.ColorDepth = wire.ColorDepth
	p.FilmGrainSao = wire.FilmGrainSao
	p.AqMode = wire.AqMode
	p.ContentTuning = wire.ContentTuning
	p.CustomFlags = wire.CustomFlags
	p.VendorTuning = wire.VendorTuning
	p.QualityTier = wire.QualityTier
	p.StreamHygienePolicy = wire.StreamHygienePolicy
	p.GlobalSettings = wire.GlobalSettings
	p.OutputFileStrategy = wire.OutputFileStrategy
	if p.OutputFileStrategy == "" {
		p.OutputFileStrategy = "alongside"
	}
	p.ActionOnSuccess = wire.ActionOnSuccess
	if p.ActionOnSuccess == "" {
		p.ActionOnSuccess = "keep"
	}
	if !IsValidOutputFileStrategy(p.OutputFileStrategy) {
		return fmt.Errorf("invalid output_file_strategy %q: must be one of 'alongside', 'replace', 'alternate'", p.OutputFileStrategy)
	}
	if !IsValidActionOnSuccess(p.ActionOnSuccess) {
		return fmt.Errorf("invalid action_on_success %q: must be one of 'keep', 'trash', 'delete', 'archive'", p.ActionOnSuccess)
	}

	p.Video = &VideoConfig{
		TargetCodec:   wire.TargetCodec,
		EncoderEngine: wire.EncoderEngine,
		CRF:           wire.CRF,
		PresetSpeed:   wire.PresetSpeed,
		QualityTarget: wire.QualityTarget,
		ColorDepth:    wire.ColorDepth,
		FilmGrainSao:  wire.FilmGrainSao,
		AqMode:        wire.AqMode,
		ContentTuning: wire.ContentTuning,
		QualityTier:   wire.QualityTier,
	}
	p.Audio = &AudioConfig{
		Codec:              wire.AudioCodec,
		CodecPolicy:        wire.AudioHandling,
		LanguageMode:       wire.AudioLanguageMode,
		PreferredLanguages: append([]string{}, wire.PreferredAudioLanguages...),
	}
	p.Subtitles = &SubtitlesConfig{
		FormatPolicy:       wire.SubtitleFormatPolicy,
		LanguageMode:       wire.SubtitleLanguageMode,
		PreferredLanguages: append([]string{}, wire.PreferredSubtitleLanguages...),
		ForcedPolicy:       wire.ForcedSubtitlePolicy,
		DeliveryMode:       "soft_stream",
	}
	p.ContainerConfig = &ContainerConfig{
		Format:             wire.Container,
		StripMetadata:      wire.StripMetadata,
		OutputFileStrategy: p.OutputFileStrategy,
		ActionOnSuccess:    p.ActionOnSuccess,
	}
	var flags []string
	if wire.CustomFlags != "" {
		flags = strings.Fields(wire.CustomFlags)
	}
	p.Execution = &ExecutionConfig{
		CustomFlags:               flags,
		ResolutionWarnings:        []string{},
		PostTranscodeVerification: true,
	}
	return nil
}

// RejectionReason represents structured error reasons when a worker fast-fails a job.
type RejectionReason string

const (
	RejectionFileNotFound      RejectionReason = "FILE_NOT_FOUND"
	RejectionLibraryNotMounted RejectionReason = "LIBRARY_NOT_MOUNTED"
)

// MaxFileCompleteErrorLen bounds the error_message carried in a FILE_COMPLETE
// envelope. ffmpeg failures can quote arbitrary stderr (including filter
// output), so an uncapped message is an exfiltration channel off the agent.
const MaxFileCompleteErrorLen = 4000

// TruncateFileCompleteError caps an error message for the FILE_COMPLETE
// envelope, sanitizing local system paths prior to length truncation.
func TruncateFileCompleteError(msg string) string {
	msg = SanitizeErrorMessage(msg)
	if len(msg) <= MaxFileCompleteErrorLen {
		return msg
	}
	return msg[:MaxFileCompleteErrorLen] + "… (truncated)"
}

// FfmpegCommandRecord captures one ffmpeg invocation of a transcode job so
// the exact argv the agent ran is inspectable after completion. Presets
// resolve into different argv depending on encoder, capabilities, and vendor
// tuning, and ffmpeg's own rate-control selection is otherwise invisible
// without verbose logs. Paths only — never redacted.
type FfmpegCommandRecord struct {
	// Phase identifies the encode phase, e.g. "encode" (full-stream),
	// "encode:chunk" (chunked pipeline; Count > 1 means the argv shape
	// repeated per chunk with different input/output paths), or "concat".
	Phase string `json:"phase"`
	Argv  []string `json:"argv"`
	// Count is how many invocations shared this phase/argv shape (>1 for
	// per-chunk encodes); omitted when 1.
	Count int `json:"count,omitempty"`
}

// JobTracePayload reports the ffmpeg command trace at phase-start time so
// the exact argv a job is running is persisted before the encode finishes
// (or even if it never does: crashes and cancellations keep the command).
// Sent as a transcode.trace envelope whenever a new phase record appears;
// idempotent upsert on the server.
type JobTracePayload struct {
	JobID    string                `json:"job_id"`
	Commands []FfmpegCommandRecord `json:"commands"`
}

// FileCompletePayload represents completion results reported by the agent.
type FileCompletePayload struct {
	JobID           string          `json:"job_id"`
	MediaItemID     string          `json:"media_item_id,omitempty"`
	InputPath       string          `json:"input_path,omitempty"`
	OriginalSize    int64           `json:"original_size"`
	NewSize         int64           `json:"new_size"`
	SavingsBytes    int64           `json:"savings_bytes"`
	DurationSecs    float64         `json:"duration_secs"`
	OriginalHash    string          `json:"original_hash,omitempty"`
	NewHash         string          `json:"new_hash,omitempty"`
	Success         bool            `json:"success"`
	ErrorMessage    string          `json:"error_message,omitempty"`
	RejectionReason RejectionReason `json:"rejection_reason,omitempty"`
	// FfmpegCommands is the argv trace of the job's ffmpeg phases (encode,
	// per-chunk encodes, concat). Only set on success.
	FfmpegCommands []FfmpegCommandRecord `json:"ffmpeg_commands,omitempty"`
	// VMAFScore is the spot-check perceptual quality score over a bounded
	// sample of original vs output, computed when the job spec opted into
	// post-transcode verification. Nil when not computed.
	VMAFScore *float64 `json:"vmaf_score,omitempty"`
}

// FileCompleteAckPayload represents acknowledgment sent by server upon receiving FILE_COMPLETE.
type FileCompleteAckPayload struct {
	EnvelopeID string `json:"envelope_id"`
	JobID      string `json:"job_id,omitempty"`
	Success    bool   `json:"success"`
}

// SanitizeTelemetryPath sanitizes local filesystem paths for outgoing telemetry
// and completion payloads, stripping usernames, host mounts, and absolute roots
// while preserving library-relative paths or file basenames.
func SanitizeTelemetryPath(inputPath, relPath string) string {
	if relPath != "" {
		normRel := strings.ReplaceAll(relPath, "\\", "/")
		idx := strings.LastIndex(normRel, "/")
		if idx == -1 {
			return normRel
		}
		return strings.TrimPrefix(normRel, "/")
	}
	if inputPath == "" {
		return ""
	}
	normInput := strings.TrimRight(strings.ReplaceAll(inputPath, "\\", "/"), "/")
	idx := strings.LastIndex(normInput, "/")
	if idx != -1 {
		return normInput[idx+1:]
	}
	return normInput
}

// MarshalJSON ensures command traces scrub absolute local filesystem paths
// before serialization.
func (r FfmpegCommandRecord) MarshalJSON() ([]byte, error) {
	type wire FfmpegCommandRecord
	w := wire(r)
	if len(w.Argv) > 0 {
		w.Argv = ScrubCommandArgs(w.Argv)
	}
	return json.Marshal(w)
}

// MarshalJSON ensures JobTracePayload scrubs all command traces before serialization.
func (p JobTracePayload) MarshalJSON() ([]byte, error) {
	type wire JobTracePayload
	w := wire(p)
	if len(w.Commands) > 0 {
		w.Commands = ScrubFfmpegCommands(w.Commands)
	}
	return json.Marshal(w)
}

// MarshalJSON ensures FileCompletePayload sanitizes ErrorMessage and scrubs
// FfmpegCommands before serialization.
func (p FileCompletePayload) MarshalJSON() ([]byte, error) {
	type wire FileCompletePayload
	w := wire(p)
	if w.ErrorMessage != "" {
		w.ErrorMessage = TruncateFileCompleteError(w.ErrorMessage)
	}
	if len(w.FfmpegCommands) > 0 {
		w.FfmpegCommands = ScrubFfmpegCommands(w.FfmpegCommands)
	}
	return json.Marshal(w)
}

// Standardized path placeholders for host filesystem scrubbing.
const (
	PlaceholderMediaRoot  = "<MEDIA_ROOT>"
	PlaceholderScratchDir = "<SCRATCH_DIR>"
	PlaceholderQuarantine = "<QUARANTINE_DIR>"
	PlaceholderHomeDir    = "<HOME>"
	PlaceholderSystemRoot = "<SYSTEM_ROOT>"
)

// PathScrubber context holds optional directories and identifiers used to scrub
// host filesystem paths from telemetry, traces, and error messages.
type PathScrubber struct {
	MediaDir       string
	ScratchDir     string
	QuarantineDir  string
	HomeDir        string
	InputPath      string
	RelativePath   string
	StripMediaRoot bool
}

var (
	reUnixHome    = regexp.MustCompile(`(^|[\s:'",;=\(\[])(/(?:home|Users)/[^/\s:'",;\\<>()\[\]]+)`)
	reWinHome     = regexp.MustCompile(`(?i)(^|[\s:'",;=\(\[])([A-Za-z]:[/\\]Users[/\\][^/\\\s:'",;\\<>()\[\]]+)`)
	reScratch     = regexp.MustCompile(`(?i)(^|[\s:'",;=\(\[])((?:[A-Za-z]:)?[^:\s'",;=<>()\[\]]*[/\\](?:scratch|\.scratch))`)
	reQuarantine  = regexp.MustCompile(`(?i)(^|[\s:'",;=\(\[])((?:[A-Za-z]:)?[^:\s'",;=<>()\[\]]*[/\\](?:quarantine|\.quarantine))`)
	reMedia       = regexp.MustCompile(`(?i)(^|[\s:'",;=\(\[])((?:[A-Za-z]:)?[^:\s'",;=<>()\[\]]*[/\\](?:media|movies|tv|videos|music|shares|library))`)
	reGenericUnix = regexp.MustCompile(`(^|[\s:'",;=\(\[])(/(?:[a-zA-Z0-9_.-]+/)+[a-zA-Z0-9_.-]+)`)
	reGenericWin  = regexp.MustCompile(`(?i)(^|[\s:'",;=\(\[])([A-Za-z]:[/\\](?:[^\s:'",;\\<>()\[\]]+[/\\])+[^\s:'",;\\<>()\[\]]+)`)
)

// Scrub masks local filesystem paths across the input text.
func (s PathScrubber) Scrub(text string) string {
	if text == "" {
		return ""
	}
	res := text

	// 1. Explicit ScratchDir
	if s.ScratchDir != "" {
		clean := strings.TrimRight(filepath.Clean(s.ScratchDir), "/\\")
		if clean != "" && clean != "." && clean != "/" {
			res = strings.ReplaceAll(res, clean, PlaceholderScratchDir)
			res = strings.ReplaceAll(res, strings.ReplaceAll(clean, "\\", "/"), PlaceholderScratchDir)
			res = strings.ReplaceAll(res, strings.ReplaceAll(clean, "/", "\\"), PlaceholderScratchDir)
		}
	}

	// 2. Explicit QuarantineDir
	if s.QuarantineDir != "" {
		clean := strings.TrimRight(filepath.Clean(s.QuarantineDir), "/\\")
		if clean != "" && clean != "." && clean != "/" {
			res = strings.ReplaceAll(res, clean, PlaceholderQuarantine)
			res = strings.ReplaceAll(res, strings.ReplaceAll(clean, "\\", "/"), PlaceholderQuarantine)
			res = strings.ReplaceAll(res, strings.ReplaceAll(clean, "/", "\\"), PlaceholderQuarantine)
		}
	}

	// 3. Explicit InputPath & RelativePath
	if s.InputPath != "" {
		cleanInput := strings.TrimRight(filepath.Clean(s.InputPath), "/\\")
		mediaTarget := PlaceholderMediaRoot
		if s.RelativePath != "" {
			cleanRel := strings.TrimPrefix(strings.ReplaceAll(s.RelativePath, "\\", "/"), "/")
			mediaTarget = PlaceholderMediaRoot + "/" + cleanRel
			normInput := strings.ReplaceAll(cleanInput, "\\", "/")
			if strings.HasSuffix(normInput, cleanRel) {
				rootPrefix := strings.TrimSuffix(normInput, cleanRel)
				rootPrefix = strings.TrimRight(rootPrefix, "/")
				if rootPrefix != "" {
					res = strings.ReplaceAll(res, rootPrefix, PlaceholderMediaRoot)
					res = strings.ReplaceAll(res, strings.ReplaceAll(rootPrefix, "/", "\\"), PlaceholderMediaRoot)
				}
			}
		} else {
			base := filepath.Base(cleanInput)
			mediaTarget = PlaceholderMediaRoot + "/" + base
		}
		res = strings.ReplaceAll(res, cleanInput, mediaTarget)
		res = strings.ReplaceAll(res, strings.ReplaceAll(cleanInput, "\\", "/"), mediaTarget)
		res = strings.ReplaceAll(res, strings.ReplaceAll(cleanInput, "/", "\\"), mediaTarget)
	}

	// 4. Explicit MediaDir
	if s.MediaDir != "" {
		clean := strings.TrimRight(filepath.Clean(s.MediaDir), "/\\")
		if clean != "" && clean != "." && clean != "/" {
			res = strings.ReplaceAll(res, clean, PlaceholderMediaRoot)
			res = strings.ReplaceAll(res, strings.ReplaceAll(clean, "\\", "/"), PlaceholderMediaRoot)
			res = strings.ReplaceAll(res, strings.ReplaceAll(clean, "/", "\\"), PlaceholderMediaRoot)
		}
	}

	// 5. Explicit or dynamic HomeDir
	homeDir := s.HomeDir
	if homeDir == "" {
		if h, err := os.UserHomeDir(); err == nil && h != "" && h != "/" {
			homeDir = h
		}
	}
	if homeDir != "" {
		clean := strings.TrimRight(filepath.Clean(homeDir), "/\\")
		if clean != "" && clean != "." && clean != "/" {
			res = strings.ReplaceAll(res, clean, PlaceholderHomeDir)
			res = strings.ReplaceAll(res, strings.ReplaceAll(clean, "\\", "/"), PlaceholderHomeDir)
			res = strings.ReplaceAll(res, strings.ReplaceAll(clean, "/", "\\"), PlaceholderHomeDir)
		}
	}

	// 6. Generic regex scrubbing for scratch and quarantine
	res = reScratch.ReplaceAllString(res, "${1}"+PlaceholderScratchDir)
	res = reQuarantine.ReplaceAllString(res, "${1}"+PlaceholderQuarantine)

	// 7. Generic regex scrubbing for user home directories
	res = reUnixHome.ReplaceAllString(res, "${1}"+PlaceholderHomeDir)
	res = reWinHome.ReplaceAllString(res, "${1}"+PlaceholderHomeDir)

	// 8. Generic media roots
	res = reMedia.ReplaceAllString(res, "${1}"+PlaceholderMediaRoot)

	// 9. Remaining generic absolute paths
	res = reGenericUnix.ReplaceAllStringFunc(res, func(m string) string {
		sub := reGenericUnix.FindStringSubmatch(m)
		if len(sub) < 3 {
			return m
		}
		prefix := sub[1]
		path := sub[2]
		if strings.HasPrefix(path, "//") {
			return m
		}
		base := filepath.Base(path)
		return prefix + PlaceholderSystemRoot + "/" + base
	})
	res = reGenericWin.ReplaceAllStringFunc(res, func(m string) string {
		sub := reGenericWin.FindStringSubmatch(m)
		if len(sub) < 3 {
			return m
		}
		prefix := sub[1]
		path := sub[2]
		base := filepath.Base(path)
		return prefix + PlaceholderSystemRoot + "\\" + base
	})

	if s.StripMediaRoot {
		res = strings.ReplaceAll(res, PlaceholderMediaRoot+"/", "")
		res = strings.ReplaceAll(res, PlaceholderMediaRoot+"\\", "")
	}

	return res
}

// ScrubCommandArgs scrubs absolute filesystem paths from an argv list.
func ScrubCommandArgs(argv []string, scrubbers ...PathScrubber) []string {
	if len(argv) == 0 {
		return nil
	}
	var s PathScrubber
	if len(scrubbers) > 0 {
		s = scrubbers[0]
	}
	out := make([]string, len(argv))
	for i, arg := range argv {
		out[i] = s.Scrub(arg)
	}
	return out
}

// ScrubFfmpegCommands scrubs absolute filesystem paths from command records.
func ScrubFfmpegCommands(commands []FfmpegCommandRecord, scrubbers ...PathScrubber) []FfmpegCommandRecord {
	if len(commands) == 0 {
		return nil
	}
	var s PathScrubber
	if len(scrubbers) > 0 {
		s = scrubbers[0]
	}
	out := make([]FfmpegCommandRecord, len(commands))
	for i, cmd := range commands {
		out[i] = FfmpegCommandRecord{
			Phase: cmd.Phase,
			Argv:  ScrubCommandArgs(cmd.Argv, s),
			Count: cmd.Count,
		}
	}
	return out
}

// SanitizeErrorMessage scrubs absolute filesystem paths from an error string.
func SanitizeErrorMessage(msg string, scrubbers ...PathScrubber) string {
	if msg == "" {
		return ""
	}
	var s PathScrubber
	if len(scrubbers) > 0 {
		s = scrubbers[0]
	}
	return s.Scrub(msg)
}

// SanitizeFileCompleteError masks local filesystem paths in an error string using
// the provided scrubber context and caps it to MaxFileCompleteErrorLen.
func SanitizeFileCompleteError(msg string, scrubbers ...PathScrubber) string {
	msg = SanitizeErrorMessage(msg, scrubbers...)
	if len(msg) <= MaxFileCompleteErrorLen {
		return msg
	}
	return msg[:MaxFileCompleteErrorLen] + "… (truncated)"
}
