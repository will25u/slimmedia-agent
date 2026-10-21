package protocol

import "fmt"

// VendorTuning carries a user's per-engine hardware encoder tuning
// selections (issue #195). Pointer fields distinguish "unset" (the agent's
// default vendor package applies for that flag) from an explicit selection —
// including an explicit false, which omits the flag. A nil or empty
// *VendorTuning means the agent's canary-verified default package applies
// unchanged.
//
// NVENC fields map to hevc_nvenc argv; QSV fields map to hevc_qsv argv (the
// h264_qsv/av1_qsv sets are subsets). The agent renders selected flags
// through the same canary gate as the default packages.
type VendorTuning struct {
	// NVENC
	Tune       *string `json:"tune,omitempty"`        // -tune
	Multipass  *string `json:"multipass,omitempty"`   // -multipass
	SpatialAQ  *bool   `json:"spatial_aq,omitempty"`  // -spatial-aq
	TemporalAQ *bool   `json:"temporal_aq,omitempty"` // -temporal-aq
	BRefMode   *string `json:"b_ref_mode,omitempty"`  // -b_ref_mode

	// QSV
	Extbrc         *bool `json:"extbrc,omitempty"`           // -extbrc
	LookAhead      *bool `json:"look_ahead,omitempty"`       // -look_ahead
	LookAheadDepth *int  `json:"look_ahead_depth,omitempty"` // -look_ahead_depth
	BStrategy      *bool `json:"b_strategy,omitempty"`       // -b_strategy
	AdaptiveB      *bool `json:"adaptive_b,omitempty"`       // -adaptive_b
	AdaptiveI      *bool `json:"adaptive_i,omitempty"`       // -adaptive_i

	// CPU x265
	AQStrength *string `json:"aq_strength,omitempty"`
	BFrames    *int    `json:"bframes,omitempty"`
	Deblock    *string `json:"deblock,omitempty"`

	// CPU SVT-AV1
	SvtTune            *int `json:"svt_tune,omitempty"`
	FilmGrain          *int `json:"film_grain,omitempty"`
	HierarchicalLevels *int `json:"hierarchical_levels,omitempty"`
	FastDecode         *int `json:"fast_decode,omitempty"`
}

// Allowed values for the string-select vendor tuning fields. Shared by the
// API's preset validation and the frontend's control options.
var VendorTuningSelectValues = map[string]map[string]bool{
	"tune":        {"hq": true, "ll": true, "ull": true, "lossless": true},
	"multipass":   {"single": true, "quarterres": true, "fullres": true},
	"b_ref_mode":  {"disabled": true, "each": true, "middle": true},
	"aq_strength": {"0.8": true, "1.0": true, "1.2": true, "1.5": true},
	"deblock":     {"0:0": true, "-1:-1": true, "-2:-2": true, "1:1": true},
}

// VendorTuningKnownKeys is the set of recognized vendor tuning JSON keys.
var VendorTuningKnownKeys = map[string]bool{
	"tune": true, "multipass": true, "spatial_aq": true, "temporal_aq": true,
	"b_ref_mode": true,
	"extbrc":     true, "look_ahead": true, "look_ahead_depth": true,
	"b_strategy": true, "adaptive_b": true, "adaptive_i": true,
	"aq_strength": true, "bframes": true, "deblock": true,
	"svt_tune": true, "film_grain": true, "hierarchical_levels": true, "fast_decode": true,
}

// Empty reports whether no field carries an explicit selection.
func (vt *VendorTuning) Empty() bool {
	return vt == nil ||
		(vt.Tune == nil && vt.Multipass == nil && vt.SpatialAQ == nil &&
			vt.TemporalAQ == nil && vt.BRefMode == nil &&
			vt.Extbrc == nil && vt.LookAhead == nil && vt.LookAheadDepth == nil &&
			vt.BStrategy == nil && vt.AdaptiveB == nil && vt.AdaptiveI == nil &&
			vt.AQStrength == nil && vt.BFrames == nil && vt.Deblock == nil &&
			vt.SvtTune == nil && vt.FilmGrain == nil && vt.HierarchicalLevels == nil && vt.FastDecode == nil)
}

// ValidateVendorTuning checks every explicitly set field against its
// allowlist. Engine-foreign keys are tolerated (a preset may carry stale
// tuning from a previous engine selection); unknown keys and out-of-range
// values are rejected.
func ValidateVendorTuning(vt *VendorTuning) error {
	if vt == nil {
		return nil
	}
	if vt.Tune != nil && !VendorTuningSelectValues["tune"][*vt.Tune] {
		return fmt.Errorf("invalid vendor tuning tune %q: must be one of hq, ll, ull, lossless", *vt.Tune)
	}
	if vt.Multipass != nil && !VendorTuningSelectValues["multipass"][*vt.Multipass] {
		return fmt.Errorf("invalid vendor tuning multipass %q: must be one of single, quarterres, fullres", *vt.Multipass)
	}
	if vt.BRefMode != nil && !VendorTuningSelectValues["b_ref_mode"][*vt.BRefMode] {
		return fmt.Errorf("invalid vendor tuning b_ref_mode %q: must be one of disabled, each, middle", *vt.BRefMode)
	}
	if vt.LookAheadDepth != nil && (*vt.LookAheadDepth < 1 || *vt.LookAheadDepth > 100) {
		return fmt.Errorf("invalid vendor tuning look_ahead_depth %d: must be between 1 and 100", *vt.LookAheadDepth)
	}
	if vt.AQStrength != nil && !VendorTuningSelectValues["aq_strength"][*vt.AQStrength] {
		return fmt.Errorf("invalid vendor tuning aq_strength %q: must be one of 0.8, 1.0, 1.2, 1.5", *vt.AQStrength)
	}
	if vt.Deblock != nil && !VendorTuningSelectValues["deblock"][*vt.Deblock] {
		return fmt.Errorf("invalid vendor tuning deblock %q: must be one of 0:0, -1:-1, -2:-2, 1:1", *vt.Deblock)
	}
	if vt.BFrames != nil && (*vt.BFrames < 0 || *vt.BFrames > 16) {
		return fmt.Errorf("invalid vendor tuning bframes %d: must be between 0 and 16", *vt.BFrames)
	}
	if vt.SvtTune != nil && (*vt.SvtTune < 0 || *vt.SvtTune > 2) {
		return fmt.Errorf("invalid vendor tuning svt_tune %d: must be between 0 and 2", *vt.SvtTune)
	}
	if vt.FilmGrain != nil && (*vt.FilmGrain < 0 || *vt.FilmGrain > 50) {
		return fmt.Errorf("invalid vendor tuning film_grain %d: must be between 0 and 50", *vt.FilmGrain)
	}
	if vt.HierarchicalLevels != nil && (*vt.HierarchicalLevels < 3 || *vt.HierarchicalLevels > 5) {
		return fmt.Errorf("invalid vendor tuning hierarchical_levels %d: must be between 3 and 5", *vt.HierarchicalLevels)
	}
	if vt.FastDecode != nil && (*vt.FastDecode < 0 || *vt.FastDecode > 1) {
		return fmt.Errorf("invalid vendor tuning fast_decode %d: must be 0 or 1", *vt.FastDecode)
	}
	return nil
}
