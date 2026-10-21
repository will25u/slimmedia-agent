package ffmpeg

import (
	"reflect"
	"strings"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/jobspec"
)

// TestCommandBuilderDeterministicOrdering verifies the builder maintains strict
// phase ordering across all argument tiers.
func TestCommandBuilderDeterministicOrdering(t *testing.T) {
	builder := NewCommandBuilder("/media/in.mkv", "/media/out.mkv")
	builder.SetStreams(nil)
	if err := builder.SetAudio("copy", "", StreamHandlingCopyAll); err != nil {
		t.Fatalf("SetAudio: %v", err)
	}
	if err := builder.SetVideoCodec("libx264"); err != nil {
		t.Fatalf("SetVideoCodec: %v", err)
	}
	builder.SetQuality(24, 0)
	if err := builder.SetPreset("fast"); err != nil {
		t.Fatalf("SetPreset: %v", err)
	}

	got, err := builder.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	want := []string{
		// Phase 1: Tier 1 Pre-Input
		"-hide_banner", "-nostdin", "-y", "-fflags", "+genpts", "-probesize", "10000000", "-analyzeduration", "10000000",
		"-protocol_whitelist", "file,pipe",
		// Phase 2: Input File
		"-i", "/media/in.mkv",
		// Phase 3: Tier 2 Stream Maps
		"-map", "0:v?", "-map", "0:a?", "-map", "0:s?",
		// Phase 4: Tier 1 Post-Input & Metadata
		"-ignore_unknown", "-map_metadata", "0", "-map_chapters", "0",
		"-max_muxing_queue_size", "4096",
		// Full-stream timestamp preservation (#200)
		"-copyts",
		// Phase 5: Tier 3 Audio Encoding
		"-c:a", "copy",
		// Phase 6: Container Flags
		"-c:s", "copy",
		// Phase 7: Tier 3 Video Pipeline
		"-c:v", "libx264", "-crf", "24", "-preset", "fast",
		// Phase 8: Tier 1 Telemetry
		"-progress", "pipe:1", "-nostats",
		// Phase 9: Output Target
		"/media/out.mkv",
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("deterministic ordering mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestCommandBuilderTier1Immutability verifies that Tier 1 engine plumbing
// arguments cannot be overridden or removed by custom flags or higher tiers.
func TestCommandBuilderTier1Immutability(t *testing.T) {
	forbiddenTier1CustomFlags := []string{
		"-nostdin",
		"-hide_banner",
		"-y",
		"-fflags +genpts",
		"-protocol_whitelist file,pipe",
		"-max_muxing_queue_size 4096",
		"-ignore_unknown",
		"-progress pipe:1",
		"-nostats",
		"-i /etc/passwd",
	}

	for _, flags := range forbiddenTier1CustomFlags {
		t.Run(flags, func(t *testing.T) {
			builder := NewCommandBuilder("/media/in.mkv", "/media/out.mkv")
			if err := builder.SetVideoCodec("libx264"); err != nil {
				t.Fatalf("SetVideoCodec: %v", err)
			}
			err := builder.SetCustomFlags("", flags)
			if err == nil {
				t.Fatalf("expected custom flags %q to be rejected as immutable Tier 1, got nil error", flags)
			}
			if !strings.Contains(err.Error(), "immutable") && !strings.Contains(err.Error(), "forbidden token") {
				t.Errorf("expected error mentioning immutable or forbidden token, got: %v", err)
			}
		})
	}
}

// TestCommandBuilderAudioCodecAndBitrate verifies Tier 3 preset audio mapping.
func TestCommandBuilderAudioCodecAndBitrate(t *testing.T) {
	tests := []struct {
		name         string
		audioCodec   string
		audioBitrate string
		handling     StreamHandling
		wantTokens   []string
		wantErr      bool
	}{
		{
			name:       "unconfigured defaults to copy",
			wantTokens: []string{"-c:a", "copy"},
		},
		{
			name:       "explicit copy",
			audioCodec: "copy",
			wantTokens: []string{"-c:a", "copy"},
		},
		{
			name:       "aac with unconfigured bitrate defaults to 192k",
			audioCodec: "aac",
			wantTokens: []string{"-c:a", "aac", "-b:a", "192k"},
		},
		{
			name:         "aac with explicit bitrate",
			audioCodec:   "aac",
			audioBitrate: "256k",
			wantTokens:   []string{"-c:a", "aac", "-b:a", "256k"},
		},
		{
			name:     "transcode_aac handling sets aac and 192k",
			handling: StreamHandlingTranscodeAAC,
			wantTokens: []string{"-c:a", "aac", "-b:a", "192k"},
		},
		{
			name:       "opus without bitrate",
			audioCodec: "opus",
			wantTokens: []string{"-c:a", "opus"},
		},
		{
			name:         "opus with bitrate",
			audioCodec:   "opus",
			audioBitrate: "128k",
			wantTokens:   []string{"-c:a", "opus", "-b:a", "128k"},
		},
		{
			name:       "strip_all audio emits -an",
			handling:   StreamHandlingStripAll,
			wantTokens: []string{"-an"},
		},
		{
			name:       "invalid audio codec errors",
			audioCodec: "nonexistent_audio_codec",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := NewCommandBuilder("/media/in.mkv", "/media/out.mkv")
			if err := builder.SetVideoCodec("libx264"); err != nil {
				t.Fatalf("SetVideoCodec: %v", err)
			}
			err := builder.SetAudio(tt.audioCodec, tt.audioBitrate, tt.handling)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for audio codec %q, got nil", tt.audioCodec)
				}
				return
			}
			if err != nil {
				t.Fatalf("SetAudio unexpected error: %v", err)
			}

			args, err := builder.Build()
			if err != nil {
				t.Fatalf("Build failed: %v", err)
			}

			if !containsSubsequence(args, tt.wantTokens) {
				t.Errorf("expected args to contain %v, got %v", tt.wantTokens, args)
			}

			if tt.handling == StreamHandlingStripAll && containsArg(args, "-c:a") {
				t.Errorf("strip_all must not emit -c:a, got %v", args)
			}
		})
	}
}

// TestCommandBuilderStripAllEmitsSnAtBuildTime verifies that strip_all subtitle
// handling cleanly emits -sn at build time without retroactive string inspection
// or array slicing in the runner.
func TestCommandBuilderStripAllEmitsSnAtBuildTime(t *testing.T) {
	builder := NewCommandBuilder("/media/in.mkv", "/media/chunk_0000.mkv")
	builder.SetSubtitleHandling(StreamHandlingStripAll)
	builder.SetStreams(tuningTestStreams)
	if err := builder.SetVideoCodec("libx265"); err != nil {
		t.Fatalf("SetVideoCodec: %v", err)
	}

	args, err := builder.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Must cleanly emit -sn
	if !containsArg(args, "-sn") {
		t.Errorf("strip_all must emit -sn: %q", args)
	}

	// Must NOT emit any subtitle mapping or -c:s copy
	for _, tok := range args {
		if strings.HasPrefix(tok, "0:s") || strings.HasPrefix(tok, "1:s") {
			t.Errorf("strip_all must not map any subtitle streams, found: %s in %q", tok, args)
		}
	}
	if containsArg(args, "-c:s") {
		t.Errorf("strip_all must not emit -c:s copy: %q", args)
	}
}

// TestBuildFfmpegArgsStripAll proves that buildFfmpegArgs generates the exact
// strip_all argv directly without any runner string surgery.
func TestBuildFfmpegArgsStripAll(t *testing.T) {
	job := TranscodeJob{
		InputFile:  "/media/in.mkv",
		OutputFile: "/media/chunk_0000.mkv",
		Codec:      "libx264",
		Spec: jobspec.JobSpec{
			Container:        "mkv",
			CRF:              22,
			PresetSpeed:      "medium",
			SubtitleHandling: "strip_all",
		},
	}

	args, err := buildFfmpegArgs(job, tuningTestStreams)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}

	want := []string{
		"-hide_banner", "-nostdin", "-y", "-fflags", "+genpts", "-probesize", "10000000", "-analyzeduration", "10000000",
		"-protocol_whitelist", "file,pipe",
		"-i", "/media/in.mkv",
		"-map", "0:v?", "-map", "0:a?",
		"-ignore_unknown", "-map_metadata", "0", "-map_chapters", "0",
		"-max_muxing_queue_size", "4096", "-copyts", "-c:a", "copy",
		"-sn",
		"-c:v", "libx264", "-crf", "22", "-preset", "medium",
		"-progress", "pipe:1", "-nostats", "/media/chunk_0000.mkv",
	}

	if !reflect.DeepEqual(args, want) {
		t.Errorf("buildFfmpegArgs argv mismatch:\n got: %q\nwant: %q", args, want)
	}
}

// TestTier2AudioLanguageFilteringAndStripping verifies that audio tracks
// matching user-preferred languages are preserved, while foreign dub tracks
// in unconfigured languages are stripped.
func TestTier2AudioLanguageFilteringAndStripping(t *testing.T) {
	streams := []StreamInfo{
		{Type: "audio", Codec: "truehd", Language: "eng", IsDefault: true},
		{Type: "audio", Codec: "ac3", Language: "fra"},
		{Type: "audio", Codec: "ac3", Language: "spa"},
		{Type: "audio", Codec: "aac", Language: "jpn"},
	}

	builder := NewCommandBuilder("/media/in.mkv", "/media/out.mkv")
	builder.SetAudioHygiene([]string{"eng", "jpn"}, "whitelist")
	builder.SetStreams(streams)
	if err := builder.SetVideoCodec("libx264"); err != nil {
		t.Fatalf("SetVideoCodec: %v", err)
	}

	args, err := builder.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Must map 0:a:0 (eng) and 0:a:3 (jpn), but NOT 0:a:1 (fra) or 0:a:2 (spa)
	if !containsSubsequence(args, []string{"-map", "0:a:0"}) {
		t.Errorf("expected -map 0:a:0 (eng) in %q", args)
	}
	if !containsSubsequence(args, []string{"-map", "0:a:3"}) {
		t.Errorf("expected -map 0:a:3 (jpn) in %q", args)
	}
	if containsSubsequence(args, []string{"-map", "0:a:1"}) {
		t.Errorf("did not expect -map 0:a:1 (fra dub) in %q", args)
	}
	if containsSubsequence(args, []string{"-map", "0:a:2"}) {
		t.Errorf("did not expect -map 0:a:2 (spa dub) in %q", args)
	}
}

// TestTier2ZeroAudioFallbackWhenNoMatch verifies that if no audio tracks match
// user-preferred languages, the default audio track is kept as a zero-audio
// fallback to guarantee non-silent video output.
func TestTier2ZeroAudioFallbackWhenNoMatch(t *testing.T) {
	streams := []StreamInfo{
		{Type: "audio", Codec: "truehd", Language: "deu", IsDefault: true},
		{Type: "audio", Codec: "ac3", Language: "ita"},
	}

	builder := NewCommandBuilder("/media/in.mkv", "/media/out.mkv")
	builder.SetAudioHygiene([]string{"eng", "jpn"}, "whitelist")
	builder.SetStreams(streams)
	if err := builder.SetVideoCodec("libx264"); err != nil {
		t.Fatalf("SetVideoCodec: %v", err)
	}

	args, err := builder.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Must keep 0:a:0 as fallback
	if !containsSubsequence(args, []string{"-map", "0:a:0"}) {
		t.Errorf("expected -map 0:a:0 fallback in %q", args)
	}
	if containsSubsequence(args, []string{"-map", "0:a:1"}) {
		t.Errorf("did not expect 0:a:1 to be mapped in fallback: %q", args)
	}
}

// TestTier2CommentaryAudioPruning verifies that audio tracks identified as
// commentary via stream tags (title, description) or dispositions are pruned.
func TestTier2CommentaryAudioPruning(t *testing.T) {
	streams := []StreamInfo{
		{Type: "audio", Codec: "truehd", Language: "eng", Title: "Main Feature", IsDefault: true},
		{Type: "audio", Codec: "ac3", Language: "eng", Title: "Director's Commentary"},
		{Type: "audio", Codec: "ac3", Language: "eng", IsCommentary: true},
		{Type: "audio", Codec: "ac3", Language: "eng", Description: "Commentary by cast and crew"},
	}

	builder := NewCommandBuilder("/media/in.mkv", "/media/out.mkv")
	builder.SetAudioHygiene([]string{"eng"}, "whitelist")
	builder.SetStreams(streams)
	if err := builder.SetVideoCodec("libx264"); err != nil {
		t.Fatalf("SetVideoCodec: %v", err)
	}

	args, err := builder.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Only 0:a:0 (Main Feature) should be mapped
	if !containsSubsequence(args, []string{"-map", "0:a:0"}) {
		t.Errorf("expected -map 0:a:0 in %q", args)
	}
	for _, commIdx := range []string{"0:a:1", "0:a:2", "0:a:3"} {
		if containsSubsequence(args, []string{"-map", commIdx}) {
			t.Errorf("commentary track %s must be pruned, found in %q", commIdx, args)
		}
	}
}

// TestTier2MP4BitmapSubtitlePruning verifies that bitmap PGS/VobSub subtitles
// are omitted for MP4 containers, while text subtitles and forced subtitles are retained.
func TestTier2MP4BitmapSubtitlePruning(t *testing.T) {
	streams := []StreamInfo{
		{Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "eng", IsDefault: true},
		{Type: "subtitle", Codec: "subrip", Language: "eng"},
		{Type: "subtitle", Codec: "dvd_subtitle", Language: "eng", IsForced: true},
	}

	builder := NewCommandBuilder("/media/in.mkv", "/media/out.mp4")
	builder.SetStreams(streams)
	if err := builder.SetVideoCodec("libx264"); err != nil {
		t.Fatalf("SetVideoCodec: %v", err)
	}

	args, err := builder.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// In MP4, bitmap subs (0:s:0 and 0:s:2) must be omitted; only text sub (0:s:1) mapped
	if !containsSubsequence(args, []string{"-map", "0:s:1"}) {
		t.Errorf("expected text subtitle 0:s:1 mapped in MP4: %q", args)
	}
	if containsSubsequence(args, []string{"-map", "0:s:0"}) {
		t.Errorf("bitmap PGS subtitle 0:s:0 must be omitted for MP4: %q", args)
	}
	if containsSubsequence(args, []string{"-map", "0:s:2"}) {
		t.Errorf("bitmap VobSub subtitle 0:s:2 must be omitted for MP4: %q", args)
	}
}

// TestTier2MKVTextOnlySubtitlePolicyPreservesForced verifies that in MKV containers,
// when text_only subtitle policy is configured, bitmap subtitles are pruned while
// forced subtitles and text subtitles are retained.
func TestTier2MKVTextOnlySubtitlePolicyPreservesForced(t *testing.T) {
	streams := []StreamInfo{
		{Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "eng", IsDefault: true},
		{Type: "subtitle", Codec: "subrip", Language: "eng"},
		{Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "eng", IsForced: true},
	}

	builder := NewCommandBuilder("/media/in.mkv", "/media/out.mkv")
	builder.SetSubtitleHygiene("text_only", "keep_always", nil, "")
	builder.SetStreams(streams)
	if err := builder.SetVideoCodec("libx264"); err != nil {
		t.Fatalf("SetVideoCodec: %v", err)
	}

	args, err := builder.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// 0:s:0 (unforced bitmap PGS) must be pruned
	if containsSubsequence(args, []string{"-map", "0:s:0"}) {
		t.Errorf("unforced bitmap PGS 0:s:0 must be pruned under text_only: %q", args)
	}
	// 0:s:1 (text SRT) must be retained
	if !containsSubsequence(args, []string{"-map", "0:s:1"}) {
		t.Errorf("text subtitle 0:s:1 must be retained: %q", args)
	}
	// 0:s:2 (forced bitmap PGS) must be retained
	if !containsSubsequence(args, []string{"-map", "0:s:2"}) {
		t.Errorf("forced bitmap subtitle 0:s:2 must be retained: %q", args)
	}
}

// TestTier2GlobalMetadataStripping verifies that metadata stripping emits
// -map_metadata -1 and -map_chapters 0, preserving chapter navigation.
func TestTier2GlobalMetadataStripping(t *testing.T) {
	builder := NewCommandBuilder("/media/in.mkv", "/media/out.mkv")
	builder.SetMetadataStripping(true)
	builder.SetStreams(nil)
	if err := builder.SetVideoCodec("libx264"); err != nil {
		t.Fatalf("SetVideoCodec: %v", err)
	}

	args, err := builder.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	wantMetadataSeq := []string{"-map_metadata", "-1", "-map_chapters", "0"}
	if !containsSubsequence(args, wantMetadataSeq) {
		t.Errorf("expected metadata stripping sequence %v in %q", wantMetadataSeq, args)
	}

	// Ensure -map_metadata 0 is NOT emitted
	unwanted := []string{"-map_metadata", "0"}
	if containsSubsequence(args, unwanted) {
		t.Errorf("did not expect -map_metadata 0 when stripping enabled: %q", args)
	}
}

// TestTier4CustomScalarFlagOverrides verifies that custom scalar flags replace
// preset values without duplicating at the tail of the argument list.
func TestTier4CustomScalarFlagOverrides(t *testing.T) {
	builder := NewCommandBuilder("/media/in.mkv", "/media/out.mkv")
	builder.SetStreams(nil)
	if err := builder.SetVideoCodec("libx264"); err != nil {
		t.Fatalf("SetVideoCodec: %v", err)
	}
	builder.SetQuality(24, 0)
	if err := builder.SetPreset("medium"); err != nil {
		t.Fatalf("SetPreset: %v", err)
	}
	if err := builder.SetAudio("copy", "", StreamHandlingCopyAll); err != nil {
		t.Fatalf("SetAudio: %v", err)
	}

	customFlags := "-crf 20 -preset slow -b:v 2M -c:a aac -b:a 192k"
	if err := builder.SetCustomFlags("", customFlags); err != nil {
		t.Fatalf("SetCustomFlags: %v", err)
	}

	args, err := builder.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Preset -crf 24 must be replaced by -crf 20
	if containsSubsequence(args, []string{"-crf", "24"}) {
		t.Errorf("-crf 24 must be replaced: %q", args)
	}
	if !containsSubsequence(args, []string{"-crf", "20"}) {
		t.Errorf("-crf 20 must be present: %q", args)
	}

	// Preset -preset medium must be replaced by -preset slow
	if containsSubsequence(args, []string{"-preset", "medium"}) {
		t.Errorf("-preset medium must be replaced: %q", args)
	}
	if !containsSubsequence(args, []string{"-preset", "slow"}) {
		t.Errorf("-preset slow must be present: %q", args)
	}

	// -b:v 2M must be present
	if !containsSubsequence(args, []string{"-b:v", "2M"}) {
		t.Errorf("-b:v 2M must be present: %q", args)
	}

	// Audio codec must be updated to aac, bitrate 192k
	if !containsSubsequence(args, []string{"-c:a", "aac", "-b:a", "192k"}) {
		t.Errorf("expected audio to be transcoded to aac 192k: %q", args)
	}

	// Telemetry -progress pipe:1 -nostats must appear before output file,
	// and NO scalar flags must duplicate after -nostats.
	nostatsIdx := -1
	for i, arg := range args {
		if arg == "-nostats" {
			nostatsIdx = i
			break
		}
	}
	if nostatsIdx == -1 {
		t.Fatalf("-nostats not found in args: %q", args)
	}
	tail := args[nostatsIdx+1 : len(args)-1]
	for _, f := range []string{"-crf", "-preset", "-b:v", "-c:a", "-b:a"} {
		for _, tok := range tail {
			if tok == f {
				t.Errorf("scalar flag %q must not be duplicated in tail after -nostats: %v", f, tail)
			}
		}
	}
}

// TestTier4CompoundParamsMerging_X265 verifies deep dictionary merging for
// -x265-params, ensuring probed HDR10 metadata and color tags are preserved.
func TestTier4CompoundParamsMerging_X265(t *testing.T) {
	builder := NewCommandBuilder("/media/in.mkv", "/media/out.mkv")
	builder.SetStreams(nil)
	if err := builder.SetVideoCodec("libx265"); err != nil {
		t.Fatalf("SetVideoCodec: %v", err)
	}
	builder.SetQuality(24, 0)
	if err := builder.SetPreset("medium"); err != nil {
		t.Fatalf("SetPreset: %v", err)
	}
	builder.SetColorInfo(&ColorInfo{
		Primaries:  "bt2020",
		Transfer:   "smpte2084",
		Colorspace: "bt2020nc",
	})

	presetX265 := "aq-mode=2:sao=0:hdr10=1:hdr10_opt=1:master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,5):max-cll=1000,400"
	builder.SetX265Params(presetX265)

	customFlags := "-x265-params keyint=60:bframes=4:aq-mode=3"
	if err := builder.SetCustomFlags("", customFlags); err != nil {
		t.Fatalf("SetCustomFlags: %v", err)
	}

	args, err := builder.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Verify color tags are preserved
	if !containsSubsequence(args, []string{"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc"}) {
		t.Errorf("color tags must be preserved: %q", args)
	}

	// Extract -x265-params from args
	var x265Values []string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-x265-params" {
			x265Values = append(x265Values, args[i+1])
		}
	}
	if len(x265Values) != 1 {
		t.Fatalf("expected exactly 1 -x265-params in args, got %d: %q", len(x265Values), args)
	}

	merged := x265Values[0]
	// Protected preset key aq-mode=2 must not be overwritten by custom aq-mode=3
	if !strings.Contains(merged, "aq-mode=2") {
		t.Errorf("expected aq-mode=2 preserved in merged params: %q", merged)
	}
	if strings.Contains(merged, "aq-mode=3") {
		t.Errorf("aq-mode=3 should not overwrite protected aq-mode=2: %q", merged)
	}
	// New custom keys should be present
	if !strings.Contains(merged, "keyint=60") || !strings.Contains(merged, "bframes=4") {
		t.Errorf("expected keyint=60 and bframes=4 in merged params: %q", merged)
	}
	// Existing preset keys must be preserved
	if !strings.Contains(merged, "sao=0") {
		t.Errorf("expected sao=0 preserved in merged params: %q", merged)
	}
	if !strings.Contains(merged, "master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,5)") {
		t.Errorf("expected master-display preserved in merged params: %q", merged)
	}
	if !strings.Contains(merged, "max-cll=1000,400") {
		t.Errorf("expected max-cll preserved in merged params: %q", merged)
	}
	if !strings.Contains(merged, "hdr10=1") || !strings.Contains(merged, "hdr10_opt=1") {
		t.Errorf("expected hdr10 flags preserved in merged params: %q", merged)
	}
}

// TestTier4VendorHardwareFlagEvictionOnCodecOverride verifies that overriding
// -c:v from a hardware encoder to libx265 strips all vendor tuning flags.
func TestTier4VendorHardwareFlagEvictionOnCodecOverride(t *testing.T) {
	builder := NewCommandBuilder("/media/in.mkv", "/media/out.mkv")
	builder.SetStreams(nil)
	if err := builder.SetVideoCodec("hevc_nvenc"); err != nil {
		t.Fatalf("SetVideoCodec: %v", err)
	}
	builder.SetVendorTuningArgs([]string{
		"-tune", "hq", "-multipass", "fullres",
		"-spatial-aq", "1", "-temporal-aq", "1", "-b_ref_mode", "middle",
	})
	builder.SetQuality(24, 0)
	if err := builder.SetPreset("p6"); err != nil {
		t.Fatalf("SetPreset: %v", err)
	}
	builder.SetColorDepth("10bit")

	// Custom flags override video codec to libx265 and crf to 20
	customFlags := "-c:v libx265 -crf 20"
	if err := builder.SetCustomFlags("", customFlags); err != nil {
		t.Fatalf("SetCustomFlags: %v", err)
	}

	args, err := builder.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// -c:v libx265 must replace hevc_nvenc
	if containsSubsequence(args, []string{"-c:v", "hevc_nvenc"}) {
		t.Errorf("-c:v hevc_nvenc must be replaced: %q", args)
	}
	if !containsSubsequence(args, []string{"-c:v", "libx265"}) {
		t.Errorf("-c:v libx265 must be present: %q", args)
	}

	// -crf 20 must be present
	if !containsSubsequence(args, []string{"-crf", "20"}) {
		t.Errorf("-crf 20 must be present: %q", args)
	}

	// All NVENC hardware flags must be completely evicted
	vendorFlags := []string{
		"-spatial-aq",
		"-temporal-aq",
		"-multipass",
		"-b_ref_mode",
		"-rc:v",
		"-cq:v",
	}
	for _, vf := range vendorFlags {
		if containsArg(args, vf) {
			t.Errorf("vendor tuning flag %q must be evicted when overriding to libx265: %q", vf, args)
		}
	}

	// Pixel format for libx265 10-bit should be yuv420p10le, NOT p010le
	if containsSubsequence(args, []string{"-pix_fmt", "p010le"}) {
		t.Errorf("p010le must be replaced with yuv420p10le: %q", args)
	}
	if !containsSubsequence(args, []string{"-pix_fmt", "yuv420p10le"}) {
		t.Errorf("yuv420p10le must be present: %q", args)
	}

	// Preset should be updated from p6 to medium (or software equivalent)
	if containsSubsequence(args, []string{"-preset", "p6"}) {
		t.Errorf("p6 preset must be replaced when overriding to libx265: %q", args)
	}
	if !containsSubsequence(args, []string{"-preset", "medium"}) {
		t.Errorf("medium preset must be present for libx265: %q", args)
	}
}

// TestTier4CompoundParamsMerging_SVTAV1 verifies deep merging for -svtav1-params.
func TestTier4CompoundParamsMerging_SVTAV1(t *testing.T) {
	builder := NewCommandBuilder("/media/in.mkv", "/media/out.mkv")
	builder.SetStreams(nil)
	if err := builder.SetVideoCodec("libsvtav1"); err != nil {
		t.Fatalf("SetVideoCodec: %v", err)
	}
	builder.SetQuality(28, 0)
	if err := builder.SetPreset("6"); err != nil {
		t.Fatalf("SetPreset: %v", err)
	}
	builder.SetSvtAV1Params("tune=0")

	customFlags := "-svtav1-params film-grain=10:tune=1"
	if err := builder.SetCustomFlags("", customFlags); err != nil {
		t.Fatalf("SetCustomFlags: %v", err)
	}

	args, err := builder.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var svtValues []string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-svtav1-params" {
			svtValues = append(svtValues, args[i+1])
		}
	}
	if len(svtValues) != 1 {
		t.Fatalf("expected exactly 1 -svtav1-params in args, got %d: %q", len(svtValues), args)
	}

	merged := svtValues[0]
	if !strings.Contains(merged, "tune=1") {
		t.Errorf("expected tune=1 in merged svtav1 params: %q", merged)
	}
	if !strings.Contains(merged, "film-grain=10") {
		t.Errorf("expected film-grain=10 in merged svtav1 params: %q", merged)
	}
}

// TestTier4CompoundParamsMerging_SVTAV1_HDR10Protected verifies that probed HDR10 metadata
// (master-display, max-cll, hdr10, hdr10_opt, aq-mode) is strictly preserved against custom flag overwrites.
func TestTier4CompoundParamsMerging_SVTAV1_HDR10Protected(t *testing.T) {
	builder := NewCommandBuilder("/media/in.mkv", "/media/out.mkv")
	builder.SetStreams(nil)
	if err := builder.SetVideoCodec("libsvtav1"); err != nil {
		t.Fatalf("SetVideoCodec: %v", err)
	}
	builder.SetQuality(28, 0)
	if err := builder.SetPreset("6"); err != nil {
		t.Fatalf("SetPreset: %v", err)
	}

	probedHDR10 := "aq-mode=2:hdr10=1:hdr10_opt=1:master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,5):max-cll=1000,400:tune=0"
	builder.SetSvtAV1Params(probedHDR10)

	customFlags := "-svtav1-params film-grain=10:tune=1:master-display=bogus:max-cll=0,0:hdr10=0:hdr10_opt=0:aq-mode=1"
	if err := builder.SetCustomFlags("", customFlags); err != nil {
		t.Fatalf("SetCustomFlags: %v", err)
	}

	args, err := builder.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var svtValues []string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-svtav1-params" {
			svtValues = append(svtValues, args[i+1])
		}
	}
	if len(svtValues) != 1 {
		t.Fatalf("expected exactly 1 -svtav1-params in args, got %d: %q", len(svtValues), args)
	}

	merged := svtValues[0]
	// Verify protected HDR10 attributes
	if !strings.Contains(merged, "aq-mode=2") || strings.Contains(merged, "aq-mode=1") {
		t.Errorf("expected aq-mode=2 preserved in svtav1 params: %q", merged)
	}
	if !strings.Contains(merged, "hdr10=1") || strings.Contains(merged, "hdr10=0") {
		t.Errorf("expected hdr10=1 preserved in svtav1 params: %q", merged)
	}
	if !strings.Contains(merged, "hdr10_opt=1") || strings.Contains(merged, "hdr10_opt=0") {
		t.Errorf("expected hdr10_opt=1 preserved in svtav1 params: %q", merged)
	}
	if !strings.Contains(merged, "master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,5)") {
		t.Errorf("expected master-display preserved in svtav1 params: %q", merged)
	}
	if strings.Contains(merged, "master-display=bogus") {
		t.Errorf("master-display=bogus should not overwrite in svtav1 params: %q", merged)
	}
	if !strings.Contains(merged, "max-cll=1000,400") || strings.Contains(merged, "max-cll=0,0") {
		t.Errorf("expected max-cll=1000,400 preserved in svtav1 params: %q", merged)
	}

	// Verify un-protected custom parameters merged
	if !strings.Contains(merged, "tune=1") {
		t.Errorf("expected tune=1 updated in svtav1 params: %q", merged)
	}
	if !strings.Contains(merged, "film-grain=10") {
		t.Errorf("expected film-grain=10 appended in svtav1 params: %q", merged)
	}
}

// TestTier4SecurityRejections verifies that blacklisted and Tier 1 arguments fail validation.
func TestTier4SecurityRejections(t *testing.T) {
	forbiddenFlags := []string{
		"-nostdin",
		"-progress pipe:1",
		"-i /dev/null",
		"-map 0:v",
		"-map_metadata 0",
		"-hide_banner",
		"-y",
		"-fflags +genpts",
		"-protocol_whitelist file,pipe",
		"-max_muxing_queue_size 4096",
		"-ignore_unknown",
		"-nostats",
	}

	for _, flags := range forbiddenFlags {
		t.Run(flags, func(t *testing.T) {
			builder := NewCommandBuilder("/media/in.mkv", "/media/out.mkv")
			if err := builder.SetVideoCodec("libx264"); err != nil {
				t.Fatalf("SetVideoCodec: %v", err)
			}
			err := builder.SetCustomFlags("", flags)
			if err == nil {
				t.Fatalf("expected error for forbidden flag %q, got nil", flags)
			}
		})
	}
}

// TestTier4NewCommandBuilderFromJob_EndToEnd verifies structured merging and eviction
// when building args from a TranscodeJob.
func TestTier4NewCommandBuilderFromJob_EndToEnd(t *testing.T) {
	colorInfo := parseColorInfo([]byte(sampleColorProbeJSON))
	if colorInfo == nil {
		t.Fatal("failed to parse sampleColorProbeJSON")
	}

	job := TranscodeJob{
		InputFile:  "/media/in.mkv",
		OutputFile: "/media/out.mkv",
		Codec:      "hevc_nvenc",
		ColorInfo:  colorInfo,
		Spec: jobspec.JobSpec{
			Container:     "mkv",
			QualityTarget: 22,
			PresetSpeed:   "p6",
			ColorDepth:    "10bit",
			CustomFlags:   "-c:v libx265 -crf 19 -preset slow -x265-params keyint=60:bframes=4",
		},
	}

	args, err := buildFfmpegArgs(job, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}

	// -c:v libx265 must replace hevc_nvenc
	if !containsSubsequence(args, []string{"-c:v", "libx265"}) {
		t.Errorf("expected -c:v libx265: %q", args)
	}
	if containsSubsequence(args, []string{"-c:v", "hevc_nvenc"}) {
		t.Errorf("-c:v hevc_nvenc must be replaced: %q", args)
	}

	// -crf 19 and -preset slow must be present
	if !containsSubsequence(args, []string{"-crf", "19"}) {
		t.Errorf("expected -crf 19: %q", args)
	}
	if !containsSubsequence(args, []string{"-preset", "slow"}) {
		t.Errorf("expected -preset slow: %q", args)
	}

	// NVENC flags must be evicted
	for _, f := range []string{"-spatial-aq", "-temporal-aq", "-multipass", "-b_ref_mode", "-rc:v", "-cq:v"} {
		if containsArg(args, f) {
			t.Errorf("expected NVENC flag %q to be evicted: %q", f, args)
		}
	}

	// Color tags must be preserved
	if !containsSubsequence(args, []string{"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc"}) {
		t.Errorf("color tags must be preserved: %q", args)
	}

	// Exactly one -x265-params with merged custom keys and preserved HDR10 metadata
	var x265Vals []string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-x265-params" {
			x265Vals = append(x265Vals, args[i+1])
		}
	}
	if len(x265Vals) != 1 {
		t.Fatalf("expected exactly 1 -x265-params, got %d: %q", len(x265Vals), args)
	}
	val := x265Vals[0]
	if !strings.Contains(val, "keyint=60") || !strings.Contains(val, "bframes=4") {
		t.Errorf("expected custom keys in -x265-params: %q", val)
	}
	if !strings.Contains(val, "master-display=") || !strings.Contains(val, "max-cll=1000,400") || !strings.Contains(val, "hdr10=1") {
		t.Errorf("expected preserved HDR10 metadata in -x265-params: %q", val)
	}
}

// TestTier4LegacyJob_EndToEnd verifies legacy x265 custom flags deep merging.
func TestTier4LegacyJob_EndToEnd(t *testing.T) {
	job := TranscodeJob{
		InputFile:  "/media/in.mkv",
		OutputFile: "/media/out.mkv",
		Codec:      "libx265",
		Spec: jobspec.JobSpec{
			Container:     "mkv",
			QualityTarget: 22,
			PresetSpeed:   "medium",
			AqMode:        2,
			FilmGrainSao:  false, // sao=0
			CustomFlags:   "keyint=60:scenecut=40",
		},
	}

	args, err := buildFfmpegArgs(job, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}

	var x265Vals []string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-x265-params" {
			x265Vals = append(x265Vals, args[i+1])
		}
	}
	if len(x265Vals) != 1 {
		t.Fatalf("expected exactly 1 -x265-params, got %d: %q", len(x265Vals), args)
	}
	val := x265Vals[0]
	if !strings.Contains(val, "aq-mode=2") || !strings.Contains(val, "sao=0") {
		t.Errorf("expected preset options in -x265-params: %q", val)
	}
	if !strings.Contains(val, "keyint=60") || !strings.Contains(val, "scenecut=40") {
		t.Errorf("expected legacy custom keys in -x265-params: %q", val)
	}
}

// TestCommandBuilderVideoOnlyChunkEncoding verifies the acceptance argv for
// video-only chunk encoding: when audio and subtitle handling are both
// strip_all, the encode collapses to pure video — a single 0:v:0 map,
// -an -sn -dn, no audio/subtitle codec flags, and -fps_mode passthrough so
// VFR sources keep their source frame timing — regardless of whether the
// stream probe succeeded.
func TestCommandBuilderVideoOnlyChunkEncoding(t *testing.T) {
	for _, streams := range [][]StreamInfo{tuningTestStreams, nil} {
		job := TranscodeJob{
			InputFile:  "/media/in.mkv",
			OutputFile: "/media/out.segments/chunk-000.mkv",
			Codec:      "libx264",
			Spec: jobspec.JobSpec{
				Container:        "mkv",
				CRF:              22,
				PresetSpeed:      "medium",
				AudioHandling:    "strip_all",
				SubtitleHandling: "strip_all",
			},
		}

		args, err := buildFfmpegArgs(job, streams)
		if err != nil {
			t.Fatalf("buildFfmpegArgs: %v", err)
		}

		want := []string{
			"-hide_banner", "-nostdin", "-y", "-fflags", "+genpts", "-probesize", "10000000", "-analyzeduration", "10000000",
			"-protocol_whitelist", "file,pipe",
			"-i", "/media/in.mkv",
			"-map", "0:v:0",
			"-ignore_unknown", "-map_metadata", "0", "-map_chapters", "0",
			"-max_muxing_queue_size", "4096", "-an", "-sn", "-dn",
			"-c:v", "libx264", "-fps_mode", "passthrough", "-crf", "22", "-preset", "medium",
			"-progress", "pipe:1", "-nostats", "/media/out.segments/chunk-000.mkv",
		}
		if !reflect.DeepEqual(args, want) {
			t.Errorf("video-only argv mismatch (streams=%v):\n got: %q\nwant: %q", streams != nil, args, want)
		}
	}
}


