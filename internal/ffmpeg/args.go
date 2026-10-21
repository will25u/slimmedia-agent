package ffmpeg

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/will25u/slimmedia-agent/internal/jobspec"
	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// StreamInfo describes one audio or subtitle stream of the input file, used
// to build -map arguments for the preset audio/subtitle handling strategies.
type StreamInfo struct {
	Type         string // "audio" or "subtitle"
	Codec        string
	Language     string
	Title        string
	Description  string
	IsDefault    bool
	IsForced     bool
	IsCommentary bool

	// Duration is the stream-level duration in seconds (ffprobe stream
	// duration field or DURATION tags). Zero when ffprobe reports no
	// stream-level duration. Used by the upfront demux verification to
	// compare tracks like-for-like between source and scratch container.
	Duration float64
}

func isCommentaryTrack(title, description string, dispositionCommentary bool) bool {
	if dispositionCommentary {
		return true
	}
	for _, text := range []string{title, description} {
		if text == "" {
			continue
		}
		lower := strings.ToLower(text)
		if strings.Contains(lower, "commentary") ||
			strings.Contains(lower, "director's comments") ||
			strings.Contains(lower, "director comments") {
			return true
		}
	}
	return false
}

// ProbeStreams lists the audio and subtitle streams of the input file via
// ffprobe. A failed probe returns nil streams and no error here — the caller
// degrades to safe map defaults rather than failing the job.
func ProbeStreams(ffprobePath, inputPath string) []StreamInfo {
	if err := CheckLocalMediaPath(inputPath); err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := execCommandContext(ctx, ffprobePath,
		"-v", "error",
		"-show_entries", "stream=codec_type,codec_name,duration:stream_tags=language,title,description,DURATION:stream_disposition=default,forced,commentary",
		"-of", "json",
		inputPath,
	).Output()
	if err != nil {
		log.Printf("stream probe failed for %s: %v; degrading to default stream maps", inputPath, err)
		return nil
	}
	var output struct {
		Streams []struct {
			CodecName   string `json:"codec_name"`
			CodecType   string `json:"codec_type"`
			Duration    string `json:"duration"`
			Disposition struct {
				Default    int `json:"default"`
				Forced     int `json:"forced"`
				Commentary int `json:"commentary"`
			} `json:"disposition"`
			Tags map[string]string `json:"tags"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &output); err != nil {
		log.Printf("failed to parse ffprobe stream json for %s: %v; degrading to default stream maps", inputPath, err)
		return nil
	}
	var streams []StreamInfo
	for _, s := range output.Streams {
		if s.CodecType != "audio" && s.CodecType != "subtitle" {
			continue
		}
		title := ""
		description := ""
		lang := ""
		for k, v := range s.Tags {
			switch strings.ToLower(k) {
			case "title":
				title = v
			case "description":
				description = v
			case "language":
				lang = v
			}
		}
		isCommentary := isCommentaryTrack(title, description, s.Disposition.Commentary == 1)
		isForced := s.Disposition.Forced == 1 || strings.Contains(strings.ToLower(title), "forced")

		streams = append(streams, StreamInfo{
			Type:         s.CodecType,
			Codec:        s.CodecName,
			Language:     lang,
			Title:        title,
			Description:  description,
			IsDefault:    s.Disposition.Default == 1,
			IsForced:     isForced,
			IsCommentary: isCommentary,
			Duration:     streamDurationSeconds(s.Duration, s.Tags),
		})
	}
	return streams
}

// streamDurationSeconds resolves a stream-level duration from the ffprobe
// stream duration field, falling back to DURATION-style tags
// (HH:MM:SS.mmm). Returns zero when neither is present or parseable.
func streamDurationSeconds(streamField string, tags map[string]string) float64 {
	if streamField != "" {
		if d, err := strconv.ParseFloat(streamField, 64); err == nil && d > 0 {
			return d
		}
	}
	for _, key := range []string{"DURATION", "DURATION-eng", "duration"} {
		if dt, ok := tags[key]; ok && dt != "" {
			parts := strings.Split(dt, ":")
			if len(parts) == 3 {
				h, _ := strconv.ParseFloat(parts[0], 64)
				m, _ := strconv.ParseFloat(parts[1], 64)
				s, _ := strconv.ParseFloat(parts[2], 64)
				if parsed := h*3600 + m*60 + s; parsed > 0 {
					return parsed
				}
			}
		}
	}
	return 0
}

// textSubtitleCodecs are subtitle codecs that MP4 can mux. Bitmap/image
// subtitles (PGS, VobSub, DVB) are not — mapping them into an MP4 output
// crashes the mux, so MP4 targets must drop them.
var textSubtitleCodecs = map[string]bool{
	"srt": true, "subrip": true, "ass": true, "ssa": true,
	"mov_text": true, "text": true, "webvtt": true, "ttml": true,
}

func isTextSubtitleCodec(codec string) bool {
	return textSubtitleCodecs[strings.ToLower(codec)]
}

// isMP4Container reports whether the target output is an MP4-family
// container, which cannot hold bitmap/image subtitles.
func isMP4Container(container, outputFile string) bool {
	lower := strings.ToLower(outputFile)
	if strings.HasSuffix(lower, ".mkv") {
		return false
	}
	c := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(container)), ".")
	if c == "mp4" || c == "m4v" || c == "mov" {
		return true
	}
	return strings.HasSuffix(lower, ".mp4") || strings.HasSuffix(lower, ".m4v") || strings.HasSuffix(lower, ".mov")
}

// StreamHandling represents audio or subtitle stream handling strategy.
type StreamHandling string

const (
	StreamHandlingCopyAll           StreamHandling = "copy_all"
	StreamHandlingStripNonPreferred StreamHandling = "strip_non_preferred"
	StreamHandlingCopyPrimary       StreamHandling = "copy_primary"
	StreamHandlingTranscodeAAC      StreamHandling = "transcode_aac"
	StreamHandlingStripAll          StreamHandling = "strip_all"
	StreamHandlingCopyForcedOnly    StreamHandling = "copy_forced_only"
)

// BuildStreamArgs builds the -map / audio / subtitle portion of the ffmpeg
// argv for the preset handling strategies:
//
//	audio:    copy_all | strip_non_preferred | copy_primary | transcode_aac | strip_all
//	subtitle: copy_all | strip_non_preferred | copy_primary | strip_all
//
// copy_all/copy_all collapses to "-map 0 -c:a copy -c:s copy" unless the
// target is MP4 with bitmap subtitles, which MP4 cannot mux — those are
// dropped and the remaining streams mapped explicitly.
func BuildStreamArgs(container, outputFile string, streams []StreamInfo, audioHandling, subtitleHandling StreamHandling) []string {
	mp4 := isMP4Container(container, outputFile)
	if audioHandling == "" {
		audioHandling = StreamHandlingCopyAll
	}
	if subtitleHandling == "" {
		subtitleHandling = StreamHandlingCopyAll
	}

	var audioStreams, subStreams []StreamInfo
	for _, s := range streams {
		if s.Type == "audio" {
			audioStreams = append(audioStreams, s)
		} else if s.Type == "subtitle" {
			subStreams = append(subStreams, s)
		}
	}
	hasImageSubs := false
	for _, s := range subStreams {
		if !isTextSubtitleCodec(s.Codec) {
			hasImageSubs = true
			break
		}
	}

	// copy_all/copy_all with a known-safe layout collapses to a blanket map.
	// With an MP4 target and unknown stream layout (probe failed) we cannot
	// rule out bitmap subtitles, so the safe fallback below applies instead.
	if audioHandling == StreamHandlingCopyAll && subtitleHandling == StreamHandlingCopyAll && (!mp4 || !hasImageSubs) && !(mp4 && streams == nil) {
		return []string{"-map", "0", "-c:a", "copy", "-c:s", "copy"}
	}

	if streams == nil {
		// Probe failed: map video + all audio, drop subtitles, never crash.
		log.Printf("stream layout unknown; using safe fallback map (all audio copied, subtitles stripped)")
		return []string{"-map", "0:v:0", "-map", "0:a", "-c:a", "copy", "-sn"}
	}

	args := []string{"-map", "0:v:0"}

	switch audioHandling {
	case StreamHandlingCopyAll:
		for i := range audioStreams {
			args = append(args, "-map", streamRef("0:a", i))
		}
		args = append(args, "-c:a", "copy")
	case StreamHandlingStripNonPreferred:
		// Default streams first, then the first stream of each language
		// not already covered.
		seen := map[string]bool{}
		for i, s := range audioStreams {
			if s.IsDefault && !seen[s.Language] {
				args = append(args, "-map", streamRef("0:a", i))
				seen[s.Language] = true
			}
		}
		for i, s := range audioStreams {
			if !seen[s.Language] {
				args = append(args, "-map", streamRef("0:a", i))
				seen[s.Language] = true
			}
		}
		args = append(args, "-c:a", "copy")
	case StreamHandlingCopyPrimary:
		if len(audioStreams) > 0 {
			args = append(args, "-map", "0:a:0")
		}
		args = append(args, "-c:a", "copy")
	case StreamHandlingTranscodeAAC:
		for i := range audioStreams {
			args = append(args, "-map", streamRef("0:a", i))
		}
		args = append(args, "-c:a", "aac", "-b:a", "192k")
	case StreamHandlingStripAll:
		args = append(args, "-an")
	}

	switch subtitleHandling {
	case StreamHandlingCopyAll:
		for i, s := range subStreams {
			if mp4 && !isTextSubtitleCodec(s.Codec) {
				log.Printf("dropping bitmap subtitle stream 0:s:%d (%s): MP4 cannot mux it", i, s.Codec)
				continue
			}
			args = append(args, "-map", streamRef("0:s", i))
		}
		args = append(args, "-c:s", "copy")
	case StreamHandlingStripNonPreferred:
		for i, s := range subStreams {
			if !s.IsDefault {
				continue
			}
			if mp4 && !isTextSubtitleCodec(s.Codec) {
				log.Printf("dropping bitmap subtitle stream 0:s:%d (%s): MP4 cannot mux it", i, s.Codec)
				continue
			}
			args = append(args, "-map", streamRef("0:s", i))
		}
		args = append(args, "-c:s", "copy")
	case StreamHandlingStripAll:
		args = append(args, "-sn")
	}

	return args
}

// streamRef formats a per-type stream index as an ffmpeg map specifier.
func streamRef(prefix string, i int) string {
	return fmt.Sprintf("%s:%d", prefix, i)
}

// shouldIncludeSubtitle evaluates container compatibility, format policies (e.g. text_only),
// forced subtitle policies, and language whitelisting to determine whether a subtitle track is preserved.
func shouldIncludeSubtitle(
	s StreamInfo,
	isMp4 bool,
	subHandling StreamHandling,
	formatPolicy, forcedPolicy string,
	preferredLangs []string,
	langMode string,
) bool {
	isText := isTextSubtitleCodec(s.Codec)
	isForced := s.IsForced

	if isMp4 && !isText {
		return false
	}
	if formatPolicy == "text_only" && !isText && !isForced {
		return false
	}
	if subHandling == StreamHandlingCopyForcedOnly {
		return isForced
	}
	if langMode == "whitelist" && len(preferredLangs) > 0 {
		if forcedPolicy == "keep_always" && isForced {
			return true
		}
		return matchesAnyLanguage(s.Language, preferredLangs)
	}
	return true
}

// AssemblySidecar describes the local-only sources of non-video streams for
// final assembly. Both paths are scratch artifacts produced by the upfront
// demux pass; the remote source path must never appear in an assembly argv.
type AssemblySidecar struct {
	// DemuxPath is the local scratch container holding every audio track,
	// muxable subtitle track, and the chapter markers. Empty when the source
	// had no audio/subtitle streams.
	DemuxPath string
	// ChapterPath is the local ffmetadata chapter sidecar for sources with
	// chapters but no audio/subtitle streams (no demux container). Empty
	// otherwise.
	ChapterPath string
}

// BuildConcatArgs builds the final-assembly ffmpeg arguments: stitch the
// concatenated local video chunks (input 0, via the concat manifest) and mux
// audio, subtitles, and chapters from the local demux scratch container or
// chapter sidecar — a local-only operation with zero secondary reads of the
// (possibly network-mounted) source.
//
// Audio handling honors the preset strategy against the demux container's
// audio streams: copy_all (default) maps them all, copy_primary the first,
// strip_non_preferred the default plus first-of-each-language, transcode_aac
// re-encodes to AAC, strip_all maps none. Subtitle handling mirrors the
// previous source-backed logic, keyed on the demux container's stream
// layout: bitmap subtitles are dropped for MP4 targets, and the format
// policy / language whitelist / forced-only filters apply as before.
//
// The demux sidecar input carries -copyts so FFmpeg preserves the original
// stream start timestamps instead of re-basing the input to zero: without
// it, intentional track offsets in the source (e.g. a deliberate broadcast
// audio delay) are annihilated and the track plays early for the whole
// output. The concat-demuxer video input (input 0) is generated 0-based by
// the split/encode phases, so only the sidecar input needs the flag.
//
// When the source carried no side streams at all there is no second input:
// the output is pure video and no chapter flag is emitted.
func BuildConcatArgs(job TranscodeJob, concatFile string, sidecar AssemblySidecar, streams []StreamInfo) []string {
	concatArgs := []string{
		"-hide_banner",
		"-y",
		"-f", "concat",
		"-safe", "0",
		"-protocol_whitelist", "file,pipe",
		"-i", concatFile,
	}

	haveDemux := sidecar.DemuxPath != ""
	haveChapters := false
	switch {
	case haveDemux:
		concatArgs = append(concatArgs, "-copyts", "-i", sidecar.DemuxPath)
		haveChapters = true
	case sidecar.ChapterPath != "":
		concatArgs = append(concatArgs, "-i", sidecar.ChapterPath)
		haveChapters = true
	}

	concatArgs = append(concatArgs, "-map", "0")

	var audioCodecArgs []string
	if haveDemux {
		maps := assemblyAudioMaps("1", streams, job)
		concatArgs = append(concatArgs, maps...)
		if StreamHandling(job.Spec.AudioHandling) == StreamHandlingTranscodeAAC && len(maps) > 0 {
			audioCodecArgs = []string{"-c:a", "aac", "-b:a", "192k"}
		}
	}

	mp4 := isMP4Container(job.Spec.Container, job.OutputFile)
	if haveDemux {
		subHandling := StreamHandling(job.Spec.SubtitleHandling)
		if subHandling == "" {
			subHandling = StreamHandlingCopyAll
		}

		if subHandling != StreamHandlingStripAll && job.Spec.SubtitleFormatPolicy != "strip_all" {
			if streams == nil {
				if !mp4 {
					concatArgs = append(concatArgs, "-map", "1:s?")
				}
			} else {
				var subStreams []StreamInfo
				for _, s := range streams {
					if s.Type == "subtitle" {
						subStreams = append(subStreams, s)
					}
				}

				hasImageSubs := false
				for _, s := range subStreams {
					if !isTextSubtitleCodec(s.Codec) {
						hasImageSubs = true
						break
					}
				}

				hasSubFilter := job.Spec.SubtitleFormatPolicy == "text_only" ||
					(job.Spec.SubtitleLanguageMode == "whitelist" && len(job.Spec.PreferredSubtitleLanguages) > 0) ||
					subHandling == StreamHandlingCopyForcedOnly

				switch subHandling {
				case StreamHandlingCopyPrimary:
					if len(subStreams) > 0 {
						s := subStreams[0]
						if shouldIncludeSubtitle(s, mp4, subHandling, job.Spec.SubtitleFormatPolicy, job.Spec.ForcedSubtitlePolicy, job.Spec.PreferredSubtitleLanguages, job.Spec.SubtitleLanguageMode) {
							concatArgs = append(concatArgs, "-map", "1:s:0")
						} else if mp4 && !isTextSubtitleCodec(s.Codec) {
							log.Printf("dropping bitmap subtitle stream 1:s:0 (%s): MP4 cannot mux it", s.Codec)
						} else if job.Spec.SubtitleFormatPolicy == "text_only" && !isTextSubtitleCodec(s.Codec) && !s.IsForced {
							log.Printf("pruning bitmap subtitle stream 1:s:0 (%s) under text_only policy", s.Codec)
						}
					}

				case StreamHandlingStripNonPreferred:
					for i, s := range subStreams {
						if !s.IsDefault {
							continue
						}
						if shouldIncludeSubtitle(s, mp4, subHandling, job.Spec.SubtitleFormatPolicy, job.Spec.ForcedSubtitlePolicy, job.Spec.PreferredSubtitleLanguages, job.Spec.SubtitleLanguageMode) {
							concatArgs = append(concatArgs, "-map", streamRef("1:s", i))
						} else if mp4 && !isTextSubtitleCodec(s.Codec) {
							log.Printf("dropping bitmap subtitle stream 1:s:%d (%s): MP4 cannot mux it", i, s.Codec)
						} else if job.Spec.SubtitleFormatPolicy == "text_only" && !isTextSubtitleCodec(s.Codec) && !s.IsForced {
							log.Printf("pruning bitmap subtitle stream 1:s:%d (%s) under text_only policy", i, s.Codec)
						}
					}

				case StreamHandlingCopyForcedOnly:
					for i, s := range subStreams {
						if shouldIncludeSubtitle(s, mp4, subHandling, job.Spec.SubtitleFormatPolicy, job.Spec.ForcedSubtitlePolicy, job.Spec.PreferredSubtitleLanguages, job.Spec.SubtitleLanguageMode) {
							concatArgs = append(concatArgs, "-map", streamRef("1:s", i))
						} else if mp4 && !isTextSubtitleCodec(s.Codec) {
							log.Printf("dropping bitmap subtitle stream 1:s:%d (%s): MP4 cannot mux it", i, s.Codec)
						}
					}

				default: // StreamHandlingCopyAll or unconfigured
					if !hasSubFilter {
						if (!mp4 || !hasImageSubs) && !(mp4 && streams == nil) {
							concatArgs = append(concatArgs, "-map", "1:s?")
						} else {
							for i, s := range subStreams {
								if shouldIncludeSubtitle(s, mp4, subHandling, job.Spec.SubtitleFormatPolicy, job.Spec.ForcedSubtitlePolicy, job.Spec.PreferredSubtitleLanguages, job.Spec.SubtitleLanguageMode) {
									concatArgs = append(concatArgs, "-map", streamRef("1:s", i))
								} else if mp4 && !isTextSubtitleCodec(s.Codec) {
									log.Printf("dropping bitmap subtitle stream 1:s:%d (%s): MP4 cannot mux it", i, s.Codec)
								}
							}
						}
					} else {
						for i, s := range subStreams {
							if shouldIncludeSubtitle(s, mp4, subHandling, job.Spec.SubtitleFormatPolicy, job.Spec.ForcedSubtitlePolicy, job.Spec.PreferredSubtitleLanguages, job.Spec.SubtitleLanguageMode) {
								concatArgs = append(concatArgs, "-map", streamRef("1:s", i))
							} else if mp4 && !isTextSubtitleCodec(s.Codec) {
								log.Printf("dropping bitmap subtitle stream 1:s:%d (%s): MP4 cannot mux it", i, s.Codec)
							} else if job.Spec.SubtitleFormatPolicy == "text_only" && !isTextSubtitleCodec(s.Codec) && !s.IsForced {
								log.Printf("pruning bitmap subtitle stream 1:s:%d (%s) under text_only policy", i, s.Codec)
							}
						}
					}
				}
			}
		}
	}

	concatArgs = append(concatArgs, "-c", "copy")
	// Audio codec overrides must follow the blanket -c copy so they win.
	concatArgs = append(concatArgs, audioCodecArgs...)
	if job.Spec.StripMetadata {
		concatArgs = append(concatArgs, "-map_metadata", "-1")
	}
	if haveChapters {
		concatArgs = append(concatArgs, "-map_chapters", "1")
	}
	if mp4 {
		concatArgs = append(concatArgs, "-movflags", "+faststart")
	}
	concatArgs = append(concatArgs, job.OutputFile)
	return concatArgs
}

// BuildConcatManifest generates the contents of an FFmpeg concat demuxer manifest (concat.txt)
// from a list of chunk file paths. It strictly rejects filenames containing carriage returns
// or newlines, normalizes Windows backslash path separators to forward slashes to prevent
// escape character misinterpretation, and escapes single quotes for FFmpeg's concat parser.
func BuildConcatManifest(segmentFiles []string) (string, error) {
	return BuildConcatManifestWithDurations(segmentFiles, nil)
}

// BuildConcatManifestWithDurations is BuildConcatManifest for chunks whose
// exact source-timeline durations are known from the split phase's segment
// timing manifest. Each durations[i] is written as an explicit duration
// directive for segmentFiles[i], so the concat demuxer advances its timeline
// by the keyframe-boundary split duration instead of the chunk container's
// B-frame-flush-padded duration — eliminating cumulative per-seam video
// drift against the un-split audio. A nil or empty durations slice falls
// back to a bare file list (the historical behavior); otherwise the slice
// length must match the file list and every duration must be positive.
func BuildConcatManifestWithDurations(segmentFiles []string, durations []float64) (string, error) {
	if len(durations) == 0 {
		var concatLines []string
		for _, f := range segmentFiles {
			line, err := concatManifestFileLine(f)
			if err != nil {
				return "", err
			}
			concatLines = append(concatLines, line)
		}
		if len(concatLines) == 0 {
			return "", nil
		}
		return strings.Join(concatLines, "\n") + "\n", nil
	}

	if len(durations) != len(segmentFiles) {
		return "", fmt.Errorf("durations length %d does not match chunk count %d", len(durations), len(segmentFiles))
	}
	var concatLines []string
	for i, f := range segmentFiles {
		line, err := concatManifestFileLine(f)
		if err != nil {
			return "", err
		}
		if durations[i] <= 0 {
			return "", fmt.Errorf("chunk %d has non-positive duration %.6f", i, durations[i])
		}
		concatLines = append(concatLines, line, fmt.Sprintf("duration %.6f", durations[i]))
	}
	if len(concatLines) == 0 {
		return "", nil
	}
	return strings.Join(concatLines, "\n") + "\n", nil
}

// concatManifestFileLine renders one escaped file entry for the concat
// demuxer manifest.
func concatManifestFileLine(f string) (string, error) {
	if strings.ContainsAny(f, "\r\n") {
		return "", fmt.Errorf("chunk path %q contains invalid newline characters", f)
	}
	normalized := strings.ReplaceAll(f, "\\", "/")
	safeName := strings.ReplaceAll(normalized, "'", "'\\''")
	return fmt.Sprintf("file '%s'", safeName), nil
}

// assemblyAudioMaps selects the demux container's audio tracks for final
// assembly, mirroring the encode-time audio hygiene the chunk path applied
// before video-only chunks: commentary tracks are pruned, and a
// preferred-language whitelist filters dubs (falling back to the
// primary/default track so the output is never silent). A failed probe
// degrades to the blanket optional map rather than failing the job.
func assemblyAudioMaps(prefix string, streams []StreamInfo, job TranscodeJob) []string {
	audioHandling := StreamHandling(job.Spec.AudioHandling)
	if audioHandling == StreamHandlingStripAll {
		return nil
	}
	blanket := []string{"-map", prefix + ":a?"}
	if streams == nil {
		return blanket
	}
	var audio []StreamInfo
	for _, s := range streams {
		if s.Type == "audio" {
			audio = append(audio, s)
		}
	}
	if len(audio) == 0 {
		return nil
	}
	ref := func(i int) string { return streamRef(prefix+":a", i) }

	// Prune commentary tracks (the disposition flag is already folded into
	// IsCommentary by ProbeStreams).
	var nonCommentary []int
	for i, s := range audio {
		if s.IsCommentary || isCommentaryTrack(s.Title, s.Description, false) {
			log.Printf("pruning commentary audio track %s (codec: %s, title: %q, lang: %q)", ref(i), s.Codec, s.Title, s.Language)
			continue
		}
		nonCommentary = append(nonCommentary, i)
	}

	if audioHandling == StreamHandlingCopyPrimary {
		if len(nonCommentary) > 0 {
			return []string{"-map", ref(nonCommentary[0])}
		}
		return []string{"-map", ref(0)}
	}

	if audioHandling == StreamHandlingStripNonPreferred {
		// Default streams first, then the first stream of each language not
		// already covered.
		var maps []string
		seen := map[string]bool{}
		for i, s := range audio {
			if s.IsDefault && !seen[s.Language] {
				maps = append(maps, "-map", ref(i))
				seen[s.Language] = true
			}
		}
		for i, s := range audio {
			if !seen[s.Language] {
				maps = append(maps, "-map", ref(i))
				seen[s.Language] = true
			}
		}
		return maps
	}

	// copy_all / transcode_aac: compact blanket map when no hygiene selection
	// is active; enumerated maps otherwise.
	langs := job.Spec.PreferredAudioLanguages
	hasLangFilter := len(langs) > 0 && job.Spec.AudioLanguageMode != "all"
	if !hasLangFilter && len(nonCommentary) == len(audio) {
		return blanket
	}

	pool := nonCommentary
	if len(pool) == 0 {
		for i := range audio {
			pool = append(pool, i)
		}
	}
	if hasLangFilter {
		var maps []string
		for _, i := range nonCommentary {
			if matchesAnyLanguage(audio[i].Language, langs) {
				maps = append(maps, "-map", ref(i))
			} else {
				log.Printf("stripping foreign dub audio track %s (lang: %s, codec: %s)", ref(i), audio[i].Language, audio[i].Codec)
			}
		}
		if len(maps) > 0 {
			return maps
		}
		// No audio matched the whitelist: fall back to the default (or
		// primary) track so the output is never silent.
		fallback := pool[0]
		for _, i := range pool {
			if audio[i].IsDefault {
				fallback = i
				break
			}
		}
		log.Printf("no audio tracks matched preferred languages; using fallback audio track %s (lang: %s)", ref(fallback), audio[fallback].Language)
		return []string{"-map", ref(fallback)}
	}
	var maps []string
	for _, i := range nonCommentary {
		maps = append(maps, "-map", ref(i))
	}
	return maps
}

// buildX265Params composes the -x265-params string from the preset tuning
// fields. The user's custom_flags are appended last so explicit values win.
func buildX265Params(aqMode int, filmGrainSao bool, contentTuning, customFlags string) string {
	var parts []string
	if aqMode > 0 {
		parts = append(parts, "aq-mode="+strconv.Itoa(aqMode))
	}
	if !filmGrainSao {
		parts = append(parts, "sao=0")
	}
	switch contentTuning {
	case "film":
		parts = append(parts, "tune=film")
	case "grain":
		parts = append(parts, "tune=grain")
	case "animation":
		parts = append(parts, "tune=animation")
	}
	if customFlags != "" {
		parts = append(parts, customFlags)
	}
	return strings.Join(parts, ":")
}

// buildSvtAV1Params composes the -svtav1-params string from the preset tuning
// fields and tier defaults. Explicit user custom_flags are appended last so explicit values win.
func buildSvtAV1Params(contentTuning string, filmGrainSao bool, customFlags string, archivistGrade bool, presetSpeed string) string {
	var parts []string
	if archivistGrade {
		parts = append(parts, "tune=0", "film-grain=8")
	} else if contentTuning != "" || filmGrainSao {
		tune := 0
		if contentTuning == "animation" {
			tune = 1
		}
		filmGrain := 4
		switch presetSpeed {
		case "fast", "8", "9", "10":
			if filmGrainSao {
				filmGrain = 4
			} else {
				filmGrain = 0
			}
		case "slow", "3", "4":
			filmGrain = 8
		default:
			if filmGrainSao {
				filmGrain = 8
			} else {
				filmGrain = 4
			}
		}
		parts = append(parts, "tune="+strconv.Itoa(tune), "film-grain="+strconv.Itoa(filmGrain))
	}
	if customFlags != "" {
		parts = append(parts, customFlags)
	}
	return strings.Join(parts, ":")
}

// selectQualityValue implements CRF-over-quality_target precedence: the
// advanced CRF field (0–51) wins when it is a valid positive value, otherwise
// fall back to quality_target. Zero means "no quality flag".
//
// The quality_target band is engine-dependent: NVENC CQ (-cq:v) and QSV ICQ
// (-global_quality) accept the full 0–51 range, so those presets may carry any
// value the dashboard's slider offers; every other encoder family keeps the
// conservative 18–26 band (its 0–51 escape hatch is the advanced CRF field).
func selectQualityValue(encoder string, crf, qualityTarget int) int {
	maxCrf := 51
	if encoder == "libsvtav1" {
		maxCrf = 63
	}
	if crf > 0 && crf <= maxCrf {
		return crf
	}
	if strings.HasSuffix(encoder, "_nvenc") || strings.HasSuffix(encoder, "_qsv") {
		if qualityTarget > 0 && qualityTarget <= 51 {
			return qualityTarget
		}
		return 0
	}
	if encoder == "libsvtav1" {
		if qualityTarget >= 15 && qualityTarget <= 45 {
			return qualityTarget
		}
		return 0
	}
	if qualityTarget >= 18 && qualityTarget <= 26 {
		return qualityTarget
	}
	return 0
}

// qualityArgsFor renders the quality value with the flag the encoder family
// actually understands.
func qualityArgsFor(encoder string, v int) []string {
	if v == 0 {
		return nil
	}
	n := strconv.Itoa(v)
	switch {
	case strings.HasSuffix(encoder, "_nvenc"):
		return []string{"-rc:v", "vbr", "-cq:v", n, "-b:v", "0"}
	case strings.HasSuffix(encoder, "_qsv"):
		return []string{"-global_quality", n}
	case strings.HasSuffix(encoder, "_amf"):
		return []string{"-rc", "cqp", "-qp_i", n, "-qp_p", strconv.Itoa(v + 2)}
	case strings.HasSuffix(encoder, "_videotoolbox"):
		q := int(math.Round(70.0 - float64(v-18)*2.5))
		if q < 1 {
			q = 1
		} else if q > 100 {
			q = 100
		}
		return []string{"-q:v", strconv.Itoa(q)}
	default:
		return []string{"-crf", n}
	}
}

// defaultPresetFor is the agent's preferred preset for the encoder family,
// matching deriveRecommendation, used when the preset does not specify one.
func defaultPresetFor(encoder string) string {
	switch encoder {
	case "hevc_nvenc", "av1_nvenc":
		return "p6"
	case "h264_nvenc":
		return "p4"
	case "hevc_qsv", "h264_qsv", "av1_qsv":
		return "medium"
	case "libx265", "libx264":
		return "medium"
	case "libsvtav1":
		return "6"
	default:
		return ""
	}
}

// pixFmtFor selects the pixel format for the requested color depth. Hardware
// HEVC encoders reject yuv420p10le and require p010le. An empty return means
// 8-bit (no -pix_fmt flag); callers degrade with a log when 10-bit is
// requested but unsupported rather than failing the job.
func pixFmtFor(encoder, colorDepth string) string {
	if colorDepth != "10bit" {
		return ""
	}
	switch encoder {
	case "libx265", "libsvtav1":
		return "yuv420p10le"
	case "hevc_nvenc", "hevc_qsv", "av1_nvenc", "av1_qsv":
		return "p010le"
	default:
		return ""
	}
}

// ValidateX265Params dry-runs custom x265 flags through a throwaway lavfi
// encode before they ever reach a real job's exec.Command. Nonzero exit
// rejects the flags with the ffmpeg stderr.
func ValidateX265Params(ffmpegPath, flags string) error {
	return dryRunCustomFlags(ffmpegPath, "libx265", "-x265-params", flags, "-f", "null", "-")
}

// ValidateRawFlags dry-runs raw custom flags through a throwaway lavfi
// encode with the job's actual encoder and container muxer before they reach
// a real job's exec.Command. Nonzero exit rejects the flags with the ffmpeg
// stderr.
func ValidateRawFlags(ffmpegPath, encoder string, mp4 bool, tokens []string) error {
	mux := "matroska"
	if mp4 {
		mux = "mp4"
	}
	tail := append(append([]string{}, tokens...), "-f", mux, os.DevNull)
	return dryRunCustomFlags(ffmpegPath, encoder, tail...)
}

// ValidateVendorTuning dry-runs vendor hardware tuning flags through a throwaway lavfi
// encode with the job's actual encoder before executing full transcode passes.
func ValidateVendorTuning(ffmpegPath, encoder string, qualityArgs, tuningArgs []string) error {
	tail := append(append([]string{}, qualityArgs...), tuningArgs...)
	tail = append(tail, "-f", "null", "-")
	return dryRunCustomFlags(ffmpegPath, encoder, tail...)
}

// dryRunCustomFlags runs a throwaway lavfi-color encode with the given
// encoder, appending tail after "-c:v <encoder>" so the caller controls the
// flags under test and the output target. Nonzero exit rejects the flags
// with the ffmpeg stderr. An empty ffmpegPath skips validation (and logs),
// which only direct/test use ever hits: real jobs always carry a path.
func dryRunCustomFlags(ffmpegPath, encoder string, tail ...string) error {
	if ffmpegPath == "" {
		log.Printf("ffmpeg path empty: skipping custom-flags dry-run validation")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	args := []string{
		"-nostdin",
		"-v", "error",
		"-f", "lavfi", "-i", "color=black:s=1920x1080:d=1",
		"-c:v", encoder,
	}
	args = append(args, tail...)
	scratchDir, scratchErr := os.MkdirTemp("", "sm-flags-dryrun-*")
	if scratchErr == nil {
		defer os.RemoveAll(scratchDir)
	}
	cmd := execCommandContext(ctx, ffmpegPath, args...)
	if scratchDir != "" {
		cmd.Dir = scratchDir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("custom flags rejected by ffmpeg dry-run: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}


// splitRawFlags splits raw custom flags into tokens, honoring 'single' and
// "double" quotes (quotes group whitespace and are stripped). An
// unterminated quote is an error.
func splitRawFlags(flags string) ([]string, error) {
	return protocol.SplitRawFlags(flags)
}

// SanitizeCustomFlags inspects custom flags in either raw or legacy mode,
// rejecting bare non-flag tokens, multiple output targets, unauthorized
// paths, and forbidden ffmpeg options.
func SanitizeCustomFlags(flags string) ([]string, error) {
	return protocol.SanitizeCustomFlags(flags)
}

// GlobalSettings represents the active tenant transcode settings.
type GlobalSettings struct {
	HWAccelPreference          string   `json:"hw_accel_preference"`
	AudioCodecPolicy           string   `json:"audio_codec_policy"`
	SubtitleFormatPolicy       string   `json:"subtitle_format_policy"`
	ForcedSubtitlePolicy       string   `json:"forced_subtitle_policy"`
	AudioLanguageMode          string   `json:"audio_language_mode"`
	PreferredAudioLanguages    []string `json:"preferred_audio_languages"`
	SubtitleLanguageMode       string   `json:"subtitle_language_mode"`
	PreferredSubtitleLanguages []string `json:"preferred_subtitle_languages"`
	StripMetadata              bool     `json:"strip_metadata"`
}

// Preset represents an encoding preset (default built-in or custom).
type Preset struct {
	ID                         string   `json:"id,omitempty"`
	Name                       string   `json:"name"`
	IsBuiltin                  bool     `json:"is_builtin"`
	TargetCodec                string   `json:"target_codec"`
	Container                  string   `json:"container"`
	CRF                        int      `json:"crf"`
	PresetSpeed                string   `json:"preset_speed"`
	AudioCodec                 string   `json:"audio_codec"`
	EncoderEngine              string   `json:"encoder_engine"`
	QualityTarget              int      `json:"quality_target"`
	AudioHandling              string   `json:"audio_handling"`
	SubtitleHandling           string   `json:"subtitle_handling"`
	ColorDepth                 string   `json:"color_depth"`
	CustomFlags                string   `json:"custom_flags"`
	QualityTier                string   `json:"quality_tier"`
	AudioLanguageMode          string   `json:"audio_language_mode,omitempty"`
	PreferredAudioLanguages    []string `json:"preferred_audio_languages,omitempty"`
	SubtitleLanguageMode       string   `json:"subtitle_language_mode,omitempty"`
	PreferredSubtitleLanguages []string `json:"preferred_subtitle_languages,omitempty"`
	SubtitleFormatPolicy       string   `json:"subtitle_format_policy,omitempty"`
}

// DefaultPresets returns the default built-in presets: The Archivist, Balanced, and Max Space.
func DefaultPresets() map[string]Preset {
	return map[string]Preset{
		"The Archivist": {
			Name:          "The Archivist",
			IsBuiltin:     true,
			TargetCodec:   "hevc",
			Container:     "mkv",
			ColorDepth:    "10bit",
			EncoderEngine: "auto",
			QualityTier:   protocol.QualityTierArchivist,
		},
		"Balanced": {
			Name:          "Balanced",
			IsBuiltin:     true,
			TargetCodec:   "hevc",
			Container:     "mkv",
			ColorDepth:    "10bit",
			EncoderEngine: "auto",
			PresetSpeed:   "medium",
			QualityTarget: 19,
			CRF:           22,
		},
		"Max Space": {
			Name:          "Max Space",
			IsBuiltin:     true,
			TargetCodec:   "hevc",
			Container:     "mkv",
			ColorDepth:    "10bit",
			EncoderEngine: "auto",
			PresetSpeed:   "fast",
			QualityTarget: 25,
			CRF:           26,
		},
	}
}

// BuildStreamMaps builds stream map arguments for video, audio, and subtitles based on language preferences.
// When language filters are specified, it filters streams using -map 0:v:0 -map 0:a:m:language:<lang>? and -map 0:s:m:language:<lang>?.
// If filters are blank or no streams match, it defaults to copying all streams (-map 0:v:0 -map 0:a? -map 0:s?).
func BuildStreamMaps(audioLangs, subLangs []string, streams []StreamInfo) []string {
	if len(audioLangs) == 0 && len(subLangs) == 0 {
		return []string{"-map", "0:v:0", "-map", "0:a?", "-map", "0:s?"}
	}

	hasAudioMatch := false
	hasSubMatch := false
	if streams != nil {
		for _, s := range streams {
			if s.Type == "audio" && matchesAnyLanguage(s.Language, audioLangs) {
				hasAudioMatch = true
			}
			if s.Type == "subtitle" && matchesAnyLanguage(s.Language, subLangs) {
				hasSubMatch = true
			}
		}
		audioFilterActive := len(audioLangs) > 0
		subFilterActive := len(subLangs) > 0
		if (audioFilterActive && !hasAudioMatch) && (subFilterActive && !hasSubMatch) {
			return []string{"-map", "0:v:0", "-map", "0:a?", "-map", "0:s?"}
		}
		if audioFilterActive && !subFilterActive && !hasAudioMatch {
			return []string{"-map", "0:v:0", "-map", "0:a?", "-map", "0:s?"}
		}
		if subFilterActive && !audioFilterActive && !hasSubMatch {
			return []string{"-map", "0:v:0", "-map", "0:a?", "-map", "0:s?"}
		}
	}

	maps := []string{"-map", "0:v:0"}
	if len(audioLangs) > 0 {
		if streams != nil && !hasAudioMatch {
			maps = append(maps, "-map", "0:a?")
		} else {
			for _, lang := range audioLangs {
				maps = append(maps, "-map", fmt.Sprintf("0:a:m:language:%s?", lang))
			}
		}
	} else {
		maps = append(maps, "-map", "0:a?")
	}

	if len(subLangs) > 0 {
		if streams != nil && !hasSubMatch {
			maps = append(maps, "-map", "0:s?")
		} else {
			for _, lang := range subLangs {
				maps = append(maps, "-map", fmt.Sprintf("0:s:m:language:%s?", lang))
			}
		}
	} else {
		maps = append(maps, "-map", "0:s?")
	}

	return maps
}

// ResolvePreset resolves a preset against tenant Global Settings and agent hardware capabilities.
// For default presets (IsBuiltin == true), it inherits unassigned / "auto" fields directly from Global Settings:
// - Hardware Acceleration: If preset engine is "auto", resolves against Global Settings + Agent hardware audit.
// - Audio Transcode Policy: Applies global audio policy.
// - Subtitle Format Policy: Applies global subtitle policy.
// - Language Filters: Inherits global preferred audio and subtitle languages.
// Custom presets retain the ability to explicitly override any of these fields.
// Quality and rate control are calibrated properly for Hardware (QSV ICQ) vs CPU (CRF).
func ResolvePreset(preset Preset, global GlobalSettings, caps *EncoderCaps) jobspec.JobSpec {
	isDefault := preset.IsBuiltin || isDefaultPresetName(preset.Name)

	targetCodec := preset.TargetCodec
	if targetCodec == "" {
		targetCodec = "hevc"
	}
	container := preset.Container
	if container == "" {
		container = "mkv"
	}
	colorDepth := preset.ColorDepth
	if colorDepth == "" {
		colorDepth = "10bit"
	}

	// 1. Hardware Acceleration engine resolution
	engine := preset.EncoderEngine
	if isDefault || engine == "" || engine == "auto" {
		pref := global.HWAccelPreference
		if pref == "" || pref == "auto" {
			if caps != nil {
				if caps.HasIntel || caps.HasQSVHEVC || caps.RecommendedEncoder == "hevc_qsv" {
					engine = "qsv"
				} else if caps.HasNvencHEVC || caps.RecommendedEncoder == "hevc_nvenc" {
					engine = "nvenc"
				} else {
					engine = "cpu"
				}
			} else {
				engine = "cpu"
			}
		} else {
			engine = pref
		}
	}

	// 2. Audio policy
	audioHandling := preset.AudioHandling
	audioCodec := preset.AudioCodec
	if isDefault || audioHandling == "" || audioHandling == "auto" {
		if global.AudioCodecPolicy != "" {
			audioHandling = global.AudioCodecPolicy
		} else {
			audioHandling = "copy_all"
		}
	}
	if audioHandling == "transcode_aac" {
		audioCodec = "aac"
	} else if audioHandling == "copy" || audioHandling == "copy_all" {
		audioCodec = "copy"
	}

	// 3. Subtitle policy
	subHandling := preset.SubtitleHandling
	subFormatPolicy := preset.SubtitleFormatPolicy
	if isDefault || (subHandling == "" && subFormatPolicy == "") {
		if global.SubtitleFormatPolicy != "" {
			subHandling = global.SubtitleFormatPolicy
			subFormatPolicy = global.SubtitleFormatPolicy
		} else {
			subHandling = "copy_all"
			subFormatPolicy = "copy_all"
		}
	}
	if subFormatPolicy == "" {
		subFormatPolicy = subHandling
	}

	// 4. Language filters
	preferredAudioLangs := preset.PreferredAudioLanguages
	audioLangMode := preset.AudioLanguageMode
	if isDefault || len(preferredAudioLangs) == 0 {
		preferredAudioLangs = global.PreferredAudioLanguages
		audioLangMode = global.AudioLanguageMode
	}
	preferredSubLangs := preset.PreferredSubtitleLanguages
	subLangMode := preset.SubtitleLanguageMode
	if isDefault || len(preferredSubLangs) == 0 {
		preferredSubLangs = global.PreferredSubtitleLanguages
		subLangMode = global.SubtitleLanguageMode
	}

	// 5. Quality & Rate Control Calibration (Hardware vs. CPU)
	crf := preset.CRF
	qualityTarget := preset.QualityTarget
	presetSpeed := preset.PresetSpeed
	qualityTier := preset.QualityTier

	lowerName := strings.ToLower(preset.Name)
	if strings.Contains(lowerName, "archivist") || qualityTier == protocol.QualityTierArchivist {
		qualityTier = protocol.QualityTierArchivist
		colorDepth = "10bit"
		if engine == "qsv" {
			crf = 0
			qualityTarget = 13
			presetSpeed = "slower"
		} else if engine == "cpu" {
			crf = 18
			qualityTarget = 0
			presetSpeed = "slow"
		}
	} else if strings.Contains(lowerName, "balanced") {
		colorDepth = "10bit"
		if engine == "qsv" {
			crf = 0
			qualityTarget = 19
			presetSpeed = "medium"
		} else if engine == "cpu" {
			crf = 22
			qualityTarget = 0
			presetSpeed = "medium"
		}
	} else if strings.Contains(lowerName, "space") {
		colorDepth = "10bit"
		if engine == "qsv" {
			crf = 0
			qualityTarget = 25
			presetSpeed = "fast"
		} else if engine == "cpu" {
			crf = 26
			qualityTarget = 0
			presetSpeed = "fast"
		}
	}

	return jobspec.JobSpec{
		Container:                  container,
		EncoderEngine:              engine,
		CRF:                        crf,
		PresetSpeed:                presetSpeed,
		AudioCodec:                 audioCodec,
		QualityTarget:              qualityTarget,
		AudioHandling:              audioHandling,
		SubtitleHandling:           subHandling,
		ColorDepth:                 colorDepth,
		CustomFlags:                preset.CustomFlags,
		QualityTier:                qualityTier,
		PreferredAudioLanguages:    append([]string{}, preferredAudioLangs...),
		AudioLanguageMode:          audioLangMode,
		SubtitleFormatPolicy:       subFormatPolicy,
		ForcedSubtitlePolicy:       global.ForcedSubtitlePolicy,
		PreferredSubtitleLanguages: append([]string{}, preferredSubLangs...),
		SubtitleLanguageMode:       subLangMode,
		StripMetadata:              global.StripMetadata,
	}
}

func isDefaultPresetName(name string) bool {
	lower := strings.ToLower(name)
	return strings.Contains(lower, "archivist") || strings.Contains(lower, "balanced") || strings.Contains(lower, "space")
}

// ResolvePresetJob creates a TranscodeJob from a resolved preset and settings.
func ResolvePresetJob(preset Preset, global GlobalSettings, caps *EncoderCaps, inputFile, outputFile string) TranscodeJob {
	spec := ResolvePreset(preset, global, caps)
	targetCodec := preset.TargetCodec
	if targetCodec == "" {
		targetCodec = "hevc"
	}
	return TranscodeJob{
		InputFile:  inputFile,
		OutputFile: outputFile,
		Codec:      targetCodec,
		Spec:       spec,
		Caps:       caps,
	}
}

// BuildFfmpegArgs assembles the full ffmpeg argv. streams may be nil (probe
// failed) — stream handling then degrades to safe defaults.
func BuildFfmpegArgs(job TranscodeJob, streams []StreamInfo) ([]string, error) {
	builder, err := NewCommandBuilderFromJob(job, streams)
	if err != nil {
		return nil, err
	}
	return builder.Build()
}

func buildFfmpegArgs(job TranscodeJob, streams []StreamInfo) ([]string, error) {
	return BuildFfmpegArgs(job, streams)
}


// ffprobeNextTo resolves the ffprobe binary expected next to the ffmpeg
// binary, mirroring the orchestrator's convention.
func ffprobeNextTo(ffmpegPath string) string {
	return filepath.Join(filepath.Dir(ffmpegPath), "ffprobe")
}

