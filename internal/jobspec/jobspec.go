// Package jobspec owns the single definition of a transcode job's
// preset-tuning options. The same dozen fields used to be copied
// field-by-field from the START_JOB wire payload into an orchestrator
// options struct and again into the runner's job struct; JobSpec is built
// once at the dispatch boundary (via FromWire) and passed down intact.
package jobspec

import (
	"fmt"

	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// JobSpec is the complete set of preset-tuning options for one transcode
// job: everything the ffmpeg argv builder needs to honour a preset, and
// nothing else. Execution-only data (binary path, probed capabilities,
// stream probes, scheduling/pause hooks, derived paths) lives on the runner
// and orchestrator structs, not here.
//
// Adding a new tuning field touches this definition, the FromWire mapping,
// and the protocol.StartJobPayload wire struct — nothing else.
type JobSpec struct {
	// Version is the wire spec_version the spec was built from. Zero means
	// a legacy pre-tuning payload; see Legacy.
	Version int

	CRF         int
	PresetSpeed string
	AudioCodec  string

	// Advanced preset tuning (protocol migration 000012 fields); zero-valued
	// for pre-tuning payloads.
	Container        string
	EncoderEngine    string
	QualityTarget    int
	AudioHandling    string
	SubtitleHandling string
	ColorDepth       string
	FilmGrainSao     bool
	AqMode           int
	ContentTuning    string
	CustomFlags      string

	// VendorTuning carries user-selected hardware encoder tuning (issue
	// #195); nil (or empty) means the agent's default vendor package applies.
	// Archivist-grade jobs still replace it with the code-owned package.
	VendorTuning *protocol.VendorTuning

	// QualityTier selects a code-owned per-vendor quality package on the
	// agent (protocol.QualityTierArchivist); empty means plain preset
	// values apply at every encoder fallback step.
	QualityTier string

	// Tier 2 User Global Hygiene
	PreferredAudioLanguages    []string
	AudioLanguageMode          string
	SubtitleFormatPolicy       string
	ForcedSubtitlePolicy       string
	PreferredSubtitleLanguages []string
	SubtitleLanguageMode       string
	StripMetadata              bool

	// Tier 3 Assembly & Disposition Policies
	OutputFileStrategy string
	ActionOnSuccess    string

	// Chunking selects the pipeline shape. "auto" (the zero value) chunks
	// when the resolved encoder is software and runs a single full-stream
	// pass when it is hardware; ChunkingChunked and ChunkingSinglePass force
	// either shape for tests and future presets. Not mapped from the wire
	// payload — a server-side knob would be added deliberately, not by
	// default — and intentionally absent from any DB column.
	Chunking string

	// PostTranscodeVerification opts the job into the post-encode VMAF spot
	// check (original vs output over a bounded sample). False means the
	// agent runs the existing gates (size ratio, decodability, duration,
	// A/V alignment) only. Mapped from protocol.ExecutionConfig.
	PostTranscodeVerification bool

	RelativePath string
	LocalPath    string
}

// Pipeline-shape override values for JobSpec.Chunking.
const (
	// ChunkingAuto is the zero value: the pipeline shape is derived from the
	// resolved encoder (software → chunked, hardware → single pass).
	ChunkingAuto = ""
	// ChunkingChunked forces the split-first chunked pipeline.
	ChunkingChunked = "chunked"
	// ChunkingSinglePass forces one full-stream encode with no demux/split
	// phases and no segment directory.
	ChunkingSinglePass = "single_pass"
)

// ChunkingOverride reports whether the spec forces a pipeline shape,
// independent of the resolved encoder: (chunked, true) when it does.
// (false, false) means "auto" — the caller derives the shape.
func (s JobSpec) ChunkingOverride() (chunked bool, ok bool) {
	switch s.Chunking {
	case ChunkingChunked:
		return true, true
	case ChunkingSinglePass:
		return false, true
	default:
		return false, false
	}
}

// FromWire builds a JobSpec from a parsed START_JOB payload. This is the
// only copy boundary between the wire representation and the agent's
// internal job representation.
func FromWire(p protocol.StartJobPayload) (JobSpec, error) {
	outputStrategy := p.OutputFileStrategy
	if outputStrategy == "" && p.ContainerConfig != nil {
		outputStrategy = p.ContainerConfig.OutputFileStrategy
	}
	if outputStrategy == "" {
		outputStrategy = "alongside"
	}
	if !protocol.IsValidOutputFileStrategy(outputStrategy) {
		return JobSpec{}, fmt.Errorf("invalid output_file_strategy %q: must be one of 'alongside', 'replace', 'alternate'", outputStrategy)
	}

	actionSuccess := p.ActionOnSuccess
	if actionSuccess == "" && p.ContainerConfig != nil {
		actionSuccess = p.ContainerConfig.ActionOnSuccess
	}
	if actionSuccess == "" {
		actionSuccess = "keep"
	}
	if !protocol.IsValidActionOnSuccess(actionSuccess) {
		return JobSpec{}, fmt.Errorf("invalid action_on_success %q: must be one of 'keep', 'trash', 'delete', 'archive'", actionSuccess)
	}

	vendorTuning, err := vendorTuningFromWire(p)
	if err != nil {
		return JobSpec{}, err
	}

	return JobSpec{
		Version:                    p.SpecVersion,
		CRF:                        p.CRF,
		PresetSpeed:                p.PresetSpeed,
		AudioCodec:                 p.AudioCodec,
		Container:                  p.Container,
		EncoderEngine:              p.EncoderEngine,
		QualityTarget:              p.QualityTarget,
		AudioHandling:              p.AudioHandling,
		SubtitleHandling:           p.SubtitleHandling,
		ColorDepth:                 p.ColorDepth,
		FilmGrainSao:               p.FilmGrainSao,
		AqMode:                     p.AqMode,
		ContentTuning:              p.ContentTuning,
		CustomFlags:                p.CustomFlags,
		VendorTuning:               vendorTuning,
		QualityTier:                qualityTierFromWire(p),
		PreferredAudioLanguages:    append([]string{}, p.PreferredAudioLanguages...),
		AudioLanguageMode:          p.AudioLanguageMode,
		SubtitleFormatPolicy:       p.SubtitleFormatPolicy,
		ForcedSubtitlePolicy:       p.ForcedSubtitlePolicy,
		PreferredSubtitleLanguages: append([]string{}, p.PreferredSubtitleLanguages...),
		SubtitleLanguageMode:       p.SubtitleLanguageMode,
		StripMetadata:              p.StripMetadata,
		OutputFileStrategy:         outputStrategy,
		ActionOnSuccess:            actionSuccess,
		PostTranscodeVerification:  postTranscodeVerificationFromWire(p),
		RelativePath:               p.RelativePath,
		LocalPath:                  p.LocalPath,
	}, nil
}

// postTranscodeVerificationFromWire reads the opt-in flag from the wire
// Execution section; pre-v2 payloads carry no Execution section and default
// to false.
func postTranscodeVerificationFromWire(p protocol.StartJobPayload) bool {
	return p.Execution != nil && p.Execution.PostTranscodeVerification
}

// vendorTuningFromWire resolves the user-selected hardware vendor tuning,
// tolerating callers that populate only the structured Video section or only
// the flat top-level field. Out-of-allowlist values are rejected: the API
// validates at write time, and the agent re-validates at the trust boundary
// so a compromised or buggy server cannot inject arbitrary argv.
func vendorTuningFromWire(p protocol.StartJobPayload) (*protocol.VendorTuning, error) {
	vt := p.VendorTuning
	if vt == nil && p.Video != nil {
		vt = p.Video.VendorTuning
	}
	if vt.Empty() {
		return nil, nil
	}
	if err := protocol.ValidateVendorTuning(vt); err != nil {
		return nil, fmt.Errorf("invalid vendor_tuning: %w", err)
	}
	return vt, nil
}

// qualityTierFromWire resolves the code-owned quality tier, tolerating
// callers that populate only the structured Video section or only the
// flat top-level field.
func qualityTierFromWire(p protocol.StartJobPayload) string {
	if p.QualityTier != "" {
		return p.QualityTier
	}
	if p.Video != nil {
		return p.Video.QualityTier
	}
	return ""
}

// Legacy reports whether the spec came from a pre-tuning wire payload
// (no spec_version field). Legacy jobs keep the historic single-pass,
// no-probe behavior when no pause/schedule controls are attached; this
// replaces the old field-sniffing heuristic.
func (s JobSpec) Legacy() bool {
	return s.Version == 0
}
