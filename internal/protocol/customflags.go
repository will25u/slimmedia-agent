package protocol

import (
	"fmt"
	"strings"
)

// CustomFlagsAreRaw reports whether a custom_flags value should be treated as
// raw trailing ffmpeg arguments: any whitespace-separated token starting with
// "-". Values without such tokens keep the legacy meaning (x265 params like
// "keyint=60").
//
// This is the single definition of the custom_flags mode split, shared by the
// API (validate-flags endpoint) and the agent (argv builder). Both sides must
// agree, so neither may reimplement it locally.
func CustomFlagsAreRaw(flags string) bool {
	for _, tok := range strings.Fields(flags) {
		if strings.HasPrefix(tok, "-") {
			return true
		}
	}
	return false
}

// SplitRawFlags splits raw custom flags into tokens, honoring 'single' and
// "double" quotes (quotes group whitespace and are stripped). An
// unterminated quote is an error.
func SplitRawFlags(flags string) ([]string, error) {
	var tokens []string
	var cur strings.Builder
	var quote rune
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for _, r := range flags {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote in custom flags")
	}
	flush()
	return tokens, nil
}

// booleanRawFlags identifies standalone boolean FFmpeg flags that take no argument values.
var booleanRawFlags = map[string]bool{
	"-an":                      true,
	"-vn":                      true,
	"-sn":                      true,
	"-dn":                      true,
	"-nostdin":                 true,
	"-nostats":                 true,
	"-hide_banner":             true,
	"-y":                       true,
	"-n":                       true,
	"-re":                      true,
	"-benchmark":               true,
	"-shortest":                true,
	"-copyts":                  true,
	"-copytb":                  true,
	"-accurate_seek":           true,
	"-auto_conversion_filters": true,
	"-ignore_unknown":          true,
}

// BooleanRawFlags returns a defensive copy of recognized standalone boolean FFmpeg flags.
func BooleanRawFlags() map[string]bool {
	cp := make(map[string]bool, len(booleanRawFlags))
	for k, v := range booleanRawFlags {
		cp[k] = v
	}
	return cp
}

// RawFlagItem describes a parsed argument flag with its associated value or tokens.
type RawFlagItem struct {
	Flag      string
	Val       string
	HasVal    bool
	RawTokens []string
}

// ParseRawFlagItems parses a slice of raw flag tokens into structured RawFlagItem records.
func ParseRawFlagItems(tokens []string) []RawFlagItem {
	var items []RawFlagItem
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if idx := strings.Index(tok, "="); idx != -1 {
			flag := tok[:idx]
			val := tok[idx+1:]
			items = append(items, RawFlagItem{
				Flag:      flag,
				Val:       val,
				HasVal:    true,
				RawTokens: []string{tok},
			})
			continue
		}
		if booleanRawFlags[tok] {
			items = append(items, RawFlagItem{
				Flag:      tok,
				HasVal:    false,
				RawTokens: []string{tok},
			})
			continue
		}
		if i+1 < len(tokens) && (!strings.HasPrefix(tokens[i+1], "-") || (len(tokens[i+1]) > 1 && tokens[i+1][1] >= '0' && tokens[i+1][1] <= '9')) {
			items = append(items, RawFlagItem{
				Flag:      tok,
				Val:       tokens[i+1],
				HasVal:    true,
				RawTokens: []string{tok, tokens[i+1]},
			})
			i++
			continue
		}
		items = append(items, RawFlagItem{
			Flag:      tok,
			HasVal:    false,
			RawTokens: []string{tok},
		})
	}
	return items
}

// forbiddenRawFlags are exact tokens that raw custom flags may not contain
// to prevent parameter smuggling, stream hijacking, or arbitrary file writes.
var forbiddenRawFlags = map[string]bool{
	"-i":                     true,
	"-nostdin":               true,
	"-hide_banner":           true,
	"-y":                     true,
	"-n":                     true,
	"-fflags":                true,
	"-protocol_whitelist":    true,
	"-max_muxing_queue_size": true,
	"-ignore_unknown":        true,
	"-progress":              true,
	"-nostats":               true,
	// Timestamp preservation is platform-owned: the full-stream encode shape
	// emits -copyts itself (#200) and the chunked path applies it at concat
	// on the demux sidecar (#192). A user-supplied copy would duplicate the
	// flag and fight the phase that owns stream timing.
	"-copyts":            true,
	"-f":                 true,
	"-target":            true,
	"-tee":               true,
	"-attach":            true,
	"-dump_attachment":   true,
	"-passlogfile":       true,
	"-vstats_file":       true,
	"-print_graphs_file": true,
	"-filter_complex":    true,
	"-lavfi":             true,
	// Single-input filter family: filter graphs can read arbitrary local
	// files (e.g. -vf subtitles=/path/to/secret) and either burn the content
	// into output frames or echo it back through ffmpeg's error output.
	"-vf":     true,
	"-af":     true,
	"-report": true,
}

var forbiddenRawFlagPrefixes = []string{
	"-map",
	"-dump_attachment",
	"-passlogfile",
	"-vstats_file",
	"-print_graphs_file",
	// Covers -filter, -filter:v, -filter:a, -filter_script,
	// -filter_script:v, -filter_script:a, -filter_complex_script.
	"-filter",
	"-vf",
	"-af",
	"-lavfi",
}


var forbiddenEncoderParamKeys = map[string]bool{
	"csv":                 true,
	"dump-yuv":            true,
	"dump_yuv":            true,
	"stats":               true,
	"passlogfile":         true,
	"qpfile":              true,
	"lambda-file":         true,
	"analysis-save":       true,
	"analysis-load":       true,
	"analysis-reuse-file": true,
	"recon":               true,
	"scaling-list":        true,
	"log-file":            true,
	"logfile":             true,
}

func isEncoderParamFlag(flag string) bool {
	norm := flag
	if idx := strings.Index(norm, "="); idx != -1 {
		norm = norm[:idx]
	}
	if idx := strings.Index(norm, ":"); idx != -1 {
		norm = norm[:idx]
	}
	return strings.HasSuffix(norm, "-params") || strings.HasSuffix(norm, "-opts") || strings.HasSuffix(norm, "opts")
}

func isNumericRatio(s string) bool {
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return false
	}
	if len(parts[0]) == 0 || len(parts[1]) == 0 {
		return false
	}
	for _, r := range parts[0] {
		if r < '0' || r > '9' {
			return false
		}
	}
	for _, r := range parts[1] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func validateRawFlagValue(flag, val string) error {
	trimmedVal := strings.TrimSpace(val)
	if strings.Contains(trimmedVal, "..") {
		return fmt.Errorf("custom flags contain unauthorized path token %q", val)
	}
	if strings.Contains(trimmedVal, "\\") {
		return fmt.Errorf("custom flags contain unauthorized path token %q", val)
	}

	normFlag := flag
	if idx := strings.Index(normFlag, "="); idx != -1 {
		normFlag = normFlag[:idx]
	}
	if normFlag == "-c:v" || normFlag == "-vcodec" || strings.HasPrefix(normFlag, "-c:v:") {
		if trimmedVal == "copy" {
			return fmt.Errorf("video codec %q is not permitted in custom transcode flags", val)
		}
	}

	if isEncoderParamFlag(flag) {
		if strings.Contains(trimmedVal, "/") {
			return fmt.Errorf("custom flags contain unauthorized path token %q", val)
		}
		for _, part := range strings.Split(trimmedVal, ":") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if strings.Contains(part, "/") || strings.Contains(part, "\\") || strings.Contains(part, "..") {
				return fmt.Errorf("custom flags contain unauthorized path token %q", part)
			}
			key := part
			if idx := strings.Index(part, "="); idx != -1 {
				key = part[:idx]
			}
			key = strings.ToLower(strings.TrimSpace(key))
			if forbiddenEncoderParamKeys[key] {
				return fmt.Errorf("custom flags contain forbidden encoder parameter %q", key)
			}
		}
		return nil
	}

	if strings.Contains(trimmedVal, "/") && !isNumericRatio(trimmedVal) {
		return fmt.Errorf("custom flags contain unauthorized path token %q", val)
	}

	return nil
}

// SanitizeCustomFlags inspects custom flags in either raw or legacy mode,
// rejecting bare non-flag tokens, multiple output targets, unauthorized
// filesystem paths, and forbidden ffmpeg options. On success, it returns
// the parsed raw tokens (if in raw mode) or nil tokens (if in legacy mode).
func SanitizeCustomFlags(flags string) ([]string, error) {
	trimmed := strings.TrimSpace(flags)
	if trimmed == "" {
		return nil, nil
	}

	if len(trimmed) > 2000 {
		return nil, fmt.Errorf("custom flags exceed maximum length of 2000 characters")
	}

	if !CustomFlagsAreRaw(trimmed) {
		// Legacy mode (x265 params): reject spaces, slashes, or bare filenames.
		if strings.ContainsAny(trimmed, " \t\r\n") {
			return nil, fmt.Errorf("custom flags in legacy mode must not contain whitespace or bare tokens: %q", trimmed)
		}
		for _, part := range strings.Split(trimmed, ":") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if strings.Contains(part, "/") || strings.Contains(part, "\\") || strings.Contains(part, "..") {
				return nil, fmt.Errorf("custom flags contain unauthorized path token %q", part)
			}
			key := part
			if idx := strings.Index(part, "="); idx != -1 {
				key = part[:idx]
			}
			if strings.Contains(key, ".") {
				return nil, fmt.Errorf("custom flags contain unauthorized token %q", part)
			}
			normKey := strings.ToLower(strings.TrimSpace(key))
			if forbiddenEncoderParamKeys[normKey] {
				return nil, fmt.Errorf("custom flags contain forbidden encoder parameter %q", normKey)
			}
		}
		return nil, nil
	}

	tokens, err := SplitRawFlags(trimmed)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, nil
	}
	if len(tokens) > 100 {
		return nil, fmt.Errorf("custom flags exceed maximum of 100 argument tokens")
	}

	if !strings.HasPrefix(tokens[0], "-") {
		return nil, fmt.Errorf("custom flags start with bare non-flag token %q", tokens[0])
	}

	expectingValueFor := ""
	for _, tok := range tokens {
		if tok == "-" {
			return nil, fmt.Errorf("custom flags contain unauthorized output target %q", tok)
		}

		if expectingValueFor != "" {
			// A value token: if it does NOT start with "-", or is a negative number, it's the value.
			if !strings.HasPrefix(tok, "-") {
				if err := validateRawFlagValue(expectingValueFor, tok); err != nil {
					return nil, err
				}
				expectingValueFor = ""
				continue
			}
			// It starts with "-". Could it be a negative number (e.g. -1)?
			if len(tok) > 1 && (tok[1] >= '0' && tok[1] <= '9') {
				if err := validateRawFlagValue(expectingValueFor, tok); err != nil {
					return nil, err
				}
				expectingValueFor = ""
				continue
			}
			// Otherwise, previous flag was left without an argument!
			return nil, fmt.Errorf("custom flag %q is missing an argument", expectingValueFor)
		}

		// Here we are expecting a flag starting with "-"
		if !strings.HasPrefix(tok, "-") {
			return nil, fmt.Errorf("bare non-flag token %q not allowed in custom flags", tok)
		}

		baseFlag := tok
		if idx := strings.Index(tok, "="); idx != -1 {
			baseFlag = tok[:idx]
		}
		normFlag := baseFlag
		if idx := strings.Index(normFlag, ":"); idx != -1 {
			normFlag = normFlag[:idx]
		}

		if forbiddenRawFlags[baseFlag] || forbiddenRawFlags[normFlag] {
			return nil, fmt.Errorf("custom flags contain forbidden token %q", tok)
		}

		for _, prefix := range forbiddenRawFlagPrefixes {
			if strings.HasPrefix(baseFlag, prefix) || strings.HasPrefix(normFlag, prefix) {
				return nil, fmt.Errorf("custom flags contain forbidden token %q", tok)
			}
		}

		if idx := strings.Index(tok, "="); idx != -1 {
			// Embedded value like -preset=medium or -c:v=libx264
			val := tok[idx+1:]
			if err := validateRawFlagValue(baseFlag, val); err != nil {
				return nil, err
			}
			expectingValueFor = ""
			continue
		}

		if booleanRawFlags[tok] {
			// Boolean flag takes 0 arguments
			expectingValueFor = ""
			continue
		}

		// Takes 1 argument
		expectingValueFor = tok
	}

	if expectingValueFor != "" {
		return nil, fmt.Errorf("custom flag %q is missing an argument", expectingValueFor)
	}

	return tokens, nil
}
