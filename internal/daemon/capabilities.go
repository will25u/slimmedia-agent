package daemon

import (
	"time"

	"github.com/will25u/slimmedia-agent/internal/ffmpeg"
	"github.com/will25u/slimmedia-agent/internal/hardware"
	"github.com/will25u/slimmedia-agent/internal/safety"
)

// FFmpegCaps describes the FFmpeg encode/decode capabilities reported to the server.
type FFmpegCaps struct {
	// Software encoders
	HasLibx264   bool `json:"has_libx264"`
	HasLibx265   bool `json:"has_libx265"`
	HasLibaomAV1 bool `json:"has_libaom_av1"`
	HasLibsvtAV1 bool `json:"has_libsvtav1"`
	// NVIDIA NVENC
	HasNvencH264 bool `json:"has_nvenc_h264"`
	HasNvencHEVC bool `json:"has_nvenc_hevc"`
	HasNvencAV1  bool `json:"has_nvenc_av1"`
	// Intel QSV
	HasQSVH264 bool `json:"has_qsv_h264"`
	HasQSVHEVC bool `json:"has_qsv_hevc"`
	HasQSVAV1  bool `json:"has_qsv_av1"`
	// AMD AMF
	HasAMFH264 bool `json:"has_amf_h264"`
	HasAMFHEVC bool `json:"has_amf_hevc"`
	HasAMFAV1  bool `json:"has_amf_av1"`
	// VideoToolbox (macOS)
	HasVTH264 bool `json:"has_vt_h264"`
	HasVTHEVC bool `json:"has_vt_hevc"`
	// AV1 hardware encode, verified by a canary encode on the agent machine
	HasNvencAV1Verified bool `json:"has_nvenc_av1_verified"`
	HasQSVAV1Verified   bool `json:"has_qsv_av1_verified"`
	HasAMFAV1Verified   bool `json:"has_amf_av1_verified"`
	// Advanced tuning flags, verified by a canary encode on the agent machine
	HasNvencTuningVerified    bool `json:"has_nvenc_tuning_verified"`
	HasNvencAV1TuningVerified bool `json:"has_nvenc_av1_tuning_verified"`
	HasQSVHEVCTuningVerified  bool `json:"has_qsv_tuning_verified"`
	HasQSVH264TuningVerified  bool `json:"has_qsv_h264_tuning_verified"`
	HasQSVAV1TuningVerified   bool `json:"has_qsv_av1_tuning_verified"`
	HasAMFTuningVerified     bool `json:"has_amf_tuning_verified"`
	// Archivist-grade QSV package verification (per-flag canaries)
	HasQSVExtBRCVerified     bool `json:"has_qsv_extbrc_verified"`
	HasQSVAdaptiveIBVerified bool `json:"has_qsv_adaptive_ib_verified"`
	// Hardware acceleration backends
	HasHWAccelCUDA    bool `json:"has_hwaccel_cuda,omitempty"`
	HasHWAccelVAAPI   bool `json:"has_hwaccel_vaapi,omitempty"`
	HasHWAccelD3D11VA bool `json:"has_hwaccel_d3d11va,omitempty"`
	HasHWAccelQSV     bool `json:"has_hwaccel_qsv,omitempty"`
	// Hardware decode verification via canaries
	HasHWDecodeHEVC10Verified bool `json:"has_hwdecode_hevc10_verified,omitempty"`
	HasHWDecodeAV1Verified    bool `json:"has_hwdecode_av1_verified,omitempty"`
	// Hardware HDR tone-mapping filter verification via canaries
	HasTonemapOpenCLVerified bool `json:"has_tonemap_opencl_verified,omitempty"`
	HasTonemapCUDAVerified   bool `json:"has_tonemap_cuda_verified,omitempty"`
	HasTonemapVAAPIVerified  bool `json:"has_tonemap_vaapi_verified,omitempty"`
	HasHWTonemapVerified     bool `json:"has_hwtonemap_verified,omitempty"`
	// Recommended preset for this system
	RecommendedEncoder string  `json:"recommended_encoder"`
	RecommendedPreset  string  `json:"recommended_preset"`
	RecommendedCRF     int     `json:"recommended_crf"`
	EstimatedFPS       float64 `json:"estimated_fps"`
}

// AuditReport is the hardware/FFmpeg/media capabilities report the agent
// sends to the dashboard. The interactive `audit` command fills the media
// section; the daemon sends the report with only hardware, FFmpeg, and
// media-server sections populated.
type AuditReport struct {
	AgentVersion string `json:"agent_version"`
	ScannedAt    string `json:"scanned_at"`
	Hardware     struct {
		OS struct {
			Name string `json:"name,omitempty"`
		} `json:"os"`
		CPU struct {
			Model         string `json:"model,omitempty"`
			Arch          string `json:"arch"`
			PhysicalCores int    `json:"physical_cores,omitempty"`
			LogicalCores  int    `json:"logical_cores,omitempty"`
			HasAVX        bool   `json:"has_avx"`
			HasAVX2       bool   `json:"has_avx2"`
			HasAVX512     bool   `json:"has_avx512,omitempty"`
			// Topology reports the detected core/die layout so the
			// dashboard can see why software encodes are (or are not)
			// being steered (issue #201).
			Topology *CPUTopologyReport `json:"topology,omitempty"`
		} `json:"cpu"`
		GPU struct {
			Model             string `json:"model,omitempty"`
			Architecture      string `json:"architecture,omitempty"`
			HasNvidia         bool   `json:"has_nvidia"`
			HasIntel          bool   `json:"has_intel"`
			HasAMD            bool   `json:"has_amd"`
			DedicatedVRAMMB   uint64 `json:"dedicated_vram_mb,omitempty"`
			SharedSystemMemMB uint64 `json:"shared_system_mem_mb,omitempty"`
			TotalMemoryMB     uint64 `json:"total_memory_mb,omitempty"`
		} `json:"gpu"`
		Memory struct {
			TotalGB float64 `json:"total_gb,omitempty"`
			FreeGB  float64 `json:"free_gb,omitempty"`
		} `json:"memory"`
		MediaDisk struct {
			Mount   string  `json:"mount,omitempty"`
			TotalGB float64 `json:"total_gb,omitempty"`
			FreeGB  float64 `json:"free_gb,omitempty"`
		} `json:"media_disk"`
		Tier    string `json:"tier"`
		TierMsg string `json:"tier_message"`
	} `json:"hardware"`
	FFmpeg FFmpegCaps `json:"ffmpeg"`
	Media  struct {
		TargetDirectory         string         `json:"target_directory"`
		FilesAnalyzed           int            `json:"files_analyzed"`
		TotalSourceDurationHrs  float64        `json:"total_source_duration_hours"`
		TotalSizeGB             float64        `json:"total_size_gb"`
		EstimatedSavingsGB      float64        `json:"estimated_savings_gb"`
		EstimatedSavingsPercent float64        `json:"estimated_savings_percent"`
		EstimatedTranscodeHours float64        `json:"estimated_transcode_hours,omitempty"`
		ScanElapsedSeconds      float64        `json:"scan_elapsed_seconds,omitempty"`
		CodecBreakdown          map[string]int `json:"codec_breakdown,omitempty"`
	} `json:"media"`
	// MediaServers reports which media servers (Plex/Jellyfin/Emby) were
	// detected on the agent machine at probe time, so the dashboard knows
	// which stream-safety checks are active.
	MediaServers struct {
		Plex     safety.ServerPresence `json:"plex"`
		Jellyfin safety.ServerPresence `json:"jellyfin"`
		Emby     safety.ServerPresence `json:"emby"`
	} `json:"media_servers"`
}

// CPUTopologyReport is the audit-report view of the agent's detected CPU
// core/die layout (issue #201). Symmetric means "no steering needed".
type CPUTopologyReport struct {
	Vendor           string `json:"vendor,omitempty"`
	Hybrid           bool   `json:"hybrid"`
	PerformanceCores int    `json:"performance_cores,omitempty"`
	EfficiencyCores  int    `json:"efficiency_cores,omitempty"`
	CCDs             int    `json:"ccds,omitempty"`
	NUMANodes        int    `json:"numa_nodes,omitempty"`
	Symmetric        bool   `json:"symmetric"`
}

// topologyReport renders a detected topology for the audit report. It is
// always populated; a zero (undetected) topology reports as not symmetric
// with every field empty, which steering treats as "leave encodes
// untouched".
func topologyReport(topo hardware.CPUTopology) *CPUTopologyReport {
	return &CPUTopologyReport{
		Vendor:           topo.Vendor,
		Hybrid:           topo.Hybrid,
		PerformanceCores: topo.PerformanceCores,
		EfficiencyCores:  topo.EfficiencyCores,
		CCDs:             topo.CCDs,
		NUMANodes:        topo.NUMANodes,
		Symmetric:        topo.Symmetric,
	}
}

func toFFmpegCaps(enc ffmpeg.EncoderCaps) FFmpegCaps {
	return FFmpegCaps{
		HasLibx264:                enc.HasLibx264,
		HasLibx265:                enc.HasLibx265,
		HasLibaomAV1:              enc.HasLibaomAV1,
		HasLibsvtAV1:              enc.HasLibsvtAV1,
		HasNvencH264:              enc.HasNvencH264,
		HasNvencHEVC:              enc.HasNvencHEVC,
		HasNvencAV1:               enc.HasNvencAV1,
		HasQSVH264:                enc.HasQSVH264,
		HasQSVHEVC:                enc.HasQSVHEVC,
		HasQSVAV1:                 enc.HasQSVAV1,
		HasAMFH264:                enc.HasAMFH264,
		HasAMFHEVC:                enc.HasAMFHEVC,
		HasAMFAV1:                 enc.HasAMFAV1,
		HasVTH264:                 enc.HasVTH264,
		HasVTHEVC:                 enc.HasVTHEVC,
		HasNvencAV1Verified:       enc.HasNvencAV1Verified,
		HasQSVAV1Verified:         enc.HasQSVAV1Verified,
		HasAMFAV1Verified:         enc.HasAMFAV1Verified,
		HasNvencTuningVerified:    enc.HasNvencTuningVerified,
		HasNvencAV1TuningVerified: enc.HasNvencAV1TuningVerified,
		HasQSVHEVCTuningVerified:  enc.HasQSVHEVCTuningVerified,
		HasQSVH264TuningVerified:  enc.HasQSVH264TuningVerified,
		HasQSVAV1TuningVerified:   enc.HasQSVAV1TuningVerified,
		HasAMFTuningVerified:      enc.HasAMFTuningVerified,
		HasQSVExtBRCVerified:      enc.HasQSVExtBRCVerified,
		HasQSVAdaptiveIBVerified:  enc.HasQSVAdaptiveIBVerified,
		HasHWAccelCUDA:            enc.HasHWAccelCUDA,
		HasHWAccelVAAPI:           enc.HasHWAccelVAAPI,
		HasHWAccelD3D11VA:         enc.HasHWAccelD3D11VA,
		HasHWAccelQSV:             enc.HasHWAccelQSV,
		HasHWDecodeHEVC10Verified: enc.HasHWDecodeHEVC10Verified,
		HasHWDecodeAV1Verified:    enc.HasHWDecodeAV1Verified,
		HasTonemapOpenCLVerified:  enc.HasTonemapOpenCLVerified,
		HasTonemapCUDAVerified:    enc.HasTonemapCUDAVerified,
		HasTonemapVAAPIVerified:   enc.HasTonemapVAAPIVerified,
		HasHWTonemapVerified:      enc.HasHWTonemapVerified,
		RecommendedEncoder:        enc.RecommendedEncoder,
		RecommendedPreset:         enc.RecommendedPreset,
		RecommendedCRF:            enc.RecommendedCRF,
		EstimatedFPS:              enc.EstimatedFPS,
	}
}

// BuildCapabilitiesReport assembles the hardware, FFmpeg, and media-server
// sections of an AuditReport from live probes. The media section is only
// populated by the interactive `audit` command; the daemon uses this for its
// automatic pre-connect capabilities report.
func BuildCapabilitiesReport(caps hardware.Capabilities, encCaps ffmpeg.EncoderCaps, presence safety.MediaServerPresence) AuditReport {
	encCaps.StripUnverified(caps.GPU.HasNvidia, caps.GPU.HasIntel, caps.GPU.HasAMD)
	assessment := DetermineTier(caps, encCaps)

	report := AuditReport{}
	report.AgentVersion = Version
	report.ScannedAt = time.Now().UTC().Format(time.RFC3339)

	report.Hardware.OS.Name = caps.OS.Name

	report.Hardware.CPU.Model = caps.CPU.Model
	report.Hardware.CPU.Arch = caps.CPU.Arch
	report.Hardware.CPU.PhysicalCores = caps.CPU.PhysicalCores
	report.Hardware.CPU.LogicalCores = caps.CPU.LogicalCores
	report.Hardware.CPU.HasAVX = caps.CPU.HasAVX
	report.Hardware.CPU.HasAVX2 = caps.CPU.HasAVX2
	report.Hardware.CPU.Topology = topologyReport(caps.CPU.Topology)

	report.Hardware.GPU.Model = caps.GPU.Model
	report.Hardware.GPU.Architecture = caps.GPU.Architecture
	report.Hardware.GPU.HasNvidia = caps.GPU.HasNvidia
	report.Hardware.GPU.HasIntel = caps.GPU.HasIntel
	report.Hardware.GPU.HasAMD = caps.GPU.HasAMD
	report.Hardware.GPU.DedicatedVRAMMB = caps.GPU.DedicatedVRAMMB
	report.Hardware.GPU.SharedSystemMemMB = caps.GPU.SharedSystemMemMB
	report.Hardware.GPU.TotalMemoryMB = caps.GPU.TotalMemoryMB

	report.Hardware.Memory.TotalGB = caps.Memory.TotalGB

	report.Hardware.Tier = string(assessment.Tier)
	report.Hardware.TierMsg = assessment.Message

	report.FFmpeg = toFFmpegCaps(encCaps)

	report.MediaServers.Plex = presence.Plex
	report.MediaServers.Jellyfin = presence.Jellyfin
	report.MediaServers.Emby = presence.Emby
	return report
}
