package ffmpeg

import (
	"context"
	"encoding/json"
	"log"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// ColorInfo holds HDR color metadata parsed from ffprobe: the colorimetry
// of the first video stream plus HDR10 mastering-display and content-light
// side data from its first frame.
type ColorInfo struct {
	Primaries     string
	Transfer      string
	Colorspace    string
	MasterDisplay *MasterDisplay
	ContentLight  *ContentLight
}

// fraction is an exact rational colorimetry value parsed from ffprobe's
// "num/den" (or plain decimal) string form. The zero value (nil rat) is
// what an unparseable probe string yields, and scales to 0.
type fraction struct {
	rat *big.Rat
}

// parseFraction parses a "num/den" fraction or plain decimal number into an
// exact rational. Empty or malformed input yields the zero fraction, false.
func parseFraction(s string) (fraction, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return fraction{}, false
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return fraction{}, false
	}
	return fraction{r}, true
}

// scaledInt renders the fraction as an integer scaled to targetDenom,
// rounding to nearest. The zero fraction (parse failure) yields 0.
func (f fraction) scaledInt(targetDenom int64) int64 {
	if f.rat == nil {
		return 0
	}
	scaled := new(big.Rat).Mul(f.rat, new(big.Rat).SetInt64(targetDenom))
	num, den := scaled.Num(), scaled.Denom()
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	// Round half up (probe values are non-negative).
	if new(big.Int).Mul(rem, big.NewInt(2)).Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	return q.Int64()
}

// MasterDisplay holds mastering display color volume data as exact
// rationals, parsed from ffprobe's "num/den" fraction strings and scaled to
// x265's integer conventions only when the x265 param string is built.
type MasterDisplay struct {
	RedX, RedY     fraction
	GreenX, GreenY fraction
	BlueX, BlueY   fraction
	WhiteX, WhiteY fraction
	MaxLuminance   fraction
	MinLuminance   fraction
}

// ContentLight holds content light level data (MaxCLL / MaxFALL) in cd/m².
type ContentLight struct {
	MaxContent int
	MaxAverage int
}

// colorProbeOutput mirrors the two ffprobe sections ProbeColorInfo reads:
// per-stream colorimetry and first-frame side data.
type colorProbeOutput struct {
	Streams []struct {
		CodecType      string `json:"codec_type"`
		ColorPrimaries string `json:"color_primaries"`
		ColorTransfer  string `json:"color_transfer"`
		ColorSpace     string `json:"color_space"`
	} `json:"streams"`
	Frames []struct {
		SideDataList []struct {
			SideDataType string `json:"side_data_type"`
			RedX         string `json:"red_x"`
			RedY         string `json:"red_y"`
			GreenX       string `json:"green_x"`
			GreenY       string `json:"green_y"`
			BlueX        string `json:"blue_x"`
			BlueY        string `json:"blue_y"`
			WhitePointX  string `json:"white_point_x"`
			WhitePointY  string `json:"white_point_y"`
			MinLuminance string `json:"min_luminance"`
			MaxLuminance string `json:"max_luminance"`
			MaxContent   int    `json:"max_content"`
			MaxAverage   int    `json:"max_average"`
		} `json:"side_data_list"`
	} `json:"frames"`
}

// ProbeColorInfo reads HDR color metadata from the first video stream of the
// input file via ffprobe. Returns nil on any probe failure (degrade, never
// fail the job). The returned info is HDR-relevant only when the transfer
// is smpte2084 (HDR10/PQ) or arib-std-b67 (HLG), or the primaries are
// bt2020; otherwise nil is returned (SDR → nothing emitted).
func ProbeColorInfo(ffprobePath, inputPath string) *ColorInfo {
	if err := CheckLocalMediaPath(inputPath); err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Mastering display / content light metadata live in frame side data, so
	// the probe is limited to the first frame.
	out, err := execCommandContext(ctx, ffprobePath,
		"-v", "error",
		"-read_intervals", "%+#1",
		"-show_entries", "stream=codec_type,color_primaries,color_transfer,color_space:frame_side_data=side_data_type,red_x,red_y,green_x,green_y,blue_x,blue_y,white_point_x,white_point_y,min_luminance,max_luminance,max_content,max_average",
		"-of", "json",
		inputPath,
	).Output()
	if err != nil {
		log.Printf("color probe failed for %s: %v; degrading", inputPath, err)
		return nil
	}
	return parseColorInfo(out)
}

// parseColorInfo extracts HDR-relevant color metadata from ffprobe JSON.
// Returns nil for SDR or unparseable input.
func parseColorInfo(data []byte) *ColorInfo {
	var output colorProbeOutput
	if err := json.Unmarshal(data, &output); err != nil {
		log.Printf("failed to parse color probe json: %v; degrading", err)
		return nil
	}

	info := &ColorInfo{}
	for _, s := range output.Streams {
		if s.CodecType != "video" {
			continue
		}
		info.Primaries = s.ColorPrimaries
		info.Transfer = s.ColorTransfer
		info.Colorspace = s.ColorSpace
		break
	}

	if info.Transfer != "smpte2084" && info.Transfer != "arib-std-b67" && info.Primaries != "bt2020" {
		return nil
	}

	for _, f := range output.Frames {
		for _, sd := range f.SideDataList {
			switch sd.SideDataType {
			case "Mastering display metadata":
				info.MasterDisplay = &MasterDisplay{
					RedX:         mustFraction(sd.RedX),
					RedY:         mustFraction(sd.RedY),
					GreenX:       mustFraction(sd.GreenX),
					GreenY:       mustFraction(sd.GreenY),
					BlueX:        mustFraction(sd.BlueX),
					BlueY:        mustFraction(sd.BlueY),
					WhiteX:       mustFraction(sd.WhitePointX),
					WhiteY:       mustFraction(sd.WhitePointY),
					MaxLuminance: mustFraction(sd.MaxLuminance),
					MinLuminance: mustFraction(sd.MinLuminance),
				}
			case "Content light level metadata":
				info.ContentLight = &ContentLight{
					MaxContent: sd.MaxContent,
					MaxAverage: sd.MaxAverage,
				}
			}
		}
		if info.MasterDisplay != nil || info.ContentLight != nil {
			break
		}
	}

	return info
}

// mustFraction parses a probe string, mapping the malformed/empty case to
// the zero fraction so a partially-populated side_data entry degrades to 0
// in the emitted params instead of failing the probe.
func mustFraction(s string) fraction {
	f, _ := parseFraction(s)
	return f
}

// colorInfoToX265Suffix builds the HDR10 x265 parameter suffix for a
// ColorInfo: hdr10 signaling, mastering display, and max-cll. Returns ""
// unless the transfer is PQ (smpte2084) with mastering display data, and
// only includes max-cll when the content light level is above zero.
func colorInfoToX265Suffix(info *ColorInfo) string {
	if info == nil || info.MasterDisplay == nil || info.Transfer != "smpte2084" {
		return ""
	}
	parts := []string{"hdr10=1", "hdr10_opt=1", "master-display=" + masterDisplayX265(info.MasterDisplay)}
	if info.ContentLight != nil && info.ContentLight.MaxContent > 0 {
		parts = append(parts, fmtMaxCLL(info.ContentLight))
	}
	return strings.Join(parts, ":")
}

// fmtMaxCLL renders max-cll=<MaxCLL>,<MaxFALL>.
func fmtMaxCLL(cl *ContentLight) string {
	return "max-cll=" + strconv.Itoa(cl.MaxContent) + "," + strconv.Itoa(cl.MaxAverage)
}

// masterDisplayX265 renders the mastering display in x265's
// master-display=G(gx,gy)B(bx,by)R(rx,ry)WP(wx,wy)L(max,min) syntax.
// xy coordinates are scaled to denominator 50000 and luminance to
// denominator 10000, both rounded, per x265's integer conventions.
func masterDisplayX265(md *MasterDisplay) string {
	pair := func(x, y fraction, denom int64) string {
		return strconv.FormatInt(x.scaledInt(denom), 10) + "," + strconv.FormatInt(y.scaledInt(denom), 10)
	}
	return "G(" + pair(md.GreenX, md.GreenY, 50000) + ")" +
		"B(" + pair(md.BlueX, md.BlueY, 50000) + ")" +
		"R(" + pair(md.RedX, md.RedY, 50000) + ")" +
		"WP(" + pair(md.WhiteX, md.WhiteY, 50000) + ")" +
		"L(" + strconv.FormatInt(md.MaxLuminance.scaledInt(10000), 10) + "," + strconv.FormatInt(md.MinLuminance.scaledInt(10000), 10) + ")"
}
