package protocol

import (
	"strings"
)

// HDR10ProtectedKeys contains probed HDR10 metadata and colorimetry attributes
// that must be strictly preserved against custom flag overwrites during compound flag merging.
var HDR10ProtectedKeys = map[string]bool{
	"master-display": true,
	"master_display": true,
	"max-cll":        true,
	"max_cll":        true,
	"hdr10":          true,
	"hdr10_opt":      true,
	"hdr10-opt":      true,
	"aq-mode":        true,
	"aq_mode":        true,
}

// ManagedPresetFlags maps FFmpeg flags that duplicate structured preset fields to their human description.
var ManagedPresetFlags = map[string]string{
	"-crf":            "video quality target / CRF",
	"-preset":         "video preset speed",
	"-c:v":            "video target codec",
	"-vcodec":         "video target codec",
	"-c:a":            "audio codec",
	"-acodec":         "audio codec",
	"-b:v":            "video bitrate",
	"-b:a":            "audio bitrate",
	"-ab":             "audio bitrate",
	"-pix_fmt":        "color depth / pixel format",
	"-profile:v":      "video profile",
	"-cq:v":           "video quality target",
	"-q:v":            "video quality target",
	"-global_quality": "video quality target",
}

// IsManagedPresetFlag reports whether a given flag duplicates a structured preset setting.
func IsManagedPresetFlag(flag string) bool {
	baseFlag := flag
	if idx := strings.Index(flag, "="); idx != -1 {
		baseFlag = flag[:idx]
	}
	_, ok := ManagedPresetFlags[baseFlag]
	return ok
}

// CompoundParam represents a key-value parameter within a colon-delimited compound parameter string.
type CompoundParam struct {
	Key    string
	Val    string
	HasVal bool
}

// ParseCompoundParams parses a colon-delimited string (e.g. "key1=val1:key2:key3=val3")
// into an ordered slice of CompoundParam items.
func ParseCompoundParams(s string) []CompoundParam {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil
	}
	var res []CompoundParam
	for _, part := range strings.Split(trimmed, ":") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if idx := strings.Index(part, "="); idx != -1 {
			res = append(res, CompoundParam{
				Key:    strings.TrimSpace(part[:idx]),
				Val:    strings.TrimSpace(part[idx+1:]),
				HasVal: true,
			})
		} else {
			res = append(res, CompoundParam{
				Key:    part,
				Val:    "",
				HasVal: false,
			})
		}
	}
	return res
}

// NormalizeCompoundKey normalizes compound parameter keys for comparison
// (e.g. treating underscores and hyphens interchangeably).
func NormalizeCompoundKey(k string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(k), "_", "-"))
}

// MergeCompoundParams merges two colon-delimited compound parameter strings.
// Keys in customStr overwrite keys in baseStr, UNLESS the key is in protectedKeys
// and already exists in baseStr. Keys not in baseStr are appended.
func MergeCompoundParams(baseStr, customStr string, protectedKeys map[string]bool) string {
	baseTrimmed := strings.TrimSpace(baseStr)
	customTrimmed := strings.TrimSpace(customStr)

	if baseTrimmed == "" {
		return customTrimmed
	}
	if customTrimmed == "" {
		return baseTrimmed
	}

	baseList := ParseCompoundParams(baseTrimmed)
	customList := ParseCompoundParams(customTrimmed)

	keyIndex := make(map[string]int)
	for i, p := range baseList {
		normKey := NormalizeCompoundKey(p.Key)
		keyIndex[normKey] = i
	}

	for _, cp := range customList {
		normKey := NormalizeCompoundKey(cp.Key)
		if idx, exists := keyIndex[normKey]; exists {
			// Probed HDR10 metadata attributes are strictly preserved against overwrite
			if protectedKeys != nil && (protectedKeys[cp.Key] || protectedKeys[normKey]) {
				continue
			}
			baseList[idx] = cp
		} else {
			keyIndex[normKey] = len(baseList)
			baseList = append(baseList, cp)
		}
	}

	var parts []string
	for _, p := range baseList {
		if p.HasVal {
			parts = append(parts, p.Key+"="+p.Val)
		} else {
			parts = append(parts, p.Key)
		}
	}
	return strings.Join(parts, ":")
}
