package ffmpeg

import (
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"

	"github.com/will25u/slimmedia-agent/internal/jobspec"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// CommandBuilder constructs the ffmpeg argument vector across deterministic
// phases, strictly isolating immutable Tier 1 engine plumbing and cleanly
// separating preset audio and subtitle encoding.
type CommandBuilder struct {
	inputFile  string
	outputFile string
	isMp4      bool
	isMkv      bool

	// Tier 1: Immutable base plumbing
	preInputArgs  []string
	postInputArgs []string
	telemetryArgs []string

	// Tier 2: Stream & hygiene selection
	streamMaps              []string
	metadataArgs            []string
	subtitleHandling        StreamHandling
	stripMetadata           bool
	preferredAudioLanguages []string
	audioLanguageMode       string
	subtitleFormatPolicy    string
	forcedSubtitlePolicy    string
	preferredSubLanguages   []string
	subtitleLanguageMode    string
	streams                 []StreamInfo

	// Tier 3: Audio preset options
	audioCodec    string
	audioBitrate  string
	audioHandling StreamHandling

	// Tier 3: Video preset options
	videoCodec       string
	videoQualityArgs []string
	videoPixFmtArgs  []string
	videoPreset      string
	vendorTuningArgs []string
	colorArgs        []string
	x265Params       string
	svtav1Params     string
	colorInfo        *ColorInfo
	colorDepth       string
	qualityVal       int
	// steering is the topology-driven thread steering for this encode
	// (issue #201). The zero value leaves the argv untouched.
	steering ThreadSteering

	// chunkMode marks a video-only chunk encode (audio and subtitle handling
	// both strip_all). Chunk encodes carry the frame-timing invariant:
	// -fps_mode passthrough is emitted so VFR sources are not decimated to
	// the container's nominal average rate.
	chunkMode bool
	// skipFPSPassthrough is set when user custom flags carry an fps-affecting
	// rate option (-r, -vsync): the user's flag wins and the phase's
	// passthrough is skipped rather than silently nullifying the override.
	skipFPSPassthrough bool

	// Tier 4: Custom raw overrides
	customRawArgs []string
}

// NewCommandBuilder initializes a new CommandBuilder with locked Tier 1
// engine plumbing and safe defaults.
func NewCommandBuilder(inputFile, outputFile string) *CommandBuilder {
	c := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(outputFile)), ".")
	isMkv := strings.HasSuffix(c, ".mkv")
	isMp4 := strings.HasSuffix(c, ".mp4") || strings.HasSuffix(c, ".m4v") || strings.HasSuffix(c, ".mov")

	return &CommandBuilder{
		inputFile:  inputFile,
		outputFile: outputFile,
		isMp4:      isMp4,
		isMkv:      isMkv,

		// Tier 1 immutable base plumbing
		preInputArgs: []string{
			"-hide_banner",
			"-nostdin",
			"-y",
			"-fflags", "+genpts",
			"-probesize", "10000000",
			"-analyzeduration", "10000000",
			"-protocol_whitelist", "file,pipe",
		},
		postInputArgs: []string{
			"-ignore_unknown",
			"-max_muxing_queue_size", "4096",
		},
		telemetryArgs: []string{
			"-progress", "pipe:1",
			"-nostats",
		},

		audioCodec:       "copy",
		audioHandling:    StreamHandlingCopyAll,
		subtitleHandling: StreamHandlingCopyAll,
	}
}

var langEquivs = map[string]string{
	"en": "eng", "eng": "eng",
	"ja": "jpn", "jpn": "jpn",
	"es": "spa", "spa": "spa",
	"fr": "fra", "fra": "fra", "fre": "fra",
	"de": "deu", "deu": "deu", "ger": "deu",
	"it": "ita", "ita": "ita",
	"pt": "por", "por": "por",
	"ru": "rus", "rus": "rus",
	"zh": "zho", "zho": "zho", "chi": "zho",
	"ko": "kor", "kor": "kor",
}

func matchLanguage(streamLang, prefLang string) bool {
	s := strings.ToLower(strings.TrimSpace(streamLang))
	p := strings.ToLower(strings.TrimSpace(prefLang))
	if s == "" || p == "" {
		return false
	}
	if s == p {
		return true
	}
	eqS, hasS := langEquivs[s]
	eqP, hasP := langEquivs[p]
	if hasS && hasP && eqS == eqP {
		return true
	}
	if len(p) == 2 && strings.HasPrefix(s, p) {
		return true
	}
	return false
}

func matchesAnyLanguage(streamLang string, preferredLangs []string) bool {
	for _, pref := range preferredLangs {
		if matchLanguage(streamLang, pref) {
			return true
		}
	}
	return false
}

// SetMetadataStripping configures global metadata sanitization (-map_metadata -1).
func (b *CommandBuilder) SetMetadataStripping(strip bool) *CommandBuilder {
	b.stripMetadata = strip
	return b
}

// SetAudioHygiene configures user-preferred audio languages and selection mode.
func (b *CommandBuilder) SetAudioHygiene(preferredLangs []string, mode string) *CommandBuilder {
	b.preferredAudioLanguages = preferredLangs
	b.audioLanguageMode = mode
	if b.streams != nil {
		b.compileStreamMaps()
	}
	return b
}

// SetSubtitleHygiene configures subtitle format policy, forced subtitle policy,
// and subtitle language whitelist.
func (b *CommandBuilder) SetSubtitleHygiene(formatPolicy, forcedPolicy string, preferredLangs []string, mode string) *CommandBuilder {
	b.subtitleFormatPolicy = formatPolicy
	b.forcedSubtitlePolicy = forcedPolicy
	b.preferredSubLanguages = preferredLangs
	b.subtitleLanguageMode = mode
	if b.streams != nil {
		b.compileStreamMaps()
	}
	return b
}

// videoOnly reports whether the job strips every non-video stream (audio and
// subtitle handling both strip_all). Chunk encodes run in this mode so
// intermediate segments carry pure video: a single 0:v:0 map, -an -sn -dn,
// and no audio/subtitle codec flags, keeping audio sample overhang and
// boundary priming out of the segment seams entirely.
func (b *CommandBuilder) videoOnly() bool {
	return b.audioHandling == StreamHandlingStripAll && b.subtitleHandling == StreamHandlingStripAll
}

func (b *CommandBuilder) compileStreamMaps() {
	if b.videoOnly() {
		b.streamMaps = []string{"-map", "0:v:0"}
		return
	}
	if b.streams == nil {
		b.streamMaps = []string{"-map", "0:v?", "-map", "0:a?"}
		if b.subtitleHandling != StreamHandlingStripAll && b.subtitleFormatPolicy != "strip_all" {
			if !b.isMp4 {
				b.streamMaps = append(b.streamMaps, "-map", "0:s?")
			}
		}
		return
	}

	type streamEntry struct {
		idx  int
		info StreamInfo
	}
	var audioEntries []streamEntry
	var subEntries []streamEntry
	aIdx := 0
	sIdx := 0
	hasCommentary := false
	for _, s := range b.streams {
		if s.Type == "audio" {
			isComm := s.IsCommentary || isCommentaryTrack(s.Title, s.Description, false)
			if isComm {
				hasCommentary = true
			}
			audioEntries = append(audioEntries, streamEntry{idx: aIdx, info: s})
			aIdx++
		} else if s.Type == "subtitle" {
			subEntries = append(subEntries, streamEntry{idx: sIdx, info: s})
			sIdx++
		}
	}

	hasAudioLangFilter := len(b.preferredAudioLanguages) > 0 && b.audioLanguageMode != "all"
	hasSubFilter := b.subtitleFormatPolicy == "text_only" || (b.subtitleLanguageMode == "whitelist" && len(b.preferredSubLanguages) > 0) || b.subtitleHandling == StreamHandlingCopyForcedOnly

	// When no Tier 2 hygiene selection is active, retain universal baseline maps
	if !hasCommentary && !hasAudioLangFilter && !hasSubFilter && b.audioHandling == StreamHandlingCopyAll && (b.subtitleHandling == StreamHandlingCopyAll || b.subtitleHandling == StreamHandlingStripAll) {
		b.streamMaps = []string{"-map", "0:v?", "-map", "0:a?"}
		if b.subtitleHandling != StreamHandlingStripAll && b.subtitleFormatPolicy != "strip_all" {
			if b.isMp4 {
				for _, entry := range subEntries {
					if !isTextSubtitleCodec(entry.info.Codec) {
						log.Printf("dropping bitmap subtitle stream 0:s:%d (%s): MP4 cannot mux it", entry.idx, entry.info.Codec)
					} else {
						b.streamMaps = append(b.streamMaps, "-map", streamRef("0:s", entry.idx))
					}
				}
			} else {
				b.streamMaps = append(b.streamMaps, "-map", "0:s?")
			}
		}
		return
	}

	maps := []string{"-map", "0:v:0"}

	// Audio stream selection & commentary pruning
	if b.audioHandling != StreamHandlingStripAll {
		if len(audioEntries) > 0 {
			var nonCommentary []streamEntry
			for _, entry := range audioEntries {
				if entry.info.IsCommentary || isCommentaryTrack(entry.info.Title, entry.info.Description, false) {
					log.Printf("pruning commentary audio track 0:a:%d (codec: %s, title: %q, lang: %q)", entry.idx, entry.info.Codec, entry.info.Title, entry.info.Language)
					continue
				}
				nonCommentary = append(nonCommentary, entry)
			}

			if b.audioHandling == StreamHandlingCopyPrimary {
				if len(nonCommentary) > 0 {
					maps = append(maps, "-map", streamRef("0:a", nonCommentary[0].idx))
				} else {
					maps = append(maps, "-map", streamRef("0:a", audioEntries[0].idx))
				}
			} else if hasAudioLangFilter {
				var matched []streamEntry
				for _, entry := range nonCommentary {
					if matchesAnyLanguage(entry.info.Language, b.preferredAudioLanguages) {
						matched = append(matched, entry)
					} else {
						log.Printf("stripping foreign dub audio track 0:a:%d (lang: %s, codec: %s)", entry.idx, entry.info.Language, entry.info.Codec)
					}
				}

				if len(matched) > 0 {
					for _, entry := range matched {
						maps = append(maps, "-map", streamRef("0:a", entry.idx))
					}
				} else {
					// Fallback to primary/default audio track so output is never silent
					fallback := audioEntries[0]
					if len(nonCommentary) > 0 {
						fallback = nonCommentary[0]
						for _, entry := range nonCommentary {
							if entry.info.IsDefault {
								fallback = entry
								break
							}
						}
					} else {
						for _, entry := range audioEntries {
							if entry.info.IsDefault {
								fallback = entry
								break
							}
						}
					}
					log.Printf("no audio tracks matched preferred languages; using fallback audio track 0:a:%d (lang: %s)", fallback.idx, fallback.info.Language)
					maps = append(maps, "-map", streamRef("0:a", fallback.idx))
				}
			} else {
				// No language filtering: map all non-commentary audio tracks
				if len(nonCommentary) > 0 {
					for _, entry := range nonCommentary {
						maps = append(maps, "-map", streamRef("0:a", entry.idx))
					}
				} else {
					// Fallback to default or primary track if all were commentary
					fallback := audioEntries[0]
					for _, entry := range audioEntries {
						if entry.info.IsDefault {
							fallback = entry
							break
						}
					}
					maps = append(maps, "-map", streamRef("0:a", fallback.idx))
				}
			}
		}
	}

	// Subtitle stream selection & bitmap pruning
	if b.subtitleHandling != StreamHandlingStripAll && b.subtitleFormatPolicy != "strip_all" {
		for _, entry := range subEntries {
			if shouldIncludeSubtitle(entry.info, b.isMp4, b.subtitleHandling, b.subtitleFormatPolicy, b.forcedSubtitlePolicy, b.preferredSubLanguages, b.subtitleLanguageMode) {
				maps = append(maps, "-map", streamRef("0:s", entry.idx))
			} else {
				if b.isMp4 && !isTextSubtitleCodec(entry.info.Codec) {
					log.Printf("dropping bitmap subtitle stream 0:s:%d (%s): MP4 cannot mux it", entry.idx, entry.info.Codec)
				} else if b.subtitleFormatPolicy == "text_only" && !isTextSubtitleCodec(entry.info.Codec) && !entry.info.IsForced {
					log.Printf("pruning bitmap subtitle stream 0:s:%d (%s) under text_only policy", entry.idx, entry.info.Codec)
				}
			}
		}
	}

	b.streamMaps = maps
}

// SetStreams sets up stream mapping based on probed streams and container format.
func (b *CommandBuilder) SetStreams(streams []StreamInfo) *CommandBuilder {
	b.streams = streams
	b.compileStreamMaps()
	return b
}

// SetSubtitleHandling configures the subtitle handling strategy. When set to
// strip_all, subtitle mapping is skipped and -sn is emitted at build time.
func (b *CommandBuilder) SetSubtitleHandling(sh StreamHandling) *CommandBuilder {
	b.subtitleHandling = sh
	b.compileStreamMaps()
	return b
}

// SetAudio configures Tier 3 audio preset options, defaulting to copy when
// unconfigured.
func (b *CommandBuilder) SetAudio(codec, bitrate string, handling StreamHandling) error {
	if codec != "" && !isValidAudioCodec(codec) {
		return fmt.Errorf("invalid audio codec: %q", codec)
	}

	if codec == "" {
		codec = "copy"
	}
	if handling == "" {
		handling = StreamHandlingCopyAll
	}

	b.audioCodec = codec
	b.audioBitrate = bitrate
	b.audioHandling = handling

	if handling == StreamHandlingTranscodeAAC || codec == "aac" {
		b.audioCodec = "aac"
		if b.audioBitrate == "" {
			b.audioBitrate = "192k"
		}
	}

	b.compileStreamMaps()
	return nil
}

// SetVideoCodec configures the resolved video encoder.
func (b *CommandBuilder) SetVideoCodec(codec string) error {
	if !isValidCodec(codec) {
		return fmt.Errorf("invalid video codec: %q", codec)
	}
	b.videoCodec = codec
	return nil
}

// SetQuality configures quality rate control flags mapped from CRF or quality target.
func (b *CommandBuilder) SetQuality(crf, qualityTarget int) *CommandBuilder {
	v := selectQualityValue(b.videoCodec, crf, qualityTarget)
	b.qualityVal = v
	b.videoQualityArgs = qualityArgsFor(b.videoCodec, v)
	return b
}

// SetColorDepth configures pixel format and profile based on color depth.
func (b *CommandBuilder) SetColorDepth(colorDepth string) *CommandBuilder {
	if colorDepth == "" {
		colorDepth = "10bit"
	}
	b.colorDepth = colorDepth

	switch b.videoCodec {
	case "libx265":
		if colorDepth == "8bit" {
			b.videoPixFmtArgs = []string{"-profile:v", "main", "-pix_fmt", "yuv420p"}
		} else if colorDepth == "10bit" {
			b.videoPixFmtArgs = []string{"-pix_fmt", "yuv420p10le"}
		}
	case "hevc_qsv", "hevc_nvenc", "hevc_amf":
		if colorDepth == "8bit" {
			b.videoPixFmtArgs = []string{"-profile:v", "main", "-pix_fmt", "nv12"}
		} else if colorDepth == "10bit" {
			b.videoPixFmtArgs = []string{"-profile:v", "main10", "-pix_fmt", "p010le"}
		}
	default:
		if pixFmt := pixFmtFor(b.videoCodec, colorDepth); pixFmt != "" {
			b.videoPixFmtArgs = []string{"-pix_fmt", pixFmt}
		} else if colorDepth == "10bit" {
			log.Printf("10-bit output requested but encoder %s cannot deliver it; degrading to 8-bit", b.videoCodec)
		}
	}
	return b
}

// SetPreset configures the encoder preset speed.
func (b *CommandBuilder) SetPreset(preset string) error {
	if preset == "" {
		preset = defaultPresetFor(b.videoCodec)
	}
	if preset != "" && !isValidPreset(preset) {
		return fmt.Errorf("invalid preset speed: %q", preset)
	}
	b.videoPreset = preset
	return nil
}

// SetVendorTuningArgs sets hardware-specific tuning flags verified by canary.
func (b *CommandBuilder) SetVendorTuningArgs(args []string) *CommandBuilder {
	b.vendorTuningArgs = append([]string{}, args...)
	return b
}

// SetColorInfo sets HDR color metadata flags.
func (b *CommandBuilder) SetColorInfo(ci *ColorInfo) *CommandBuilder {
	b.colorInfo = ci
	if ci == nil {
		b.colorArgs = nil
		return b
	}
	var args []string
	if ci.Primaries != "" {
		args = append(args, "-color_primaries", ci.Primaries)
	}
	if ci.Transfer != "" {
		args = append(args, "-color_trc", ci.Transfer)
	}
	if ci.Colorspace != "" {
		args = append(args, "-colorspace", ci.Colorspace)
	}
	b.colorArgs = args
	return b
}

// SetX265Params sets the compound x265 parameters string.
func (b *CommandBuilder) SetX265Params(params string) *CommandBuilder {
	b.x265Params = params
	return b
}

// SetSvtAV1Params sets the compound svtav1 parameters string.
func (b *CommandBuilder) SetSvtAV1Params(params string) *CommandBuilder {
	b.svtav1Params = params
	return b
}

var hdr10ProtectedKeys = protocol.HDR10ProtectedKeys

func mergeCompoundParams(presetStr, customStr string, protectedKeys map[string]bool) string {
	return protocol.MergeCompoundParams(presetStr, customStr, protectedKeys)
}

type rawFlagItem struct {
	flag      string
	val       string
	hasVal    bool
	rawTokens []string
}

func parseRawFlagItems(tokens []string) []rawFlagItem {
	items := protocol.ParseRawFlagItems(tokens)
	res := make([]rawFlagItem, len(items))
	for i, it := range items {
		res[i] = rawFlagItem{
			flag:      it.Flag,
			val:       it.Val,
			hasVal:    it.HasVal,
			rawTokens: it.RawTokens,
		}
	}
	return res
}

func encoderVendor(encoder string) string {
	switch {
	case strings.HasSuffix(encoder, "_nvenc"):
		return "nvenc"
	case strings.HasSuffix(encoder, "_qsv"):
		return "qsv"
	case strings.HasSuffix(encoder, "_amf"):
		return "amf"
	case strings.HasSuffix(encoder, "_videotoolbox"):
		return "videotoolbox"
	default:
		return "software"
	}
}

func isHardwareHEVC(encoder string) bool {
	switch encoder {
	case "hevc_nvenc", "hevc_qsv", "hevc_amf", "hevc_videotoolbox":
		return true
	default:
		return false
	}
}

func isP010LE(pixFmtArgs []string) bool {
	for i := 0; i < len(pixFmtArgs)-1; i++ {
		if pixFmtArgs[i] == "-pix_fmt" && pixFmtArgs[i+1] == "p010le" {
			return true
		}
	}
	return false
}

func (b *CommandBuilder) evictVendorTuning(newCodec string) {
	b.vendorTuningArgs = nil
	newVendor := encoderVendor(newCodec)
	if newVendor == "software" {
		if b.qualityVal > 0 {
			b.videoQualityArgs = qualityArgsFor(newCodec, b.qualityVal)
		} else {
			b.videoQualityArgs = nil
		}
		if b.videoPreset != "" && strings.HasPrefix(b.videoPreset, "p") {
			b.videoPreset = defaultPresetFor(newCodec)
		}
	}
	b.SetColorDepth(b.colorDepth)
}

func (b *CommandBuilder) setOrReplaceVideoBitrate(val string) {
	for i := 0; i < len(b.videoQualityArgs)-1; i++ {
		if b.videoQualityArgs[i] == "-b:v" {
			b.videoQualityArgs[i+1] = val
			return
		}
	}
	b.videoQualityArgs = append(b.videoQualityArgs, "-b:v", val)
}

func (b *CommandBuilder) setOrReplacePixFmt(val string) {
	for i := 0; i < len(b.videoPixFmtArgs)-1; i++ {
		if b.videoPixFmtArgs[i] == "-pix_fmt" {
			b.videoPixFmtArgs[i+1] = val
			return
		}
	}
	b.videoPixFmtArgs = append(b.videoPixFmtArgs, "-pix_fmt", val)
}

func (b *CommandBuilder) setOrReplaceProfile(val string) {
	for i := 0; i < len(b.videoPixFmtArgs)-1; i++ {
		if b.videoPixFmtArgs[i] == "-profile:v" {
			b.videoPixFmtArgs[i+1] = val
			return
		}
	}
	b.videoPixFmtArgs = append(b.videoPixFmtArgs, "-profile:v", val)
}

func (b *CommandBuilder) applyCustomOverrides(tokens []string) error {
	items := parseRawFlagItems(tokens)

	// First pass: handle codec override if present so vendor tuning eviction occurs first
	for _, item := range items {
		if item.flag == "-c:v" || item.flag == "-vcodec" {
			newCodec := item.val
			if newCodec == "copy" {
				return fmt.Errorf("video codec %q is not permitted in custom transcode flags", newCodec)
			}
			if !isValidCodec(newCodec) {
				return fmt.Errorf("invalid video codec: %q", newCodec)
			}
			if newCodec != b.videoCodec {
				prevVendor := encoderVendor(b.videoCodec)
				newVendor := encoderVendor(newCodec)
				b.videoCodec = newCodec
				if prevVendor != newVendor || newVendor == "software" {
					b.evictVendorTuning(newCodec)
				}
				if newCodec == "libx265" && b.x265Params == "" && b.colorInfo != nil {
					if hdr := colorInfoToX265Suffix(b.colorInfo); hdr != "" {
						b.x265Params = hdr
					}
				}
			}
			break
		}
	}

	// Second pass: handle scalar parameters, compound dictionary parameters, and unconsumed flags
	var unconsumed []string
	for _, item := range items {
		switch item.flag {
		case "-c:v", "-vcodec":
			// Handled in first pass
		case "-preset":
			b.videoPreset = item.val
		case "-crf":
			b.videoQualityArgs = []string{"-crf", item.val}
		case "-b:v":
			b.setOrReplaceVideoBitrate(item.val)
		case "-cq:v":
			replaced := false
			for i := 0; i < len(b.videoQualityArgs)-1; i++ {
				if b.videoQualityArgs[i] == "-cq:v" {
					b.videoQualityArgs[i+1] = item.val
					replaced = true
					break
				}
			}
			if !replaced {
				b.videoQualityArgs = append(b.videoQualityArgs, "-cq:v", item.val)
			}
		case "-global_quality":
			b.videoQualityArgs = []string{"-global_quality", item.val}
		case "-q:v":
			b.videoQualityArgs = []string{"-q:v", item.val}
		case "-pix_fmt":
			b.setOrReplacePixFmt(item.val)
		case "-profile:v":
			b.setOrReplaceProfile(item.val)
		case "-x265-params":
			b.x265Params = mergeCompoundParams(b.x265Params, item.val, hdr10ProtectedKeys)
		case "-svtav1-params":
			b.svtav1Params = mergeCompoundParams(b.svtav1Params, item.val, hdr10ProtectedKeys)
		case "-c:a", "-acodec":
			b.audioCodec = item.val
		case "-b:a", "-ab":
			b.audioBitrate = item.val
		case "-an":
			b.audioHandling = StreamHandlingStripAll
		default:
			unconsumed = append(unconsumed, item.rawTokens...)
		}
	}

	b.customRawArgs = unconsumed
	return nil
}

// specIsChunkEncode reports whether a job spec shapes a video-only chunk
// encode: audio and subtitle handling both strip_all. This is the single
// shared predicate for chunk-ness; the runner's per-chunk job shaping and the
// builder's chunk-mode derivation must agree, so neither may reimplement it.
func specIsChunkEncode(spec jobspec.JobSpec) bool {
	return StreamHandling(spec.AudioHandling) == StreamHandlingStripAll &&
		StreamHandling(spec.SubtitleHandling) == StreamHandlingStripAll
}

// isFPSAffectingRateFlag reports whether a custom flag directly sets output
// frame timing: -r (output frame rate) or -vsync (deprecated alias of
// -fps_mode, the frame synchronization mode). Stream-specified variants
// (-r:v, -vsync:v) normalize the same way the invariant registry normalizes
// them. -fps_mode itself is not listed: it is a phase-locked chunk invariant
// and is rejected outright via the registry collision check in chunk scope.
func isFPSAffectingRateFlag(flag string) bool {
	norm := flag
	if idx := strings.Index(norm, "="); idx != -1 {
		norm = norm[:idx]
	}
	if idx := strings.Index(norm, ":"); idx != -1 {
		norm = norm[:idx]
	}
	return norm == "-r" || norm == "-vsync"
}

// SetCustomFlags validates and registers custom flags. Any flag attempting to
// override immutable Tier 1 engine plumbing is strictly rejected. In chunk
// mode the collision check runs in ScopeEncodeChunk, so phase-locked chunk
// invariants (-fps_mode, -an, -sn, -dn) are rejected with a clear error
// instead of being silently overridden at the chunk boundary.
func (b *CommandBuilder) SetCustomFlags(ffmpegPath, flags string) error {
	trimmed := strings.TrimSpace(flags)
	if trimmed == "" {
		return nil
	}

	tokens, err := SanitizeCustomFlags(trimmed)
	if err != nil {
		return err
	}

	scope := protocol.ScopeEncodeAll
	if b.chunkMode {
		scope = protocol.ScopeEncodeChunk
	}

	// Enforce Tier 1 immutability: custom flags must not override Tier 1 args
	for _, tok := range tokens {
		if col := protocol.CheckInvariantCollision(tok, scope); col != nil {
			return fmt.Errorf("tier 1 argument %q is immutable and cannot be overridden: %s (scope: %s)", col.Flag, col.Rationale, scope)
		}
	}

	if protocol.CustomFlagsAreRaw(trimmed) {
		if b.chunkMode {
			// A user rate option expresses explicit frame-timing intent: the
			// user's flag wins and the chunk passthrough is skipped, so the
			// two never fight via last-flag-wins inside ffmpeg.
			for _, item := range parseRawFlagItems(tokens) {
				if isFPSAffectingRateFlag(item.flag) {
					b.skipFPSPassthrough = true
					log.Printf("chunk encode: custom flag %q sets frame timing; skipping -fps_mode passthrough (user flag wins)", strings.Join(item.rawTokens, " "))
				}
			}
		}

		targetCodec := b.videoCodec
		for i, tok := range tokens {
			if (tok == "-c:v" || tok == "-vcodec") && i+1 < len(tokens) {
				targetCodec = tokens[i+1]
			} else if strings.HasPrefix(tok, "-c:v=") || strings.HasPrefix(tok, "-vcodec=") {
				targetCodec = strings.SplitN(tok, "=", 2)[1]
			}
		}

		if err := ValidateRawFlags(ffmpegPath, targetCodec, b.isMp4, tokens); err != nil {
			return err
		}

		if err := b.applyCustomOverrides(tokens); err != nil {
			return err
		}
	} else {
		b.x265Params = mergeCompoundParams(b.x265Params, trimmed, hdr10ProtectedKeys)
		if err := ValidateX265Params(ffmpegPath, b.x265Params); err != nil {
			return err
		}
	}

	return nil
}

// applySteering injects topology-driven thread steering into the video
// argument block (issue #201). Idempotent and user-respecting: a key the user
// already set via custom flags wins and is never duplicated. The returned
// args (currently only libx264 -threads) belong right after -preset.
func (b *CommandBuilder) applySteering() []string {
	if !b.steering.Enabled {
		return nil
	}
	switch b.videoCodec {
	case "libx265":
		if b.steering.Pools != "" && !compoundParamHas(b.x265Params, "pools") {
			b.x265Params = appendCompoundParam(b.x265Params, "pools="+b.steering.Pools)
		}
	case "libsvtav1":
		if b.steering.Threads > 0 && !compoundParamHas(b.svtav1Params, "lp") {
			b.svtav1Params = appendCompoundParam(b.svtav1Params, "lp="+strconv.Itoa(b.steering.Threads))
		}
	case "libx264":
		if b.steering.Threads > 0 && !slices.Contains(b.customRawArgs, "-threads") {
			return []string{"-threads", strconv.Itoa(b.steering.Threads)}
		}
	}
	return nil
}

// compoundParamHas reports whether a colon-delimited compound param string
// (e.g. -x265-params) already sets the given key.
func compoundParamHas(params, key string) bool {
	norm := protocol.NormalizeCompoundKey(key)
	for _, p := range protocol.ParseCompoundParams(params) {
		if protocol.NormalizeCompoundKey(p.Key) == norm {
			return true
		}
	}
	return false
}

// appendCompoundParam appends a key=value to a compound param string.
func appendCompoundParam(params, kv string) string {
	if strings.TrimSpace(params) == "" {
		return kv
	}
	return params + ":" + kv
}

// Build compiles the deterministic, properly ordered argument array across all
// phases while preserving Tier 1 immutability.
func (b *CommandBuilder) Build() ([]string, error) {
	if b.videoCodec == "" {
		return nil, fmt.Errorf("video codec is required")
	}
	vo := b.videoOnly()

	var args []string

	// 1. Tier 1 Pre-Input
	args = append(args, b.preInputArgs...)

	// 2. Input File
	args = append(args, "-i", b.inputFile)

	// 3. Tier 2 Stream Maps
	if len(b.streamMaps) > 0 {
		args = append(args, b.streamMaps...)
	} else {
		args = append(args, "-map", "0:v?", "-map", "0:a?")
		if b.subtitleHandling != StreamHandlingStripAll {
			args = append(args, "-map", "0:s?")
		}
	}

	// 4. Tier 1 Post-Input & Metadata
	args = append(args, "-ignore_unknown")
	if b.stripMetadata {
		args = append(args, "-map_metadata", "-1", "-map_chapters", "0")
	} else {
		args = append(args, "-map_metadata", "0", "-map_chapters", "0")
	}
	args = append(args, "-max_muxing_queue_size", "4096")

	// Full-stream encodes preserve per-stream timestamps: without -copyts the
	// muxer rebases output start times relative to the first packet, and
	// hardware encoders (hevc_qsv with adaptive B-frames) can surface that
	// rebase as a one-frame audio/video offset shift, failing the ±40ms
	// alignment gate (#200). The chunked path must NOT carry this: segment
	// timing is owned by the split/concat phases (#190-#192).
	if !b.chunkMode {
		args = append(args, "-copyts")
	}

	// 5. Tier 3 Audio Encoding
	if vo {
		args = append(args, "-an", "-sn", "-dn")
	} else if b.audioHandling == StreamHandlingStripAll {
		args = append(args, "-an")
	} else {
		codec := b.audioCodec
		if codec == "" {
			codec = "copy"
		}
		args = append(args, "-c:a", codec)
		if codec == "aac" {
			bitrate := b.audioBitrate
			if bitrate == "" {
				bitrate = "192k"
			}
			args = append(args, "-b:a", bitrate)
		} else if b.audioBitrate != "" {
			args = append(args, "-b:a", b.audioBitrate)
		}
	}

	// 6. Subtitle Handling & Container Flags
	if !vo {
		if b.subtitleHandling == StreamHandlingStripAll {
			args = append(args, "-sn")
		} else if b.isMkv {
			args = append(args, "-c:s", "copy")
		}
	}

	if b.isMp4 {
		args = append(args, "-movflags", "+faststart")
	}

	// 7. Tier 3 Video Pipeline
	args = append(args, "-c:v", b.videoCodec)
	if b.chunkMode && !b.skipFPSPassthrough {
		// Chunk encodes preserve source frame timing: without this the
		// encoder resamples VFR sources to the chunk container's nominal
		// average rate, decimating frames (see SystemInvariantRegistry,
		// ScopeEncodeChunk). User rate flags (-r/-vsync) skip this via
		// skipFPSPassthrough so the user's flag wins deterministically.
		args = append(args, "-fps_mode", "passthrough")
	}
	if len(b.videoQualityArgs) > 0 {
		args = append(args, b.videoQualityArgs...)
	}
	if len(b.videoPixFmtArgs) > 0 {
		args = append(args, b.videoPixFmtArgs...)
	}
	if isHardwareHEVC(b.videoCodec) && !slices.Contains(b.customRawArgs, "-g") {
		args = append(args, "-g", "240")
	}
	if len(b.vendorTuningArgs) > 0 {
		args = append(args, b.vendorTuningArgs...)
	}
	colorArgs := b.colorArgs
	if len(colorArgs) == 0 && b.colorInfo == nil && isP010LE(b.videoPixFmtArgs) && !slices.Contains(b.customRawArgs, "-color_primaries") {
		colorArgs = []string{"-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709"}
	}
	if len(colorArgs) > 0 {
		args = append(args, colorArgs...)
	}
	if b.videoPreset != "" {
		args = append(args, "-preset", b.videoPreset)
	}
	if steerArgs := b.applySteering(); len(steerArgs) > 0 {
		args = append(args, steerArgs...)
	}
	if b.videoCodec == "libx265" && b.x265Params != "" {
		args = append(args, "-x265-params", b.x265Params)
	}
	if b.videoCodec == "libsvtav1" && b.svtav1Params != "" {
		args = append(args, "-svtav1-params", b.svtav1Params)
	}

	// 8. Tier 1 Telemetry
	args = append(args, b.telemetryArgs...)

	// 9. Tier 4 Custom Raw Overrides
	if len(b.customRawArgs) > 0 {
		args = append(args, b.customRawArgs...)
	}

	// 10. Output Target
	args = append(args, b.outputFile)

	return args, nil
}

// NewCommandBuilderFromJob builds and configures a CommandBuilder from a TranscodeJob.
func NewCommandBuilderFromJob(job TranscodeJob, streams []StreamInfo) (*CommandBuilder, error) {
	spec := job.Spec
	var caps EncoderCaps
	if job.Caps != nil {
		caps = *job.Caps
	}

	encoder := job.Codec
	if spec.EncoderEngine != "" {
		encoder = ResolveEncoder(spec.EncoderEngine, job.Codec, caps)
	}

	audioCodec := spec.AudioCodec
	if audioCodec == "" {
		audioCodec = "copy"
	}
	if !isValidCodec(encoder) || !isValidPreset(spec.PresetSpeed) || !isValidAudioCodec(audioCodec) {
		return nil, fmt.Errorf("invalid codec, preset, or audio codec")
	}

	builder := NewCommandBuilder(job.InputFile, job.OutputFile)

	c := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(spec.Container)), ".")
	builder.isMkv = c == "mkv" || strings.HasSuffix(strings.ToLower(job.OutputFile), ".mkv")
	builder.isMp4 = isMP4Container(spec.Container, job.OutputFile)

	builder.SetMetadataStripping(spec.StripMetadata)
	builder.SetAudioHygiene(spec.PreferredAudioLanguages, spec.AudioLanguageMode)
	builder.SetSubtitleHygiene(spec.SubtitleFormatPolicy, spec.ForcedSubtitlePolicy, spec.PreferredSubtitleLanguages, spec.SubtitleLanguageMode)

	subHandling := StreamHandling(spec.SubtitleHandling)
	if subHandling == "" {
		subHandling = StreamHandlingCopyAll
	}
	builder.SetSubtitleHandling(subHandling)
	builder.SetStreams(streams)

	audioHandling := StreamHandling(spec.AudioHandling)
	if err := builder.SetAudio(spec.AudioCodec, "", audioHandling); err != nil {
		return nil, err
	}

	// A video-only build (audio and subtitle handling both strip_all) is a
	// chunk encode; it carries the chunk-phase frame-timing invariant.
	builder.chunkMode = specIsChunkEncode(spec)

	if err := builder.SetVideoCodec(encoder); err != nil {
		return nil, err
	}

	builder.SetQuality(spec.CRF, spec.QualityTarget)
	builder.SetColorDepth(spec.ColorDepth)

	if err := builder.SetPreset(spec.PresetSpeed); err != nil {
		return nil, err
	}

	// Vendor hardware tunings. A user-selected tuning (issue #195) replaces the
	// default package field-for-field; both are gated on the same per-driver
	// canary. A selection that cannot map to the resolved encoder (e.g. an
	// NVENC/QSV preset falling back to AMF) keeps the default package, and a
	// cross-family selection (NVENC preset resolved to QSV) applies the
	// resolved encoder's defaults with a log. Archivist-grade jobs below still
	// replace everything with the code-owned package.
	if tuning, ok := hwTuningFlagSets[encoder]; ok {
		verified := job.Caps == nil || tuningVerified(encoder, caps)
		switch {
		case !verified:
			log.Printf("skipping %s tuning flags: canary encode failed (driver rejects advanced flags)", encoder)
		case spec.VendorTuning != nil:
			if args := vendorTuningArgsFor(encoder, spec.VendorTuning); args != nil {
				if vendorTuningSelectionForeign(encoder, spec.VendorTuning) {
					log.Printf("%s: vendor tuning selection targets another hardware family; applying %s defaults", encoder, encoder)
				}
				builder.SetVendorTuningArgs(args)
			} else {
				log.Printf("%s: vendor tuning selection does not apply to this encoder; keeping default package", encoder)
				builder.SetVendorTuningArgs(tuning)
			}
		default:
			builder.SetVendorTuningArgs(tuning)
		}

		if spec.VendorTuning != nil && job.FFmpegPath != "" && len(builder.vendorTuningArgs) > 0 {
			if err := ValidateVendorTuning(job.FFmpegPath, encoder, builder.videoQualityArgs, builder.vendorTuningArgs); err != nil {
				return nil, fmt.Errorf("vendor hardware tuning parameters invalid for %s: %w", encoder, err)
			}
		}
	}

	// Archivist-grade jobs replace quality, speed, and tuning with the
	// code-owned vendor package for whichever encoder resolved — including
	// fallback steps — so the job never silently degrades to plain
	// preset values when hardware steps down.
	archivistGrade := spec.QualityTier == protocol.QualityTierArchivist
	if archivistGrade {
		if !applyArchivistGrade(builder, encoder, caps) {
			log.Printf("archivist grade: no code-owned package for resolved encoder %s; keeping preset values", encoder)
		}
	}

	builder.SetColorInfo(job.ColorInfo)
	if job.Steering != nil {
		builder.steering = *job.Steering
	}

	// x265 params for libx265
	if encoder == "libx265" {
		customLegacy := spec.CustomFlags
		if protocol.CustomFlagsAreRaw(spec.CustomFlags) {
			customLegacy = ""
		}
		// Archivist grade owns the software tuning package: aq-mode 3,
		// SAO off, tune film — regardless of the preset's tuning fields.
		aqMode, filmGrainSao, contentTuning := spec.AqMode, spec.FilmGrainSao, spec.ContentTuning
		if archivistGrade {
			aqMode, filmGrainSao, contentTuning = 3, false, "film"
		}
		params := buildX265Params(aqMode, filmGrainSao, contentTuning, customLegacy)
		if spec.VendorTuning != nil {
			if spec.VendorTuning.AQStrength != nil && *spec.VendorTuning.AQStrength != "" && !compoundParamHas(params, "aq-strength") {
				params = appendCompoundParam(params, "aq-strength="+*spec.VendorTuning.AQStrength)
			}
			if spec.VendorTuning.BFrames != nil && !compoundParamHas(params, "bframes") {
				params = appendCompoundParam(params, "bframes="+strconv.Itoa(*spec.VendorTuning.BFrames))
			}
			if spec.VendorTuning.Deblock != nil && *spec.VendorTuning.Deblock != "" && !compoundParamHas(params, "deblock") {
				params = appendCompoundParam(params, "deblock="+*spec.VendorTuning.Deblock)
			}
		}
		if hdr := colorInfoToX265Suffix(job.ColorInfo); hdr != "" {
			if params != "" {
				params += ":" + hdr
			} else {
				params = hdr
			}
		}
		builder.SetX265Params(params)
	}

	// svtav1 params for libsvtav1
	if encoder == "libsvtav1" {
		customLegacy := spec.CustomFlags
		if protocol.CustomFlagsAreRaw(spec.CustomFlags) {
			customLegacy = ""
		}
		params := buildSvtAV1Params(spec.ContentTuning, spec.FilmGrainSao, customLegacy, archivistGrade, spec.PresetSpeed)
		if spec.VendorTuning != nil {
			if spec.VendorTuning.SvtTune != nil && !compoundParamHas(params, "tune") {
				params = appendCompoundParam(params, "tune="+strconv.Itoa(*spec.VendorTuning.SvtTune))
			}
			if spec.VendorTuning.FilmGrain != nil && !compoundParamHas(params, "film-grain") {
				params = appendCompoundParam(params, "film-grain="+strconv.Itoa(*spec.VendorTuning.FilmGrain))
			}
			if spec.VendorTuning.HierarchicalLevels != nil && !compoundParamHas(params, "hierarchical-levels") {
				params = appendCompoundParam(params, "hierarchical-levels="+strconv.Itoa(*spec.VendorTuning.HierarchicalLevels))
			}
			if spec.VendorTuning.FastDecode != nil && !compoundParamHas(params, "fast-decode") {
				params = appendCompoundParam(params, "fast-decode="+strconv.Itoa(*spec.VendorTuning.FastDecode))
			}
		}
		builder.SetSvtAV1Params(params)
	}

	// Custom flags
	if spec.CustomFlags != "" {
		if err := builder.SetCustomFlags(job.FFmpegPath, spec.CustomFlags); err != nil {
			return nil, err
		}
	}

	return builder, nil
}
