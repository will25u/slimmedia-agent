package ffmpeg

import (
	"strings"
	"testing"
)

// sampleColorProbeJSON mirrors real ffprobe output for a 10-bit HDR10
// stream: colorimetry on the video stream, mastering display as flat
// "num/den" fraction strings, content light as plain integers.
const sampleColorProbeJSON = `{
  "streams": [
    {"codec_type": "video", "color_primaries": "bt2020", "color_transfer": "smpte2084", "color_space": "bt2020nc"},
    {"codec_type": "audio"}
  ],
  "frames": [
    {"side_data_list": [
      {"side_data_type": "H.26[45] User Data Unregistered SEI message"},
      {
        "side_data_type": "Mastering display metadata",
        "red_x": "34000/50000",
        "red_y": "16000/50000",
        "green_x": "13250/28000",
        "green_y": "34500/50000",
        "blue_x": "7500/50000",
        "blue_y": "3000/50000",
        "white_point_x": "15635/50000",
        "white_point_y": "16450/50000",
        "min_luminance": "1/10000",
        "max_luminance": "10000000/10000"
      },
      {"side_data_type": "Content light level metadata", "max_content": 1000, "max_average": 400}
    ]}
  ]
}`

func TestParseColorInfoHDR10(t *testing.T) {
	info := parseColorInfo([]byte(sampleColorProbeJSON))
	if info == nil {
		t.Fatal("parseColorInfo returned nil for HDR10 sample")
	}
	if info.Primaries != "bt2020" || info.Transfer != "smpte2084" || info.Colorspace != "bt2020nc" {
		t.Errorf("colorimetry = %q/%q/%q", info.Primaries, info.Transfer, info.Colorspace)
	}
	md := info.MasterDisplay
	if md == nil {
		t.Fatal("MasterDisplay not parsed")
	}
	if md.RedX.scaledInt(50000) != 34000 || md.MaxLuminance.scaledInt(10000) != 10000000 || md.MinLuminance.scaledInt(10000) != 1 {
		t.Errorf("MasterDisplay = %+v", md)
	}
	cl := info.ContentLight
	if cl == nil || cl.MaxContent != 1000 || cl.MaxAverage != 400 {
		t.Errorf("ContentLight = %+v", cl)
	}
}

func TestParseColorInfoSDRReturnsNil(t *testing.T) {
	sdr := strings.NewReplacer(
		"smpte2084", "bt709",
		`"color_primaries": "bt2020"`, `"color_primaries": "bt709"`,
	).Replace(sampleColorProbeJSON)
	if info := parseColorInfo([]byte(sdr)); info != nil {
		t.Errorf("SDR input must yield nil ColorInfo, got %+v", info)
	}
	if info := parseColorInfo([]byte("not json")); info != nil {
		t.Errorf("garbage input must yield nil ColorInfo, got %+v", info)
	}
}

func TestColorInfoToX265SuffixScaling(t *testing.T) {
	info := parseColorInfo([]byte(sampleColorProbeJSON))
	got := colorInfoToX265Suffix(info)
	// xy scaled to denominator 50000 (green_x 13250/28000 -> 23661),
	// luminance to denominator 10000.
	want := "hdr10=1:hdr10_opt=1:master-display=G(23661,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1):max-cll=1000,400"
	if got != want {
		t.Errorf("colorInfoToX265Suffix() =\n%q\nwant:\n%q", got, want)
	}

	// HLG (no PQ) gets color flags but no HDR10 suffix.
	hlg := *info
	hlg.Transfer = "arib-std-b67"
	if got := colorInfoToX265Suffix(&hlg); got != "" {
		t.Errorf("HLG suffix = %q, want empty", got)
	}

	// Zero content light level drops max-cll.
	zeroCLL := *info
	zeroCLL.ContentLight = &ContentLight{MaxContent: 0, MaxAverage: 0}
	got = colorInfoToX265Suffix(&zeroCLL)
	if strings.Contains(got, "max-cll") {
		t.Errorf("zero content light must drop max-cll, got %q", got)
	}

	// No mastering display, no suffix.
	noMD := *info
	noMD.MasterDisplay = nil
	if got := colorInfoToX265Suffix(&noMD); got != "" {
		t.Errorf("suffix without mastering display = %q, want empty", got)
	}
}

func TestFractionScaledInt(t *testing.T) {
	tests := []struct {
		in    string
		denom int64
		want  int64
	}{
		{"34000/50000", 50000, 34000},
		{"13250/28000", 50000, 23661},
		{"10000000/10000", 10000, 10000000},
		{"1/10000", 10000, 1},
		{"0.5", 50000, 25000},
		{"", 50000, 0},
		{"1/0", 50000, 0},
		{"junk", 50000, 0},
	}
	for _, tt := range tests {
		f, _ := parseFraction(tt.in)
		if got := f.scaledInt(tt.denom); got != tt.want {
			t.Errorf("parseFraction(%q).scaledInt(%d) = %d, want %d", tt.in, tt.denom, got, tt.want)
		}
	}
}
