package protocol

import (
	"fmt"
	"strings"
)

// InvariantScope defines the FFmpeg execution shape where an invariant is locked.
type InvariantScope string

const (
	ScopeEncodeAll   InvariantScope = "encode"
	ScopeEncodeChunk InvariantScope = "encode_chunk"
	ScopeSplit       InvariantScope = "split"
	ScopeDemux       InvariantScope = "demux"
	ScopeConcat      InvariantScope = "concat"
)

// ImmutabilityClass defines the classification and failure mode of an invariant.
type ImmutabilityClass string

const (
	ClassHardImmutable     ImmutabilityClass = "HARD_IMMUTABLE"
	ClassSystemLocked      ImmutabilityClass = "SYSTEM_LOCKED"
	ClassPhaseLocked       ImmutabilityClass = "PHASE_LOCKED"
	ClassContainerLocked   ImmutabilityClass = "CONTAINER_LOCKED"
	ClassTier2Coordinated  ImmutabilityClass = "TIER2_COORDINATED"
)

// InvariantEntry represents a ratified Tier 1 system invariant rule.
type InvariantEntry struct {
	Flag         string            `json:"flag"`
	PrefixMatch  bool              `json:"prefix_match,omitempty"`
	Scope        InvariantScope    `json:"scope"`
	RequiredVal  string            `json:"required_val,omitempty"`
	Class        ImmutabilityClass `json:"class"`
	RejectReason string            `json:"reject_reason"`
}

// systemInvariantRegistry contains the platform master list of immutable Tier 1 invariants
// ratified in #157.
var systemInvariantRegistry = []InvariantEntry{
	{
		Flag:         "-hide_banner",
		Scope:        ScopeEncodeAll,
		Class:        ClassHardImmutable,
		RejectReason: "Suppresses build/version banners. Omitting causes megabyte-scale log spam and breaks line-oriented output parsers.",
	},
	{
		Flag:         "-nostdin",
		Scope:        ScopeEncodeAll,
		Class:        ClassHardImmutable,
		RejectReason: "Disables stdin interaction. Overriding allows FFmpeg to block waiting for keyboard input, deadlocking worker processes.",
	},
	{
		Flag:         "-y",
		Scope:        ScopeEncodeAll,
		Class:        ClassHardImmutable,
		RejectReason: "Forces overwrite without prompting. Overriding or passing -n causes FFmpeg to halt on prompt in headless container/daemon.",
	},
	{
		Flag:         "-n",
		Scope:        ScopeEncodeAll,
		Class:        ClassHardImmutable,
		RejectReason: "Do not overwrite output files. Overriding causes FFmpeg to halt on prompt in headless container/daemon.",
	},
	{
		Flag:         "-f",
		Scope:        ScopeEncodeAll,
		Class:        ClassHardImmutable,
		RejectReason: "Forces output or input format. Overriding hijacks container format and muxer pipelines.",
	},
	{
		Flag:         "-target",
		Scope:        ScopeEncodeAll,
		Class:        ClassHardImmutable,
		RejectReason: "Specifies target system profile. Overriding disrupts container and stream mappings.",
	},
	{
		Flag:         "-tee",
		Scope:        ScopeEncodeAll,
		Class:        ClassHardImmutable,
		RejectReason: "Tee muxer duplicates output streams. Overriding permits unmanaged multiple outputs.",
	},
	{
		Flag:         "-fflags",
		Scope:        ScopeEncodeAll,
		RequiredVal:  "+genpts",
		Class:        ClassHardImmutable,
		RejectReason: "Reconstructs missing PTS from DTS. Overriding causes playback freezing, broken seek tables, and demuxer timestamp errors.",
	},
	{
		Flag:         "-protocol_whitelist",
		Scope:        ScopeEncodeAll,
		RequiredVal:  "file,pipe",
		Class:        ClassHardImmutable,
		RejectReason: "Sandboxes I/O to local files and pipes. Overriding allows SSRF, arbitrary file reads via http/subfile, and credential theft.",
	},
	{
		Flag:         "-probesize",
		Scope:        ScopeEncodeAll,
		RequiredVal:  "10000000",
		Class:        ClassSystemLocked,
		RejectReason: "Standardized probe window for user source opening. Lowering causes stream drops on high-bitrate TS/M2TS remuxes.",
	},
	{
		Flag:         "-analyzeduration",
		Scope:        ScopeEncodeAll,
		RequiredVal:  "10000000",
		Class:        ClassSystemLocked,
		RejectReason: "Standardized probe duration. Lowering causes sparse subtitle or secondary audio discovery failures.",
	},
	{
		Flag:         "-i",
		Scope:        ScopeEncodeAll,
		Class:        ClassHardImmutable,
		RejectReason: "Input declaration. Injecting additional -i flags hijacks stream indexing, corrupts input mapping, and enables arbitrary file probing.",
	},
	{
		Flag:         "-map",
		PrefixMatch:  true,
		Scope:        ScopeEncodeAll,
		Class:        ClassHardImmutable,
		RejectReason: "Stream topology selection. User injection of -map disrupts deterministic channel assignment, breaks chunk video isolation, and crashes muxers.",
	},
	{
		Flag:         "-ignore_unknown",
		Scope:        ScopeEncodeAll,
		Class:        ClassHardImmutable,
		RejectReason: "Drops unhandled metadata/data streams. Overriding causes FFmpeg to abort when encountering proprietary Blu-ray/telemetry tracks.",
	},
	{
		Flag:         "-max_muxing_queue_size",
		Scope:        ScopeEncodeAll,
		RequiredVal:  "4096",
		Class:        ClassHardImmutable,
		RejectReason: "Prevents muxing queue overflow on bursty streams. Lowering reproduces Too many packets buffered for output stream crashes.",
	},
	{
		Flag:         "-progress",
		Scope:        ScopeEncodeAll,
		RequiredVal:  "pipe:1",
		Class:        ClassHardImmutable,
		RejectReason: "Telemetry pipeline on stdout. Overriding or redirecting breaks daemon progress monitoring and stalls job updates.",
	},
	{
		Flag:         "-nostats",
		Scope:        ScopeEncodeAll,
		Class:        ClassHardImmutable,
		RejectReason: "Suppresses stderr progress rewrites. Overriding floods worker error logs.",
	},
	{
		Flag:         "-fps_mode",
		Scope:        ScopeEncodeChunk,
		Class:        ClassPhaseLocked,
		RejectReason: "Prevents VFR frame decimation in chunk transcode pipelines. Direct overriding drops frames on variable-framerate media.",
	},
	{
		Flag:         "-an",
		Scope:        ScopeEncodeChunk,
		Class:        ClassPhaseLocked,
		RejectReason: "Enforces video-only intermediate chunks. Audio packet overhang causes cumulative PTS drift across chunk seams.",
	},
	{
		Flag:         "-sn",
		Scope:        ScopeEncodeChunk,
		Class:        ClassPhaseLocked,
		RejectReason: "Enforces video-only intermediate chunks. Subtitle packet overhang causes cumulative PTS drift across chunk seams.",
	},
	{
		Flag:         "-dn",
		Scope:        ScopeEncodeChunk,
		Class:        ClassPhaseLocked,
		RejectReason: "Enforces video-only intermediate chunks. Data streams are stripped from chunk transcode intermediates.",
	},
}

// SandboxPrefix describes an argument prefix restricted by Tier 1 process sandboxing.
type SandboxPrefix struct {
	Prefix    string `json:"prefix"`
	Rationale string `json:"rationale"`
}

var securitySandboxPrefixes = []SandboxPrefix{
	{Prefix: "-filter", Rationale: "Filter specification can construct unauthorized filtergraphs."},
	{Prefix: "-vf", Rationale: "Single-input video filtergraph can read arbitrary local files or execute unsafe filters."},
	{Prefix: "-af", Rationale: "Single-input audio filtergraph can read arbitrary local files or execute unsafe filters."},
	{Prefix: "-lavfi", Rationale: "Libavfilter virtual device permits arbitrary filter graph construction."},
	{Prefix: "-dump_attachment", Rationale: "Attachment dumping permits arbitrary disk writes."},
	{Prefix: "-passlogfile", Rationale: "Arbitrary passlog file writing is prohibited by process sandboxing."},
	{Prefix: "-vstats_file", Rationale: "Arbitrary output file writing is prohibited by process sandboxing."},
	{Prefix: "-print_graphs_file", Rationale: "Arbitrary graph file writing is prohibited by process sandboxing."},
}

var securitySandboxTokens = map[string]string{
	"-vf":                "Single-input video filtergraph can read arbitrary local files or execute unsafe filters.",
	"-af":                "Single-input audio filtergraph can read arbitrary local files or execute unsafe filters.",
	"-filter":            "Filter specification can construct unauthorized filtergraphs.",
	"-filter_complex":    "Complex filtergraph permits multi-input mixing and local file smuggling.",
	"-lavfi":             "Libavfilter virtual device permits arbitrary filter graph construction.",
	"-vstats_file":       "Arbitrary output file writing is prohibited by process sandboxing.",
	"-passlogfile":       "Arbitrary passlog file writing is prohibited by process sandboxing.",
	"-print_graphs_file": "Arbitrary graph file writing is prohibited by process sandboxing.",
	"-dump_attachment":   "Attachment dumping permits arbitrary disk writes.",
	"-attach":            "Attachment embedding permits unauthorized local file reads.",
	"-report":            "FFreport generates unmanaged log files on the host filesystem.",
}

// SystemInvariantRegistry returns an immutable copy of the ratified Tier 1 system invariants.
func SystemInvariantRegistry() []InvariantEntry {
	cp := make([]InvariantEntry, len(systemInvariantRegistry))
	copy(cp, systemInvariantRegistry)
	return cp
}

// SecuritySandboxPrefixes returns an immutable copy of Tier 1 security sandbox prefixes.
func SecuritySandboxPrefixes() []SandboxPrefix {
	cp := make([]SandboxPrefix, len(securitySandboxPrefixes))
	copy(cp, securitySandboxPrefixes)
	return cp
}

// SecuritySandboxTokens returns an immutable copy of Tier 1 security sandbox tokens.
func SecuritySandboxTokens() map[string]string {
	cp := make(map[string]string, len(securitySandboxTokens))
	for k, v := range securitySandboxTokens {
		cp[k] = v
	}
	return cp
}

// CollisionReport describes a conflict between user/preset arguments and Tier 1 invariants.
type CollisionReport struct {
	Flag         string            `json:"flag"`
	Shape        string            `json:"shape"`
	Immutability ImmutabilityClass `json:"immutability"`
	Rationale    string            `json:"rationale"`
}

// Error returns a formatted error string conforming to the collision error schema.
func (c *CollisionReport) Error() string {
	return fmt.Sprintf("tier 1 invariant collision: flag %q is immutable in %s shape: %s", c.Flag, c.Shape, c.Rationale)
}

// CheckInvariantCollision evaluates a flag token against the Tier 1 Invariant Registry
// and security sandbox constraints. Returns a *CollisionReport if a violation is detected.
func CheckInvariantCollision(tok string, scope InvariantScope) *CollisionReport {
	trimmed := strings.TrimSpace(tok)
	if trimmed == "" {
		return nil
	}

	if scope == "" {
		scope = ScopeEncodeAll
	}

	baseFlag := trimmed
	if idx := strings.Index(trimmed, "="); idx != -1 {
		baseFlag = trimmed[:idx]
	}
	normFlag := baseFlag
	if idx := strings.Index(normFlag, ":"); idx != -1 {
		normFlag = normFlag[:idx]
	}

	// 1. Check Security Sandbox bypass tokens (prefix matching)
	for _, p := range securitySandboxPrefixes {
		if strings.HasPrefix(baseFlag, p.Prefix) || strings.HasPrefix(normFlag, p.Prefix) {
			return &CollisionReport{
				Flag:         baseFlag,
				Shape:        string(scope),
				Immutability: ClassHardImmutable,
				Rationale:    p.Rationale,
			}
		}
	}

	// Check Security Sandbox bypass tokens (exact map lookup)
	if reason, ok := securitySandboxTokens[baseFlag]; ok {
		return &CollisionReport{
			Flag:         baseFlag,
			Shape:        string(scope),
			Immutability: ClassHardImmutable,
			Rationale:    reason,
		}
	}
	if reason, ok := securitySandboxTokens[normFlag]; ok {
		return &CollisionReport{
			Flag:         baseFlag,
			Shape:        string(scope),
			Immutability: ClassHardImmutable,
			Rationale:    reason,
		}
	}

	// 2. Check Registry entries
	for _, entry := range systemInvariantRegistry {
		// If entry has a scope that is not generic ScopeEncodeAll, ensure it matches requested scope
		matches := false
		if entry.Scope == "" || entry.Scope == scope {
			matches = true
		} else if entry.Scope == ScopeEncodeAll && (scope == ScopeEncodeAll || scope == ScopeEncodeChunk) {
			matches = true
		}
		if !matches {
			continue
		}

		if entry.PrefixMatch {
			if strings.HasPrefix(baseFlag, entry.Flag) || strings.HasPrefix(normFlag, entry.Flag) {
				return &CollisionReport{
					Flag:         baseFlag,
					Shape:        string(scope),
					Immutability: entry.Class,
					Rationale:    entry.RejectReason,
				}
			}
		} else {
			if baseFlag == entry.Flag || normFlag == entry.Flag {
				return &CollisionReport{
					Flag:         baseFlag,
					Shape:        string(scope),
					Immutability: entry.Class,
					Rationale:    entry.RejectReason,
				}
			}
		}
	}

	return nil
}

// VerifyDefenseInDepth iterates through the final command-line arguments and
// ensures that all required invariants are present and unmodified, and that no sandbox-violating
// tokens have slipped into the final assembled args.
func VerifyDefenseInDepth(args []string, scope InvariantScope) error {
	if scope == "" {
		scope = ScopeEncodeAll
	}

	flagVals := make(map[string][]string)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		trimmed := strings.TrimSpace(arg)
		if strings.HasPrefix(trimmed, "-") {
			baseFlag := trimmed
			var val string
			if idx := strings.Index(trimmed, "="); idx != -1 {
				baseFlag = trimmed[:idx]
				val = trimmed[idx+1:]
			} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				val = strings.TrimSpace(args[i+1])
				i++
			}
			flagVals[baseFlag] = append(flagVals[baseFlag], val)
		}
	}

	for _, arg := range args {
		trimmed := strings.TrimSpace(arg)
		if !strings.HasPrefix(trimmed, "-") {
			continue
		}
		baseFlag := trimmed
		if idx := strings.Index(trimmed, "="); idx != -1 {
			baseFlag = trimmed[:idx]
		}
		normFlag := baseFlag
		if idx := strings.Index(normFlag, ":"); idx != -1 {
			normFlag = normFlag[:idx]
		}

		for _, p := range securitySandboxPrefixes {
			if strings.HasPrefix(baseFlag, p.Prefix) || strings.HasPrefix(normFlag, p.Prefix) {
				return fmt.Errorf("defense-in-depth: sandbox violation detected in final args: %s", baseFlag)
			}
		}
		if _, ok := securitySandboxTokens[baseFlag]; ok {
			return fmt.Errorf("defense-in-depth: sandbox violation detected in final args: %s", baseFlag)
		}
		if _, ok := securitySandboxTokens[normFlag]; ok {
			return fmt.Errorf("defense-in-depth: sandbox violation detected in final args: %s", baseFlag)
		}
	}

	for _, entry := range systemInvariantRegistry {
		matches := false
		if entry.Scope == "" || entry.Scope == scope {
			matches = true
		} else if entry.Scope == ScopeEncodeAll && (scope == ScopeEncodeAll || scope == ScopeEncodeChunk) {
			matches = true
		}
		if !matches {
			continue
		}

		if entry.RequiredVal != "" {
			vals, present := flagVals[entry.Flag]
			if !present {
				return fmt.Errorf("defense-in-depth invariant failure: required flag %q is missing", entry.Flag)
			}
			for _, v := range vals {
				if v != entry.RequiredVal {
					return fmt.Errorf("defense-in-depth invariant failure: required flag %q with value %q was altered to %q", entry.Flag, entry.RequiredVal, v)
				}
			}
		}
	}
	return nil
}
