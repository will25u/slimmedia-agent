package ffmpeg

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/jobspec"
)

func TestBuildX265Params(t *testing.T) {
	tests := []struct {
		name          string
		aqMode        int
		filmGrainSao  bool
		contentTuning string
		customFlags   string
		want          string
	}{
		{"all fields, custom flags last", 2, false, "film", "keyint=60", "aq-mode=2:sao=0:tune=film:keyint=60"},
		{"sao kept when smoothing on", 3, true, "grain", "", "aq-mode=3:tune=grain"},
		{"animation tune", 1, true, "animation", "deblock=-1,-1", "aq-mode=1:tune=animation:deblock=-1,-1"},
		{"none tuning omitted", 4, true, "none", "", "aq-mode=4"},
		{"no aq mode", 0, true, "film", "", "tune=film"},
		{"sao off only", 0, false, "", "", "sao=0"},
		{"nothing set", 0, true, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildX265Params(tt.aqMode, tt.filmGrainSao, tt.contentTuning, tt.customFlags); got != tt.want {
				t.Errorf("buildX265Params() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSelectQualityValue(t *testing.T) {
	tests := []struct {
		name          string
		encoder       string
		crf           int
		qualityTarget int
		want          int
	}{
		{"crf wins over quality_target", "libx265", 20, 22, 20},
		{"quality_target fallback", "libx265", 0, 22, 22},
		{"out-of-range crf falls back", "libx265", 60, 20, 20},
		{"out-of-range quality_target dropped", "libx265", 0, 30, 0},
		{"negative crf falls back", "libx265", -5, 24, 24},
		{"neither set", "libx265", 0, 0, 0},
		// NVENC/QSV accept the full CQ/ICQ 0–51 band via quality_target (#195).
		{"nvenc quality_target full range", "hevc_nvenc", 0, 30, 30},
		{"qsv quality_target full range", "hevc_qsv", 0, 51, 51},
		{"nvenc crf precedence unchanged", "hevc_nvenc", 24, 40, 24},
		{"nvenc out-of-range quality_target dropped", "hevc_nvenc", 0, 52, 0},
		{"qsv unset quality_target means no flag", "hevc_qsv", 0, 0, 0},
		// Other families (amf, videotoolbox) keep the 18–26 band.
		{"amf keeps conservative band", "hevc_amf", 0, 30, 0},
		// libsvtav1 accepts crf up to 63
		{"svtav1 crf accepts up to 63", "libsvtav1", 60, 22, 60},
		{"svtav1 crf over 63 falls back", "libsvtav1", 64, 22, 22},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := selectQualityValue(tt.encoder, tt.crf, tt.qualityTarget); got != tt.want {
				t.Errorf("selectQualityValue(%q, %d, %d) = %d, want %d", tt.encoder, tt.crf, tt.qualityTarget, got, tt.want)
			}
		})
	}
}

func TestPixFmtFor(t *testing.T) {
	tests := []struct {
		encoder    string
		colorDepth string
		want       string
	}{
		{"libx265", "10bit", "yuv420p10le"},
		{"hevc_nvenc", "10bit", "p010le"},
		{"hevc_qsv", "10bit", "p010le"},
		{"h264_nvenc", "10bit", ""}, // degrade to 8-bit, never fail
		{"h264_qsv", "10bit", ""},
		{"libx264", "10bit", ""},
		{"libx265", "8bit", ""},
		{"libx265", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.encoder+"/"+tt.colorDepth, func(t *testing.T) {
			if got := pixFmtFor(tt.encoder, tt.colorDepth); got != tt.want {
				t.Errorf("pixFmtFor(%q, %q) = %q, want %q", tt.encoder, tt.colorDepth, got, tt.want)
			}
		})
	}
}

// tuningTestStreams: a0=eng default TrueHD, a1=jpn AC3, a2=eng commentary AC3;
// s0=default PGS (bitmap), s1=SRT (text).
var tuningTestStreams = []StreamInfo{
	{Type: "audio", Codec: "truehd", Language: "eng", IsDefault: true},
	{Type: "audio", Codec: "ac3", Language: "jpn"},
	{Type: "audio", Codec: "ac3", Language: "eng"},
	{Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "eng", IsDefault: true},
	{Type: "subtitle", Codec: "subrip", Language: "eng"},
}

func TestBuildStreamArgs(t *testing.T) {
	tests := []struct {
		name             string
		container        string
		outputFile       string
		streams          []StreamInfo
		audioHandling    StreamHandling
		subtitleHandling StreamHandling
		want             []string
	}{
		{
			name:             "copy_all mkv blanket map",
			container:        "mkv",
			streams:          tuningTestStreams,
			audioHandling:    "copy_all",
			subtitleHandling: "copy_all",
			want:             []string{"-map", "0", "-c:a", "copy", "-c:s", "copy"},
		},
		{
			name:             "copy_all mp4 drops bitmap subtitles",
			container:        "mp4",
			streams:          tuningTestStreams,
			audioHandling:    "copy_all",
			subtitleHandling: "copy_all",
			want: []string{
				"-map", "0:v:0", "-map", "0:a:0", "-map", "0:a:1", "-map", "0:a:2",
				"-c:a", "copy", "-map", "0:s:1", "-c:s", "copy",
			},
		},
		{
			name:             "copy_all mp4 detected from output suffix",
			outputFile:       "/media/out.mp4",
			streams:          tuningTestStreams,
			audioHandling:    "copy_all",
			subtitleHandling: "copy_all",
			want: []string{
				"-map", "0:v:0", "-map", "0:a:0", "-map", "0:a:1", "-map", "0:a:2",
				"-c:a", "copy", "-map", "0:s:1", "-c:s", "copy",
			},
		},
		{
			name:             "strip_non_preferred keeps default and first-per-language audio plus default subtitle",
			container:        "mkv",
			streams:          tuningTestStreams,
			audioHandling:    "strip_non_preferred",
			subtitleHandling: "strip_non_preferred",
			want: []string{
				"-map", "0:v:0", "-map", "0:a:0", "-map", "0:a:1",
				"-c:a", "copy", "-map", "0:s:0", "-c:s", "copy",
			},
		},
		{
			name:             "strip_non_preferred mp4 drops default bitmap subtitle",
			container:        "mp4",
			streams:          tuningTestStreams,
			audioHandling:    "strip_non_preferred",
			subtitleHandling: "strip_non_preferred",
			want: []string{
				"-map", "0:v:0", "-map", "0:a:0", "-map", "0:a:1",
				"-c:a", "copy", "-c:s", "copy",
			},
		},
		{
			name:             "copy_primary first audio only no subtitles",
			container:        "mkv",
			streams:          tuningTestStreams,
			audioHandling:    "copy_primary",
			subtitleHandling: "copy_primary",
			want:             []string{"-map", "0:v:0", "-map", "0:a:0", "-c:a", "copy"},
		},
		{
			name:             "transcode_aac all audio strip subtitles",
			container:        "mkv",
			streams:          tuningTestStreams,
			audioHandling:    "transcode_aac",
			subtitleHandling: "strip_all",
			want: []string{
				"-map", "0:v:0", "-map", "0:a:0", "-map", "0:a:1", "-map", "0:a:2",
				"-c:a", "aac", "-b:a", "192k", "-sn",
			},
		},
		{
			name:             "strip_all",
			container:        "mkv",
			streams:          tuningTestStreams,
			audioHandling:    "strip_all",
			subtitleHandling: "strip_all",
			want:             []string{"-map", "0:v:0", "-an", "-sn"},
		},
		{
			name:             "probe failed degrades safely",
			container:        "mp4",
			streams:          nil,
			audioHandling:    "copy_all",
			subtitleHandling: "copy_all",
			want:             []string{"-map", "0:v:0", "-map", "0:a", "-c:a", "copy", "-sn"},
		},
		{
			name:             "probe failed non-mp4 keeps blanket map",
			container:        "mkv",
			streams:          nil,
			audioHandling:    "copy_all",
			subtitleHandling: "copy_all",
			want:             []string{"-map", "0", "-c:a", "copy", "-c:s", "copy"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildStreamArgs(tt.container, tt.outputFile, tt.streams, tt.audioHandling, tt.subtitleHandling)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("BuildStreamArgs() =\n%q\nwant:\n%q", got, tt.want)
			}
		})
	}
}

func TestBuildConcatArgs(t *testing.T) {
	concatTxt := "/media/out.segments/concat.txt"
	inputMkv := "/media/in.mkv"
	demuxPath := "/media/out.segments/demux.mkv"
	chapterPath := "/media/out.segments/chapters.ffmeta"
	demuxSidecar := AssemblySidecar{DemuxPath: demuxPath}
	pgsOnlyStreams := []StreamInfo{
		{Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "eng", IsDefault: true},
	}
	srtOnlyStreams := []StreamInfo{
		{Type: "subtitle", Codec: "subrip", Language: "eng", IsDefault: true},
	}

	tests := []struct {
		name       string
		job        TranscodeJob
		concatFile string
		sidecar    AssemblySidecar
		streams    []StreamInfo
		want       []string
	}{
		{
			name: "copy_all mkv keeps all subtitles",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:        "mkv",
					SubtitleHandling: "copy_all",
				},
				OutputFile: "/media/out.mkv",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams:    tuningTestStreams,
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:a?", "-map", "1:s?", "-c", "copy", "-map_chapters", "1", "/media/out.mkv",
			},
		},
		{
			name: "copy_all mp4 drops bitmap subtitle (PGS), maps text subtitle (SRT)",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:        "mp4",
					SubtitleHandling: "copy_all",
				},
				OutputFile: "/media/out.mp4",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams:    tuningTestStreams,
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:a?", "-map", "1:s:1", "-c", "copy", "-map_chapters", "1", "-movflags", "+faststart", "/media/out.mp4",
			},
		},
		{
			name: "copy_all mp4 detected from output filename drops bitmap subtitles",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					SubtitleHandling: "copy_all",
				},
				OutputFile: "/media/out.mp4",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams:    tuningTestStreams,
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:a?", "-map", "1:s:1", "-c", "copy", "-map_chapters", "1", "-movflags", "+faststart", "/media/out.mp4",
			},
		},
		{
			name: "copy_all mp4 with only bitmap subtitles drops all subtitles",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:        "mp4",
					SubtitleHandling: "copy_all",
				},
				OutputFile: "/media/out.mp4",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams:    pgsOnlyStreams,
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-c", "copy", "-map_chapters", "1", "-movflags", "+faststart", "/media/out.mp4",
			},
		},
		{
			name: "strip_all mkv drops subtitles",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:        "mkv",
					SubtitleHandling: "strip_all",
				},
				OutputFile: "/media/out.mkv",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams:    tuningTestStreams,
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:a?", "-c", "copy", "-map_chapters", "1", "/media/out.mkv",
			},
		},
		{
			name: "strip_all mp4 drops subtitles",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:        "mp4",
					SubtitleHandling: "strip_all",
				},
				OutputFile: "/media/out.mp4",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams:    tuningTestStreams,
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:a?", "-c", "copy", "-map_chapters", "1", "-movflags", "+faststart", "/media/out.mp4",
			},
		},
		{
			name: "strip_non_preferred mkv maps default PGS subtitle",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:        "mkv",
					SubtitleHandling: "strip_non_preferred",
				},
				OutputFile: "/media/out.mkv",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams:    tuningTestStreams,
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:a?", "-map", "1:s:0", "-c", "copy", "-map_chapters", "1", "/media/out.mkv",
			},
		},
		{
			name: "strip_non_preferred mp4 drops default bitmap subtitle",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:        "mp4",
					SubtitleHandling: "strip_non_preferred",
				},
				OutputFile: "/media/out.mp4",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams:    tuningTestStreams,
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:a?", "-c", "copy", "-map_chapters", "1", "-movflags", "+faststart", "/media/out.mp4",
			},
		},
		{
			name: "copy_primary mkv maps primary subtitle",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:        "mkv",
					SubtitleHandling: "copy_primary",
				},
				OutputFile: "/media/out.mkv",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams:    tuningTestStreams,
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:a?", "-map", "1:s:0", "-c", "copy", "-map_chapters", "1", "/media/out.mkv",
			},
		},
		{
			name: "copy_primary mp4 drops primary bitmap subtitle",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:        "mp4",
					SubtitleHandling: "copy_primary",
				},
				OutputFile: "/media/out.mp4",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams:    tuningTestStreams,
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:a?", "-c", "copy", "-map_chapters", "1", "-movflags", "+faststart", "/media/out.mp4",
			},
		},
		{
			name: "copy_primary mp4 maps primary text subtitle",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:        "mp4",
					SubtitleHandling: "copy_primary",
				},
				OutputFile: "/media/out.mp4",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams:    srtOnlyStreams,
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:s:0", "-c", "copy", "-map_chapters", "1", "-movflags", "+faststart", "/media/out.mp4",
			},
		},
		{
			name: "probe failed mp4 drops subtitles safely",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:        "mp4",
					SubtitleHandling: "copy_all",
				},
				OutputFile: "/media/out.mp4",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams:    nil,
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:a?", "-c", "copy", "-map_chapters", "1", "-movflags", "+faststart", "/media/out.mp4",
			},
		},
		{
			name: "probe failed non-mp4 keeps blanket map",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:        "mkv",
					SubtitleHandling: "copy_all",
				},
				OutputFile: "/media/out.mkv",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams:    nil,
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:a?", "-map", "1:s?", "-c", "copy", "-map_chapters", "1", "/media/out.mkv",
			},
		},
		{
			name: "strip_all format policy drops subtitles",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:            "mkv",
					SubtitleHandling:     "copy_all",
					SubtitleFormatPolicy: "strip_all",
				},
				OutputFile: "/media/out.mkv",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams:    tuningTestStreams,
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:a?", "-c", "copy", "-map_chapters", "1", "/media/out.mkv",
			},
		},
		{
			name: "strip_metadata preserves chapter markers from input 1",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:        "mkv",
					SubtitleHandling: "strip_all",
					StripMetadata:    true,
				},
				OutputFile: "/media/out.mkv",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams:    tuningTestStreams,
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:a?", "-c", "copy",
				"-map_metadata", "-1", "-map_chapters", "1",
				"/media/out.mkv",
			},
		},
		{
			name: "strip_metadata and mp4 preserves chapters and faststart",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:        "mp4",
					SubtitleHandling: "strip_all",
					StripMetadata:    true,
				},
				OutputFile: "/media/out.mp4",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams:    tuningTestStreams,
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:a?", "-c", "copy",
				"-map_metadata", "-1", "-map_chapters", "1",
				"-movflags", "+faststart",
				"/media/out.mp4",
			},
		},
		{
			name: "text_only policy on mkv prunes bitmap subtitle and keeps text subtitle",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:            "mkv",
					SubtitleHandling:     "copy_all",
					SubtitleFormatPolicy: "text_only",
				},
				OutputFile: "/media/out.mkv",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams:    tuningTestStreams,
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:a?", "-map", "1:s:1", "-c", "copy", "-map_chapters", "1", "/media/out.mkv",
			},
		},
		{
			name: "text_only policy on mkv retains forced bitmap subtitle",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:            "mkv",
					SubtitleHandling:     "copy_all",
					SubtitleFormatPolicy: "text_only",
				},
				OutputFile: "/media/out.mkv",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams: []StreamInfo{
				{Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "eng", IsForced: true},
				{Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "fra", IsForced: false},
			},
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:s:0", "-c", "copy", "-map_chapters", "1", "/media/out.mkv",
			},
		},
		{
			name: "whitelist mode filters non-preferred subtitle languages",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:                  "mkv",
					SubtitleHandling:           "copy_all",
					SubtitleLanguageMode:       "whitelist",
					PreferredSubtitleLanguages: []string{"eng"},
				},
				OutputFile: "/media/out.mkv",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams: []StreamInfo{
				{Type: "subtitle", Codec: "subrip", Language: "jpn"},
				{Type: "subtitle", Codec: "subrip", Language: "eng"},
			},
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:s:1", "-c", "copy", "-map_chapters", "1", "/media/out.mkv",
			},
		},
		{
			name: "copy_forced_only maps only forced subtitles",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:        "mkv",
					SubtitleHandling: "copy_forced_only",
				},
				OutputFile: "/media/out.mkv",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams: []StreamInfo{
				{Type: "subtitle", Codec: "subrip", Language: "eng", IsForced: false},
				{Type: "subtitle", Codec: "subrip", Language: "eng", IsForced: true},
			},
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:s:1", "-c", "copy", "-map_chapters", "1", "/media/out.mkv",
			},
		},
		{
			name: "audio strip_all maps no audio",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:        "mkv",
					AudioHandling:    "strip_all",
					SubtitleHandling: "copy_all",
				},
				OutputFile: "/media/out.mkv",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams:    tuningTestStreams,
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:s?", "-c", "copy", "-map_chapters", "1", "/media/out.mkv",
			},
		},
		{
			name: "audio copy_primary maps first audio track",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:        "mkv",
					AudioHandling:    "copy_primary",
					SubtitleHandling: "copy_all",
				},
				OutputFile: "/media/out.mkv",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams:    tuningTestStreams,
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:a:0", "-map", "1:s?", "-c", "copy", "-map_chapters", "1", "/media/out.mkv",
			},
		},
		{
			name: "audio strip_non_preferred maps default then first of each language",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:        "mkv",
					AudioHandling:    "strip_non_preferred",
					SubtitleHandling: "copy_all",
				},
				OutputFile: "/media/out.mkv",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams:    tuningTestStreams,
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:a:0", "-map", "1:a:1", "-map", "1:s?", "-c", "copy", "-map_chapters", "1", "/media/out.mkv",
			},
		},
		{
			name: "audio transcode_aac re-encodes after blanket copy",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:        "mkv",
					AudioHandling:    "transcode_aac",
					SubtitleHandling: "copy_all",
				},
				OutputFile: "/media/out.mkv",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams:    tuningTestStreams,
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:a?", "-map", "1:s?", "-c", "copy",
				"-c:a", "aac", "-b:a", "192k", "-map_chapters", "1", "/media/out.mkv",
			},
		},
		{
			name: "audio probe failed degrades to blanket audio map",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:        "mkv",
					AudioHandling:    "strip_non_preferred",
					SubtitleHandling: "copy_all",
				},
				OutputFile: "/media/out.mkv",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams:    nil,
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:a?", "-map", "1:s?", "-c", "copy", "-map_chapters", "1", "/media/out.mkv",
			},
		},
		{
			name: "audio copy_all prunes commentary track",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:        "mkv",
					AudioHandling:    "copy_all",
					SubtitleHandling: "copy_all",
				},
				OutputFile: "/media/out.mkv",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams: []StreamInfo{
				{Type: "audio", Codec: "truehd", Language: "eng", IsDefault: true},
				{Type: "audio", Codec: "ac3", Language: "eng", Title: "Director's Commentary", IsCommentary: true},
				{Type: "subtitle", Codec: "subrip", Language: "eng"},
			},
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:a:0", "-map", "1:s?", "-c", "copy", "-map_chapters", "1", "/media/out.mkv",
			},
		},
		{
			name: "audio copy_all language whitelist keeps only matching tracks",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:              "mkv",
					AudioHandling:          "copy_all",
					SubtitleHandling:       "copy_all",
					PreferredAudioLanguages: []string{"eng"},
					AudioLanguageMode:       "whitelist",
				},
				OutputFile: "/media/out.mkv",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams: []StreamInfo{
				{Type: "audio", Codec: "truehd", Language: "eng", IsDefault: true},
				{Type: "audio", Codec: "ac3", Language: "jpn"},
				{Type: "subtitle", Codec: "subrip", Language: "eng"},
			},
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:a:0", "-map", "1:s?", "-c", "copy", "-map_chapters", "1", "/media/out.mkv",
			},
		},
		{
			name: "audio whitelist with no match falls back to default track",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:              "mkv",
					AudioHandling:          "copy_all",
					SubtitleHandling:       "copy_all",
					PreferredAudioLanguages: []string{"deu"},
					AudioLanguageMode:       "whitelist",
				},
				OutputFile: "/media/out.mkv",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams: []StreamInfo{
				{Type: "audio", Codec: "ac3", Language: "jpn"},
				{Type: "audio", Codec: "truehd", Language: "eng", IsDefault: true},
				{Type: "subtitle", Codec: "subrip", Language: "eng"},
			},
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:a:1", "-map", "1:s?", "-c", "copy", "-map_chapters", "1", "/media/out.mkv",
			},
		},
		{
			name: "audio copy_primary skips commentary to first real track",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:        "mkv",
					AudioHandling:    "copy_primary",
					SubtitleHandling: "copy_all",
				},
				OutputFile: "/media/out.mkv",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    demuxSidecar,
			streams: []StreamInfo{
				{Type: "audio", Codec: "ac3", Language: "eng", Title: "Commentary", IsCommentary: true},
				{Type: "audio", Codec: "truehd", Language: "eng", IsDefault: true},
				{Type: "subtitle", Codec: "subrip", Language: "eng"},
			},
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-copyts", "-i", demuxPath,
				"-map", "0", "-map", "1:a:1", "-map", "1:s?", "-c", "copy", "-map_chapters", "1", "/media/out.mkv",
			},
		},
		{
			name: "chapter sidecar only carries chapters, no stream maps",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:        "mkv",
					SubtitleHandling: "copy_all",
				},
				OutputFile: "/media/out.mkv",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    AssemblySidecar{ChapterPath: chapterPath},
			streams:    tuningTestStreams,
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt, "-i", chapterPath,
				"-map", "0", "-c", "copy", "-map_chapters", "1", "/media/out.mkv",
			},
		},
		{
			name: "no sidecar emits no second input and no chapter flag",
			job: TranscodeJob{
				Spec: jobspec.JobSpec{
					Container:        "mkv",
					SubtitleHandling: "copy_all",
				},
				OutputFile: "/media/out.mkv",
				InputFile:  inputMkv,
			},
			concatFile: concatTxt,
			sidecar:    AssemblySidecar{},
			streams:    tuningTestStreams,
			want: []string{
				"-hide_banner", "-y", "-f", "concat", "-safe", "0",
				"-protocol_whitelist", "file,pipe",
				"-i", concatTxt,
				"-map", "0", "-c", "copy", "/media/out.mkv",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildConcatArgs(tt.job, tt.concatFile, tt.sidecar, tt.streams)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("BuildConcatArgs() =\n%q\nwant:\n%q", got, tt.want)
			}
			for _, arg := range got {
				if arg == tt.job.InputFile {
					t.Errorf("remote source path must not appear in assembly argv: %q", got)
				}
			}
		})
	}
}

func TestQualityArgsFor(t *testing.T) {
	tests := []struct {
		encoder string
		v       int
		want    []string
	}{
		{"hevc_nvenc", 22, []string{"-rc:v", "vbr", "-cq:v", "22", "-b:v", "0"}},
		{"h264_nvenc", 23, []string{"-rc:v", "vbr", "-cq:v", "23", "-b:v", "0"}},
		{"av1_nvenc", 30, []string{"-rc:v", "vbr", "-cq:v", "30", "-b:v", "0"}},
		{"hevc_qsv", 25, []string{"-global_quality", "25"}},
		{"h264_qsv", 23, []string{"-global_quality", "23"}},
		{"av1_qsv", 28, []string{"-global_quality", "28"}},
		{"hevc_amf", 24, []string{"-rc", "cqp", "-qp_i", "24", "-qp_p", "26"}},
		{"h264_amf", 23, []string{"-rc", "cqp", "-qp_i", "23", "-qp_p", "25"}},
		{"hevc_videotoolbox", 18, []string{"-q:v", "70"}},
		{"hevc_videotoolbox", 22, []string{"-q:v", "60"}},
		{"hevc_videotoolbox", 26, []string{"-q:v", "50"}},
		{"h264_videotoolbox", 22, []string{"-q:v", "60"}},
		{"libx265", 28, []string{"-crf", "28"}},
		{"libx264", 23, []string{"-crf", "23"}},
		{"hevc_nvenc", 0, nil},
		{"hevc_videotoolbox", 0, nil},
	}
	for _, tt := range tests {
		t.Run(tt.encoder, func(t *testing.T) {
			if got := qualityArgsFor(tt.encoder, tt.v); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("qualityArgsFor(%q, %d) = %q, want %q", tt.encoder, tt.v, got, tt.want)
			}
		})
	}
}

// TestBuildFfmpegArgsHWTuningGate: advanced tuning flags are emitted only
// when the canary verified them (Caps present) or Caps is nil (legacy/direct
// use). Quality args are always emitted.
func TestBuildFfmpegArgsHWTuningGate(t *testing.T) {
	qsvTuning := []string{"-extbrc", "1", "-look_ahead", "1", "-look_ahead_depth", "40", "-b_strategy", "1", "-adaptive_b", "1", "-adaptive_i", "1"}
	qsvShortTuning := []string{"-extbrc", "1", "-b_strategy", "1"}
	nvencTuning := []string{"-tune", "hq", "-multipass", "fullres", "-spatial-aq", "1", "-temporal-aq", "1", "-b_ref_mode", "middle"}

	base := TranscodeJob{
		InputFile:  "/media/in.mkv",
		OutputFile: "/media/out.mkv",
		Codec:      "hevc",
		Spec: jobspec.JobSpec{
			Container:     "mkv",
			EncoderEngine: "qsv",
			QualityTarget: 22,
		},
	}

	// Caps present, tuning unverified: no tuning flags, quality args stay.
	unverified := base
	unverified.Caps = &EncoderCaps{HasIntel: true, HasQSVHEVC: true, HasQSVHEVCVerified: true}
	args, err := buildFfmpegArgs(unverified, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	if containsArg(args, "-extbrc") {
		t.Errorf("unverified caps must not emit QSV tuning flags: %q", args)
	}
	if !containsArg(args, "-global_quality") {
		t.Errorf("quality args must always be emitted: %q", args)
	}

	// Caps present, tuning verified: tuning flags present.
	verified := base
	verified.Caps = &EncoderCaps{HasQSVHEVC: true, HasQSVHEVCTuningVerified: true}
	args, err = buildFfmpegArgs(verified, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	if !containsSubsequence(args, qsvTuning) {
		t.Errorf("verified caps must emit QSV tuning flags: %q", args)
	}

	// Nil Caps (legacy/direct use): tuning flags present.
	direct := TranscodeJob{
		InputFile:  "/media/in.mkv",
		OutputFile: "/media/out.mkv",
		Codec:      "hevc_qsv",
		Spec: jobspec.JobSpec{
			Container:     "mkv",
			QualityTarget: 22,
		},
	}
	args, err = buildFfmpegArgs(direct, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	if !containsSubsequence(args, qsvTuning) {
		t.Errorf("nil Caps must emit QSV tuning flags (legacy behavior): %q", args)
	}

	// QSV H.264: verified emits -extbrc 1 and -b_strategy 1
	qsvH264Job := TranscodeJob{
		InputFile:  "/media/in.mkv",
		OutputFile: "/media/out.mkv",
		Codec:      "h264_qsv",
		Spec: jobspec.JobSpec{
			Container:     "mkv",
			QualityTarget: 22,
		},
		Caps: &EncoderCaps{HasQSVH264: true, HasQSVH264TuningVerified: true},
	}
	args, err = buildFfmpegArgs(qsvH264Job, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	if !containsSubsequence(args, qsvShortTuning) {
		t.Errorf("verified QSV H264 must emit -extbrc 1 and -b_strategy 1: %q", args)
	}

	// QSV H.264: unverified degrades gracefully
	qsvH264Job.Caps = &EncoderCaps{HasQSVH264: true, HasQSVH264TuningVerified: false}
	args, err = buildFfmpegArgs(qsvH264Job, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	if containsArg(args, "-extbrc") {
		t.Errorf("unverified QSV H264 must not emit -extbrc: %q", args)
	}
	if !containsArg(args, "-global_quality") {
		t.Errorf("unverified QSV H264 must retain quality flags: %q", args)
	}

	// QSV AV1: verified emits -extbrc 1 and -b_strategy 1
	qsvAV1Job := TranscodeJob{
		InputFile:  "/media/in.mkv",
		OutputFile: "/media/out.mkv",
		Codec:      "av1_qsv",
		Spec: jobspec.JobSpec{
			Container:     "mkv",
			QualityTarget: 22,
		},
		Caps: &EncoderCaps{HasQSVAV1: true, HasQSVAV1Verified: true, HasQSVAV1TuningVerified: true},
	}
	args, err = buildFfmpegArgs(qsvAV1Job, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	if !containsSubsequence(args, qsvShortTuning) {
		t.Errorf("verified QSV AV1 must emit -extbrc 1 and -b_strategy 1: %q", args)
	}

	// QSV AV1: unverified degrades gracefully
	qsvAV1Job.Caps = &EncoderCaps{HasQSVAV1: true, HasQSVAV1Verified: true, HasQSVAV1TuningVerified: false}
	args, err = buildFfmpegArgs(qsvAV1Job, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	if containsArg(args, "-extbrc") {
		t.Errorf("unverified QSV AV1 must not emit -extbrc: %q", args)
	}

	// NVENC HEVC: verified emits multipass fullres
	nvencJob := TranscodeJob{
		InputFile:  "/media/in.mkv",
		OutputFile: "/media/out.mkv",
		Codec:      "hevc_nvenc",
		Spec: jobspec.JobSpec{
			Container:     "mkv",
			QualityTarget: 22,
		},
		Caps: &EncoderCaps{HasNvencHEVC: true, HasNvencTuningVerified: true},
	}
	args, err = buildFfmpegArgs(nvencJob, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	if !containsSubsequence(args, nvencTuning) {
		t.Errorf("verified NVENC must emit multipass fullres in tuning flags: %q", args)
	}

	// NVENC HEVC: unverified degrades gracefully without advanced tuning flags
	nvencJob.Caps = &EncoderCaps{HasNvencHEVC: true, HasNvencTuningVerified: false}
	args, err = buildFfmpegArgs(nvencJob, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	if containsArg(args, "-multipass") || containsArg(args, "-spatial-aq") {
		t.Errorf("unverified NVENC must omit advanced tuning flags: %q", args)
	}
	if !containsArg(args, "-cq:v") {
		t.Errorf("unverified NVENC must retain quality args: %q", args)
	}

	// VideoToolbox: emits -q:v, never emits -crf
	vtJob := TranscodeJob{
		InputFile:  "/media/in.mkv",
		OutputFile: "/media/out.mp4",
		Codec:      "hevc_videotoolbox",
		Spec: jobspec.JobSpec{
			Container:     "mp4",
			QualityTarget: 22,
		},
	}
	args, err = buildFfmpegArgs(vtJob, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	if containsArg(args, "-crf") {
		t.Errorf("VideoToolbox must never emit -crf: %q", args)
	}
	if !containsSubsequence(args, []string{"-q:v", "60"}) {
		t.Errorf("VideoToolbox with quality target 22 must emit -q:v 60: %q", args)
	}
}

// TestBuildFfmpegArgsHDRMetadata: HDR10 color metadata is passed through as
// ffmpeg color flags for any encoder, and libx265 additionally gets the
// HDR10 x265 params suffix with x265-scaled integers.
func TestBuildFfmpegArgsHDRMetadata(t *testing.T) {
	caps := EncoderCaps{HasLibx265: true, RecommendedEncoder: "libx265"}
	hdr := &ColorInfo{
		Primaries:  "bt2020",
		Transfer:   "smpte2084",
		Colorspace: "bt2020nc",
		MasterDisplay: &MasterDisplay{
			RedX: mustFraction("34000/50000"), RedY: mustFraction("16000/50000"),
			GreenX: mustFraction("13250/28000"), GreenY: mustFraction("34500/50000"),
			BlueX: mustFraction("7500/50000"), BlueY: mustFraction("3000/50000"),
			WhiteX: mustFraction("15635/50000"), WhiteY: mustFraction("16450/50000"),
			MaxLuminance: mustFraction("10000000/10000"), MinLuminance: mustFraction("1/10000"),
		},
		ContentLight: &ContentLight{MaxContent: 1000, MaxAverage: 400},
	}
	job := TranscodeJob{
		InputFile:  "/media/in.mkv",
		OutputFile: "/media/out.mkv",
		Codec:      "hevc",
		Spec: jobspec.JobSpec{
			Container:     "mkv",
			EncoderEngine: "cpu",
			QualityTarget: 22,
			AqMode:        2,
		},
		Caps:      &caps,
		ColorInfo: hdr,
	}
	want := []string{
		"-hide_banner", "-nostdin", "-y", "-fflags", "+genpts", "-probesize", "10000000", "-analyzeduration", "10000000",
		"-protocol_whitelist", "file,pipe", "-i", "/media/in.mkv",
		"-map", "0:v?", "-map", "0:a?", "-map", "0:s?",
		"-ignore_unknown", "-map_metadata", "0", "-map_chapters", "0",
		"-max_muxing_queue_size", "4096", "-copyts", "-c:a", "copy",
		"-c:s", "copy",
		"-c:v", "libx265", "-crf", "22",
		"-pix_fmt", "yuv420p10le",
		"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc",
		"-preset", "medium",
		"-x265-params", "aq-mode=2:sao=0:hdr10=1:hdr10_opt=1:master-display=G(23661,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1):max-cll=1000,400",
		"-progress", "pipe:1", "-nostats", "/media/out.mkv",
	}
	got, err := buildFfmpegArgs(job, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("HDR argv mismatch:\n got: %q\nwant: %q", got, want)
	}

	// SDR (nil ColorInfo): no color flags, plain x265 params.
	job.ColorInfo = nil
	got, err = buildFfmpegArgs(job, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	if containsArg(got, "-color_primaries") || containsArg(got, "-color_trc") || containsArg(got, "-colorspace") {
		t.Errorf("SDR argv must not carry color flags: %q", got)
	}
	if !containsSubsequence(got, []string{"-x265-params", "aq-mode=2:sao=0"}) {
		t.Errorf("SDR argv must not carry HDR x265 params: %q", got)
	}
	if containsArg(got, "hdr10=1") {
		t.Errorf("SDR argv must not carry HDR10 params: %q", got)
	}
}

// TestBuildFfmpegArgsRawCustomFlags: custom flags with a leading-dash token
// are raw trailing ffmpeg args, appended after -nostats so they override
// everything, and never injected into -x265-params.
func TestBuildFfmpegArgsRawCustomFlags(t *testing.T) {
	job := TranscodeJob{
		InputFile:  "/media/in.mkv",
		OutputFile: "/media/out.mkv",
		Codec:      "hevc_nvenc",
		Spec: jobspec.JobSpec{
			Container:     "mkv",
			QualityTarget: 22,
			CustomFlags:   "-bf 4 -rc-lookahead 30",
		},
		// FFmpegPath "" skips the dry-run validation.
	}
	want := []string{
		"-hide_banner", "-nostdin", "-y", "-fflags", "+genpts", "-probesize", "10000000", "-analyzeduration", "10000000",
		"-protocol_whitelist", "file,pipe", "-i", "/media/in.mkv",
		"-map", "0:v?", "-map", "0:a?", "-map", "0:s?",
		"-ignore_unknown", "-map_metadata", "0", "-map_chapters", "0",
		"-max_muxing_queue_size", "4096", "-copyts", "-c:a", "copy",
		"-c:s", "copy",
		"-c:v", "hevc_nvenc", "-rc:v", "vbr", "-cq:v", "22", "-b:v", "0",
		"-profile:v", "main10", "-pix_fmt", "p010le",
		"-g", "240",
		"-tune", "hq", "-multipass", "fullres", "-spatial-aq", "1", "-temporal-aq", "1", "-b_ref_mode", "middle",
		"-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709",
		"-preset", "p6",
		"-progress", "pipe:1", "-nostats", "-bf", "4", "-rc-lookahead", "30", "/media/out.mkv",
	}
	got, err := buildFfmpegArgs(job, nil)
	if err != nil {
		t.Fatalf("buildFfmpegArgs: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("raw custom flags argv mismatch:\n got: %q\nwant: %q", got, want)
	}
	if containsArg(got, "-x265-params") {
		t.Errorf("raw-mode custom flags must not appear in -x265-params: %q", got)
	}
}

// TestBuildFfmpegArgsRawCustomFlagsForbidden: tokens that would break the
// agent's own argv plumbing are rejected with an error naming the token.
func TestBuildFfmpegArgsRawCustomFlagsForbidden(t *testing.T) {
	for _, flags := range []string{
		"-map 0",
		"-i foo",
		"-nostdin",
		"-progress pipe:1",
		"-nostats",
		"-map_metadata 0",
		"-preset ultrafast /tmp/extra.mkv",
		"/tmp/extra.mkv -preset ultrafast",
		"-bf 4 out.mkv",
		"-an /tmp/extra.mkv",
		"-f null /dev/null",
		"-dump_attachment:t:0 /etc/shadow",
		"-bf 4 -",
		"-passlogfile /tmp/pwn",
	} {
		job := TranscodeJob{
			InputFile:  "/media/in.mkv",
			OutputFile: "/media/out.mkv",
			Codec:      "libx264",
			Spec: jobspec.JobSpec{
				CRF:         30,
				PresetSpeed: "ultrafast",
				CustomFlags: flags,
			},
		}
		_, err := buildFfmpegArgs(job, nil)
		if err == nil {
			t.Errorf("custom flags %q must be rejected", flags)
		}
	}
}

func TestSplitRawFlags(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"-bf 4 -rc-lookahead 30", []string{"-bf", "4", "-rc-lookahead", "30"}},
		{`-metadata comment="a b"`, []string{"-metadata", "comment=a b"}},
		{"'-x' \"y z\"", []string{"-x", "y z"}},
		{"keyint=60", []string{"keyint=60"}},
	}
	for _, tt := range tests {
		got, err := splitRawFlags(tt.in)
		if err != nil {
			t.Fatalf("splitRawFlags(%q): %v", tt.in, err)
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("splitRawFlags(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	if _, err := splitRawFlags("-bf '4"); err == nil {
		t.Error("unterminated quote must error")
	}
}

// TestBuildConcatArgsMP4Faststart: MP4 concat output gets -movflags
// +faststart before the output file; MKV does not.
func TestBuildConcatArgsMP4Faststart(t *testing.T) {
	job := TranscodeJob{
		OutputFile: "/media/out.mp4",
		InputFile:  "/media/in.mkv",
		Spec: jobspec.JobSpec{
			Container:        "mp4",
			SubtitleHandling: "strip_all",
		},
	}
	args := BuildConcatArgs(job, "/media/concat.txt", AssemblySidecar{DemuxPath: "/media/out.segments/demux.mkv"}, nil)
	if !containsSubsequence(args, []string{"-movflags", "+faststart", "/media/out.mp4"}) {
		t.Errorf("MP4 concat must end with faststart before output: %q", args)
	}

	job.Spec.Container = "mkv"
	job.OutputFile = "/media/out.mkv"
	args = BuildConcatArgs(job, "/media/concat.txt", AssemblySidecar{DemuxPath: "/media/out.segments/demux.mkv"}, nil)
	if containsArg(args, "-movflags") {
		t.Errorf("MKV concat must not have faststart: %q", args)
	}
}

// TestValidateRawFlagsAgainstRealFFmpeg: when the repo's ffmpeg binary is
// available, a sane flag passes the dry-run and a bogus flag is rejected
// with stderr.
func TestValidateRawFlagsAgainstRealFFmpeg(t *testing.T) {
	ffmpegPath := filepath.Join(repoRoot(t), "bin", "ffmpeg")
	if _, err := os.Stat(ffmpegPath); err != nil {
		t.Skipf("repo ffmpeg binary not available: %v", err)
	}
	if err := ValidateRawFlags(ffmpegPath, "libx264", false, []string{"-bf", "4"}); err != nil {
		t.Errorf("ValidateRawFlags(-bf 4) = %v, want nil", err)
	}
	if err := ValidateRawFlags("", "libx264", false, []string{"-bf", "4"}); err != nil {
		t.Errorf("ValidateRawFlags with empty path must skip validation, got %v", err)
	}
	if err := ValidateRawFlags(ffmpegPath, "libx264", false, []string{"-definitely-not-a-real-flag"}); err == nil {
		t.Error("ValidateRawFlags must reject bogus flags")
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func containsSubsequence(args, sub []string) bool {
	for i := 0; i+len(sub) <= len(args); i++ {
		match := true
		for j := range sub {
			if args[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func TestBuildConcatManifest(t *testing.T) {
	t.Run("valid unix paths", func(t *testing.T) {
		chunks := []string{"/media/chunk_0.mkv", "/media/chunk_1.mkv"}
		got, err := BuildConcatManifest(chunks)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "file '/media/chunk_0.mkv'\nfile '/media/chunk_1.mkv'\n"
		if got != want {
			t.Errorf("BuildConcatManifest() =\n%q\nwant:\n%q", got, want)
		}
	})

	t.Run("normalizes windows backslashes", func(t *testing.T) {
		chunks := []string{`C:\media\chunks\chunk_0.mkv`, `D:\data\test\chunk_1.mkv`}
		got, err := BuildConcatManifest(chunks)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "file 'C:/media/chunks/chunk_0.mkv'\nfile 'D:/data/test/chunk_1.mkv'\n"
		if got != want {
			t.Errorf("BuildConcatManifest() =\n%q\nwant:\n%q", got, want)
		}
	})

	t.Run("escapes single quotes", func(t *testing.T) {
		chunks := []string{"/media/it's a movie/chunk_0.mkv"}
		got, err := BuildConcatManifest(chunks)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "file '/media/it'\\''s a movie/chunk_0.mkv'\n"
		if got != want {
			t.Errorf("BuildConcatManifest() =\n%q\nwant:\n%q", got, want)
		}
	})

	t.Run("rejects newline characters", func(t *testing.T) {
		rejectCases := []string{
			"/media/chunk_0.mkv\nfile '/evil'",
			"/media/chunk_0.mkv\rfile '/evil'",
			"/media/chunk_0.mkv\r\nfile '/evil'",
			"chunk\n.mkv",
			"chunk\r.mkv",
		}
		for _, tc := range rejectCases {
			_, err := BuildConcatManifest([]string{tc})
			if err == nil {
				t.Errorf("expected error for newline-containing chunk %q, got nil", tc)
			}
		}
	})

	t.Run("empty slice returns empty string", func(t *testing.T) {
		got, err := BuildConcatManifest(nil)
		if err != nil {
			t.Fatalf("unexpected error for nil: %v", err)
		}
		if got != "" {
			t.Errorf("expected empty string for nil chunks, got %q", got)
		}
	})
}

func TestBuildConcatManifestWithDurations(t *testing.T) {
	t.Run("emits duration directive per chunk", func(t *testing.T) {
		chunks := []string{"/media/chunk-000.mkv", "/media/chunk-001.mkv", "/media/chunk-002.mkv"}
		durations := []float64{2.021, 2.0, 1.0}
		got, err := BuildConcatManifestWithDurations(chunks, durations)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "file '/media/chunk-000.mkv'\n" +
			"duration 2.021000\n" +
			"file '/media/chunk-001.mkv'\n" +
			"duration 2.000000\n" +
			"file '/media/chunk-002.mkv'\n" +
			"duration 1.000000\n"
		if got != want {
			t.Errorf("BuildConcatManifestWithDurations() =\n%q\nwant:\n%q", got, want)
		}
	})

	t.Run("nil durations falls back to bare file list", func(t *testing.T) {
		chunks := []string{"/media/chunk-000.mkv", "/media/chunk-001.mkv"}
		got, err := BuildConcatManifestWithDurations(chunks, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "file '/media/chunk-000.mkv'\nfile '/media/chunk-001.mkv'\n"
		if got != want {
			t.Errorf("BuildConcatManifestWithDurations() =\n%q\nwant:\n%q", got, want)
		}
	})

	t.Run("rejects length mismatch", func(t *testing.T) {
		chunks := []string{"/media/chunk-000.mkv", "/media/chunk-001.mkv"}
		if _, err := BuildConcatManifestWithDurations(chunks, []float64{2.0}); err == nil {
			t.Fatal("expected error for duration count mismatch, got nil")
		}
	})

	t.Run("rejects non-positive duration", func(t *testing.T) {
		chunks := []string{"/media/chunk-000.mkv"}
		for _, d := range []float64{0, -1.5} {
			if _, err := BuildConcatManifestWithDurations(chunks, []float64{d}); err == nil {
				t.Fatalf("expected error for duration %f, got nil", d)
			}
		}
	})

	t.Run("rejects newline chunk paths", func(t *testing.T) {
		chunks := []string{"/media/chunk-000.mkv\nfile '/evil'"}
		if _, err := BuildConcatManifestWithDurations(chunks, []float64{2.0}); err == nil {
			t.Fatal("expected error for newline-containing chunk, got nil")
		}
	})
}

func TestDefaultPresetInheritsGlobalSettings(t *testing.T) {
	defaultPresets := DefaultPresets()
	balanced, ok := defaultPresets["Balanced"]
	if !ok {
		t.Fatal("expected 'Balanced' default preset")
	}

	global := GlobalSettings{
		HWAccelPreference:          "auto",
		AudioCodecPolicy:           "transcode_aac",
		SubtitleFormatPolicy:       "text_only",
		PreferredAudioLanguages:    []string{"eng", "fra"},
		AudioLanguageMode:          "whitelist",
		PreferredSubtitleLanguages: []string{"eng", "spa"},
		SubtitleLanguageMode:       "whitelist",
		ForcedSubtitlePolicy:       "keep_forced",
		StripMetadata:              true,
	}

	caps := &EncoderCaps{
		HasIntel:   true,
		HasQSVHEVC: true,
	}

	spec := ResolvePreset(balanced, global, caps)

	if spec.EncoderEngine != "qsv" {
		t.Errorf("expected engine 'qsv', got %s", spec.EncoderEngine)
	}
	if spec.AudioHandling != "transcode_aac" || spec.AudioCodec != "aac" {
		t.Errorf("expected audio 'transcode_aac' / 'aac', got %s / %s", spec.AudioHandling, spec.AudioCodec)
	}
	if spec.SubtitleFormatPolicy != "text_only" {
		t.Errorf("expected subtitle policy 'text_only', got %s", spec.SubtitleFormatPolicy)
	}
	if len(spec.PreferredAudioLanguages) != 2 || spec.PreferredAudioLanguages[0] != "eng" || spec.PreferredAudioLanguages[1] != "fra" {
		t.Errorf("expected preferred audio languages ['eng', 'fra'], got %v", spec.PreferredAudioLanguages)
	}
	if len(spec.PreferredSubtitleLanguages) != 2 || spec.PreferredSubtitleLanguages[0] != "eng" || spec.PreferredSubtitleLanguages[1] != "spa" {
		t.Errorf("expected preferred subtitle languages ['eng', 'spa'], got %v", spec.PreferredSubtitleLanguages)
	}

	// Verify BuildStreamMaps behavior
	streams := []StreamInfo{
		{Type: "audio", Language: "eng"},
		{Type: "audio", Language: "deu"},
		{Type: "subtitle", Language: "spa"},
	}
	maps := BuildStreamMaps(spec.PreferredAudioLanguages, spec.PreferredSubtitleLanguages, streams)
	if !containsSubsequence(maps, []string{"-map", "0:v:0"}) ||
		!containsSubsequence(maps, []string{"-map", "0:a:m:language:eng?"}) ||
		!containsSubsequence(maps, []string{"-map", "0:s:m:language:spa?"}) {
		t.Errorf("expected language map flags, got %v", maps)
	}

	// Verify fallback when no streams match
	noMatchStreams := []StreamInfo{
		{Type: "audio", Language: "jpn"},
		{Type: "subtitle", Language: "ita"},
	}
	fallbackMaps := BuildStreamMaps(spec.PreferredAudioLanguages, spec.PreferredSubtitleLanguages, noMatchStreams)
	expectedFallback := []string{"-map", "0:v:0", "-map", "0:a?", "-map", "0:s?"}
	if !reflect.DeepEqual(fallbackMaps, expectedFallback) {
		t.Errorf("expected fallback maps %v, got %v", expectedFallback, fallbackMaps)
	}

	// Verify fallback when filters are blank
	blankMaps := BuildStreamMaps(nil, nil, streams)
	if !reflect.DeepEqual(blankMaps, expectedFallback) {
		t.Errorf("expected fallback maps %v for blank filters, got %v", expectedFallback, blankMaps)
	}
}

func TestBuildStreamMapsLanguageSpecifiers(t *testing.T) {
	maps := BuildStreamMaps([]string{"eng"}, []string{"eng"}, nil)
	expected := []string{
		"-map", "0:v:0",
		"-map", "0:a:m:language:eng?",
		"-map", "0:s:m:language:eng?",
	}
	if !reflect.DeepEqual(maps, expected) {
		t.Errorf("expected %v, got %v", expected, maps)
	}

	for _, m := range maps {
		if m == "0:m:language:eng" {
			t.Errorf("unexpected unqualified map specifier %q", m)
		}
	}
}

func TestCustomPresetOverridesSupersedeGlobalSettings(t *testing.T) {
	customPreset := Preset{
		Name:                       "My Custom Preset",
		IsBuiltin:                  false,
		TargetCodec:                "hevc",
		EncoderEngine:              "cpu",
		AudioHandling:              "copy_all",
		AudioCodec:                 "copy",
		SubtitleHandling:           "copy_all",
		SubtitleFormatPolicy:       "copy_all",
		PreferredAudioLanguages:    []string{"jpn"},
		AudioLanguageMode:          "whitelist",
		PreferredSubtitleLanguages: []string{"jpn"},
		SubtitleLanguageMode:       "whitelist",
		CRF:                        20,
		PresetSpeed:                "slow",
		ColorDepth:                 "10bit",
	}

	global := GlobalSettings{
		HWAccelPreference:          "nvenc",
		AudioCodecPolicy:           "transcode_aac",
		SubtitleFormatPolicy:       "text_only",
		PreferredAudioLanguages:    []string{"eng"},
		PreferredSubtitleLanguages: []string{"eng"},
	}

	caps := &EncoderCaps{
		HasNvencHEVC: true,
	}

	spec := ResolvePreset(customPreset, global, caps)

	if spec.EncoderEngine != "cpu" {
		t.Errorf("expected custom engine 'cpu' to supersede global 'nvenc', got %s", spec.EncoderEngine)
	}
	if spec.AudioHandling != "copy_all" || spec.AudioCodec != "copy" {
		t.Errorf("expected custom audio policy 'copy_all' to supersede global 'transcode_aac', got %s / %s", spec.AudioHandling, spec.AudioCodec)
	}
	if spec.SubtitleFormatPolicy != "copy_all" {
		t.Errorf("expected custom subtitle format policy 'copy_all' to supersede global 'text_only', got %s", spec.SubtitleFormatPolicy)
	}
	if len(spec.PreferredAudioLanguages) != 1 || spec.PreferredAudioLanguages[0] != "jpn" {
		t.Errorf("expected custom audio languages ['jpn'], got %v", spec.PreferredAudioLanguages)
	}
	if spec.CRF != 20 {
		t.Errorf("expected custom CRF 20, got %d", spec.CRF)
	}
}

func TestQSVICQQualityAndGOP240Generated(t *testing.T) {
	defaults := DefaultPresets()
	qsvCaps := &EncoderCaps{
		HasIntel:           true,
		HasQSVHEVC:         true,
		HasQSVHEVCVerified: true,
		RecommendedEncoder: "hevc_qsv",
	}

	globalQSV := GlobalSettings{
		HWAccelPreference: "qsv",
	}

	// 1. The Archivist (QSV: ICQ 13, slower, p010le, -g 240, bt709)
	archivistJob := ResolvePresetJob(defaults["The Archivist"], globalQSV, qsvCaps, "/media/in.mkv", "/media/out.mkv")
	archivistArgs, err := BuildFfmpegArgs(archivistJob, nil)
	if err != nil {
		t.Fatalf("BuildFfmpegArgs failed: %v", err)
	}
	if !containsSubsequence(archivistArgs, []string{"-c:v", "hevc_qsv"}) {
		t.Errorf("expected -c:v hevc_qsv in %v", archivistArgs)
	}
	if !containsSubsequence(archivistArgs, []string{"-rc", "icq", "-global_quality", "13"}) {
		t.Errorf("expected -rc icq -global_quality 13 in %v", archivistArgs)
	}
	if !containsSubsequence(archivistArgs, []string{"-preset", "slower"}) {
		t.Errorf("expected -preset slower in %v", archivistArgs)
	}
	if !containsSubsequence(archivistArgs, []string{"-pix_fmt", "p010le"}) {
		t.Errorf("expected -pix_fmt p010le in %v", archivistArgs)
	}
	if !containsSubsequence(archivistArgs, []string{"-g", "240"}) {
		t.Errorf("expected -g 240 in %v", archivistArgs)
	}
	if !containsSubsequence(archivistArgs, []string{"-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709"}) {
		t.Errorf("expected bt709 color tags in %v", archivistArgs)
	}

	// 2. Balanced (QSV: global_quality 19, medium, p010le, -g 240, bt709)
	balancedJob := ResolvePresetJob(defaults["Balanced"], globalQSV, qsvCaps, "/media/in.mkv", "/media/out.mkv")
	balancedArgs, err := BuildFfmpegArgs(balancedJob, nil)
	if err != nil {
		t.Fatalf("BuildFfmpegArgs failed: %v", err)
	}
	if !containsSubsequence(balancedArgs, []string{"-c:v", "hevc_qsv"}) {
		t.Errorf("expected -c:v hevc_qsv in %v", balancedArgs)
	}
	if !containsSubsequence(balancedArgs, []string{"-global_quality", "19"}) {
		t.Errorf("expected -global_quality 19 in %v", balancedArgs)
	}
	if !containsSubsequence(balancedArgs, []string{"-preset", "medium"}) {
		t.Errorf("expected -preset medium in %v", balancedArgs)
	}
	if !containsSubsequence(balancedArgs, []string{"-pix_fmt", "p010le"}) {
		t.Errorf("expected -pix_fmt p010le in %v", balancedArgs)
	}
	if !containsSubsequence(balancedArgs, []string{"-g", "240"}) {
		t.Errorf("expected -g 240 in %v", balancedArgs)
	}

	// 3. Max Space (QSV: global_quality 25, fast, p010le, -g 240, bt709)
	spaceJob := ResolvePresetJob(defaults["Max Space"], globalQSV, qsvCaps, "/media/in.mkv", "/media/out.mkv")
	spaceArgs, err := BuildFfmpegArgs(spaceJob, nil)
	if err != nil {
		t.Fatalf("BuildFfmpegArgs failed: %v", err)
	}
	if !containsSubsequence(spaceArgs, []string{"-c:v", "hevc_qsv"}) {
		t.Errorf("expected -c:v hevc_qsv in %v", spaceArgs)
	}
	if !containsSubsequence(spaceArgs, []string{"-global_quality", "25"}) {
		t.Errorf("expected -global_quality 25 in %v", spaceArgs)
	}
	if !containsSubsequence(spaceArgs, []string{"-preset", "fast"}) {
		t.Errorf("expected -preset fast in %v", spaceArgs)
	}
	if !containsSubsequence(spaceArgs, []string{"-pix_fmt", "p010le"}) {
		t.Errorf("expected -pix_fmt p010le in %v", spaceArgs)
	}
	if !containsSubsequence(spaceArgs, []string{"-g", "240"}) {
		t.Errorf("expected -g 240 in %v", spaceArgs)
	}

	// 4. CPU (libx265: CRF 18, 22, 26, yuv420p10le, NO -g 240, NO bt709)
	globalCPU := GlobalSettings{
		HWAccelPreference: "cpu",
	}
	cpuCaps := &EncoderCaps{
		HasLibx265:         true,
		RecommendedEncoder: "libx265",
	}

	archivistCPUJob := ResolvePresetJob(defaults["The Archivist"], globalCPU, cpuCaps, "/media/in.mkv", "/media/out.mkv")
	archivistCPUArgs, err := BuildFfmpegArgs(archivistCPUJob, nil)
	if err != nil {
		t.Fatalf("BuildFfmpegArgs failed: %v", err)
	}
	if !containsSubsequence(archivistCPUArgs, []string{"-c:v", "libx265"}) {
		t.Errorf("expected -c:v libx265 in %v", archivistCPUArgs)
	}
	if !containsSubsequence(archivistCPUArgs, []string{"-crf", "18"}) {
		t.Errorf("expected -crf 18 in %v", archivistCPUArgs)
	}
	if !containsSubsequence(archivistCPUArgs, []string{"-preset", "slow"}) {
		t.Errorf("expected -preset slow in %v", archivistCPUArgs)
	}
	if !containsSubsequence(archivistCPUArgs, []string{"-pix_fmt", "yuv420p10le"}) {
		t.Errorf("expected -pix_fmt yuv420p10le in %v", archivistCPUArgs)
	}
	if containsArg(archivistCPUArgs, "-g") {
		t.Errorf("did not expect -g in CPU encode %v", archivistCPUArgs)
	}
	if containsArg(archivistCPUArgs, "-color_primaries") {
		t.Errorf("did not expect -color_primaries in CPU encode %v", archivistCPUArgs)
	}

	balancedCPUJob := ResolvePresetJob(defaults["Balanced"], globalCPU, cpuCaps, "/media/in.mkv", "/media/out.mkv")
	balancedCPUArgs, err := BuildFfmpegArgs(balancedCPUJob, nil)
	if err != nil {
		t.Fatalf("BuildFfmpegArgs failed: %v", err)
	}
	if !containsSubsequence(balancedCPUArgs, []string{"-crf", "22"}) {
		t.Errorf("expected -crf 22 in %v", balancedCPUArgs)
	}
	if !containsSubsequence(balancedCPUArgs, []string{"-preset", "medium"}) {
		t.Errorf("expected -preset medium in %v", balancedCPUArgs)
	}
	if !containsSubsequence(balancedCPUArgs, []string{"-pix_fmt", "yuv420p10le"}) {
		t.Errorf("expected -pix_fmt yuv420p10le in %v", balancedCPUArgs)
	}
	if containsArg(balancedCPUArgs, "-g") {
		t.Errorf("did not expect -g in CPU encode %v", balancedCPUArgs)
	}

	spaceCPUJob := ResolvePresetJob(defaults["Max Space"], globalCPU, cpuCaps, "/media/in.mkv", "/media/out.mkv")
	spaceCPUArgs, err := BuildFfmpegArgs(spaceCPUJob, nil)
	if err != nil {
		t.Fatalf("BuildFfmpegArgs failed: %v", err)
	}
	if !containsSubsequence(spaceCPUArgs, []string{"-crf", "26"}) {
		t.Errorf("expected -crf 26 in %v", spaceCPUArgs)
	}
	if !containsSubsequence(spaceCPUArgs, []string{"-preset", "fast"}) {
		t.Errorf("expected -preset fast in %v", spaceCPUArgs)
	}
	if !containsSubsequence(spaceCPUArgs, []string{"-pix_fmt", "yuv420p10le"}) {
		t.Errorf("expected -pix_fmt yuv420p10le in %v", spaceCPUArgs)
	}
	if containsArg(spaceCPUArgs, "-g") {
		t.Errorf("did not expect -g in CPU encode %v", spaceCPUArgs)
	}
}
