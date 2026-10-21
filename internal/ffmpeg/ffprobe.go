package ffmpeg

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

type MediaInfo struct {
	Codec       string
	Duration    float64
	Size        int64
	Bitrate     int64
	AudioStream string
	AudioCodec  string
	HDR         string
	TotalFrames      int
	ExactFramesKnown bool
	Width            int
	Height      int
	Resolution  string

	// VideoStreamDuration is the duration ffprobe reports for the video
	// stream itself (stream-level duration field or DURATION tags), as
	// opposed to Duration, which prefers the container format duration.
	// Concatenated Matroska output can carry muxer/DTS padding at chunk
	// junctions that inflates the container duration without representing
	// actual content, so verification compares stream durations when both
	// probes expose them. Zero when ffprobe reports no stream-level duration.
	VideoStreamDuration float64

	// VideoStartTime and AudioStartTime are the streams' start_time values in
	// seconds. VideoStartTimeSet/AudioStartTimeSet report whether ffprobe
	// actually exposed each value: a genuine 0.0 start is common for video,
	// so presence cannot be inferred from the value alone. Verification uses
	// them to compare the audio-vs-video offset between input and output.
	VideoStartTime    float64
	VideoStartTimeSet bool
	AudioStartTime    float64
	AudioStartTimeSet bool
}

// AVOffset returns the audio-minus-video start-time offset in seconds and
// whether both stream start times were reported. Callers skip the alignment
// check rather than fail a job when either side lacks the metadata.
func (m MediaInfo) AVOffset() (offset float64, ok bool) {
	if !m.VideoStartTimeSet || !m.AudioStartTimeSet {
		return 0, false
	}
	return m.AudioStartTime - m.VideoStartTime, true
}

// ClassifyResolution normalizes video dimensions into a standard resolution tier:
// "4K", "1080p", "720p", "SD", or "unknown".
func ClassifyResolution(width, height int) string {
	if height >= 2000 || width >= 3800 {
		return "4K"
	}
	if height >= 1000 || width >= 1900 {
		return "1080p"
	}
	if height >= 700 || width >= 1200 {
		return "720p"
	}
	if height > 0 || width > 0 {
		return "SD"
	}
	return "unknown"
}

type ffprobeOutput struct {
	Format struct {
		Duration string            `json:"duration"`
		Size     string            `json:"size"`
		BitRate  string            `json:"bit_rate"`
		Tags     map[string]string `json:"tags"`
	} `json:"format"`
	Streams []struct {
		CodecName      string            `json:"codec_name"`
		CodecType      string            `json:"codec_type"`
		Profile        string            `json:"profile"`
		Width          int               `json:"width"`
		Height         int               `json:"height"`
		StartTime      string            `json:"start_time"`
		Channels       int               `json:"channels"`
		ChannelLayout  string            `json:"channel_layout"`
		ColorTransfer  string            `json:"color_transfer"`
		ColorPrimaries string            `json:"color_primaries"`
		Duration       string            `json:"duration"`
		NbFrames       string            `json:"nb_frames"`
		RFrameRate     string            `json:"r_frame_rate"`
		AvgFrameRate   string            `json:"avg_frame_rate"`
		Tags           map[string]string `json:"tags"`
		SideDataList   []struct {
			SideDataType string `json:"side_data_type"`
		} `json:"side_data_list"`
	} `json:"streams"`
}

func ProbeMedia(ffprobePath string, filePath string) (MediaInfo, error) {
	if err := CheckLocalMediaPath(filePath); err != nil {
		return MediaInfo{}, fmt.Errorf("invalid probe path: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := execCommandContext(ctx, ffprobePath,
		"-v", "error",
		"-probesize", "10000000",
		"-analyzeduration", "10000000",
		"-show_format",
		"-show_streams",
		"-of", "json",
		filePath,
	)
	out, err := cmd.Output()
	if err != nil {
		return MediaInfo{}, fmt.Errorf("ffprobe failed: %w", err)
	}

	var output ffprobeOutput
	if err := json.Unmarshal(out, &output); err != nil {
		return MediaInfo{}, fmt.Errorf("failed to parse ffprobe json: %w", err)
	}

	var info MediaInfo
	info.HDR = "SDR"
	bestAudioScore := -1
	var streamDuration float64
	var videoFPS float64

	for _, stream := range output.Streams {
		if stream.CodecType == "video" && info.Codec == "" {
			info.Codec = stream.CodecName
			info.Width = stream.Width
			info.Height = stream.Height
			info.Resolution = ClassifyResolution(stream.Width, stream.Height)

			if st, ok := parseStreamStartTime(stream.StartTime); ok {
				info.VideoStartTime = st
				info.VideoStartTimeSet = true
			}

			if stream.NbFrames != "" {
				if nf, err := strconv.Atoi(stream.NbFrames); err == nil && nf > 0 {
					info.TotalFrames = nf
					info.ExactFramesKnown = true
				}
			} else if stream.Tags != nil {
				for k, v := range stream.Tags {
					if strings.HasPrefix(strings.ToLower(k), "number_of_frames") && v != "" {
						if nf, err := strconv.Atoi(v); err == nil && nf > 0 {
							info.TotalFrames = nf
							info.ExactFramesKnown = true
							break
						}
					}
				}
			}

			if stream.Duration != "" {
				if d, err := strconv.ParseFloat(stream.Duration, 64); err == nil && d > 0 {
					streamDuration = d
				}
			} else if stream.Tags != nil {
				for _, tagKey := range []string{"DURATION", "DURATION-eng", "duration"} {
					if dt, ok := stream.Tags[tagKey]; ok && dt != "" {
						parts := strings.Split(dt, ":")
						if len(parts) == 3 {
							h, _ := strconv.ParseFloat(parts[0], 64)
							m, _ := strconv.ParseFloat(parts[1], 64)
							s, _ := strconv.ParseFloat(parts[2], 64)
							parsedSec := h*3600 + m*60 + s
							if parsedSec > 0 {
								streamDuration = parsedSec
								break
							}
						}
					}
				}
			}

			for _, fr := range []string{stream.RFrameRate, stream.AvgFrameRate} {
				if fr != "" && fr != "0/0" {
					parts := strings.Split(fr, "/")
					if len(parts) == 2 {
						num, _ := strconv.ParseFloat(parts[0], 64)
						den, _ := strconv.ParseFloat(parts[1], 64)
						if den > 0 && num > 0 {
							videoFPS = num / den
							break
						}
					}
				}
			}

			hdr := "SDR"
			for _, sd := range stream.SideDataList {
				sdType := strings.ToLower(sd.SideDataType)
				if strings.Contains(sdType, "dovi") || strings.Contains(sdType, "dolby vision") {
					hdr = "DV"
					break
				}
			}
			if hdr != "DV" {
				ct := strings.ToLower(stream.ColorTransfer)
				cp := strings.ToLower(stream.ColorPrimaries)
				if ct == "smpte2084" {
					hdr = "HDR10"
				} else if ct == "arib-std-b67" {
					hdr = "HLG"
				} else if strings.Contains(ct, "hdr") || cp == "bt2020" {
					hdr = "HDR"
				}
			}
			info.HDR = hdr
		} else if stream.CodecType == "audio" {
			score := scoreAudioStream(stream.CodecName, stream.Profile, stream.Channels)
			if score > bestAudioScore {
				bestAudioScore = score
				info.AudioCodec = stream.CodecName
				info.AudioStream = formatAudioStream(stream.CodecName, stream.Profile, stream.Channels, stream.ChannelLayout)
				if st, ok := parseStreamStartTime(stream.StartTime); ok {
					info.AudioStartTime = st
					info.AudioStartTimeSet = true
				}
			}
		}
	}

	if output.Format.Duration != "" {
		if dur, err := strconv.ParseFloat(output.Format.Duration, 64); err == nil {
			info.Duration = dur
		}
	}
	if info.Duration == 0 && streamDuration > 0 {
		info.Duration = streamDuration
	}
	info.VideoStreamDuration = streamDuration

	if !info.ExactFramesKnown && output.Format.Tags != nil {
		for k, v := range output.Format.Tags {
			if strings.HasPrefix(strings.ToLower(k), "number_of_frames") && v != "" {
				if nf, err := strconv.Atoi(v); err == nil && nf > 0 {
					info.TotalFrames = nf
					info.ExactFramesKnown = true
					break
				}
			}
		}
	}

	if info.TotalFrames == 0 && info.Duration > 0 && videoFPS > 0 {
		info.TotalFrames = int(math.Round(info.Duration * videoFPS))
	}

	if output.Format.Size != "" {
		if size, err := strconv.ParseInt(output.Format.Size, 10, 64); err == nil {
			info.Size = size
		}
	}

	if output.Format.BitRate != "" {
		if br, err := strconv.ParseInt(output.Format.BitRate, 10, 64); err == nil {
			info.Bitrate = br
		}
	}

	// if Size is not from ffprobe, check file
	if info.Size == 0 {
		if fi, err := os.Stat(filePath); err == nil {
			info.Size = fi.Size()
		}
	}

	return info, nil
}

// Chapter is one chapter marker of a container: Start/End in milliseconds
// and an optional title.
type Chapter struct {
	Start int64
	End   int64
	Title string
}

// ProbeChapters lists the chapter markers of the container via ffprobe, or
// nil when the probe fails or the path is rejected. Callers treat nil as
// "unknown/absent" and degrade rather than failing a job on a metadata probe
// hiccup, mirroring ProbeChapterCount.
func ProbeChapters(ffprobePath, filePath string) []Chapter {
	if err := CheckLocalMediaPath(filePath); err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := execCommandContext(ctx, ffprobePath,
		"-v", "error",
		"-show_entries", "chapter=start_time,end_time:chapter_tags=title",
		"-of", "json",
		filePath,
	).Output()
	if err != nil {
		log.Printf("chapter probe failed for %s: %v", filePath, err)
		return nil
	}
	var output struct {
		Chapters []struct {
			StartTime string            `json:"start_time"`
			EndTime   string            `json:"end_time"`
			Tags      map[string]string `json:"tags"`
		} `json:"chapters"`
	}
	if err := json.Unmarshal(out, &output); err != nil {
		log.Printf("failed to parse ffprobe chapter json for %s: %v", filePath, err)
		return nil
	}
	var chapters []Chapter
	for _, c := range output.Chapters {
		startSec, err1 := strconv.ParseFloat(c.StartTime, 64)
		endSec, err2 := strconv.ParseFloat(c.EndTime, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		chapters = append(chapters, Chapter{
			Start: int64(math.Round(startSec * 1000)),
			End:   int64(math.Round(endSec * 1000)),
			Title: c.Tags["title"],
		})
	}
	return chapters
}

// ProbeChapterCount reports how many chapters the container carries, or -1
// when the probe fails or the path is rejected. Callers treat -1 as
// "unknown" and skip chapter-count comparisons rather than failing a job on
// a metadata probe hiccup.
func ProbeChapterCount(ffprobePath, filePath string) int {
	if err := CheckLocalMediaPath(filePath); err != nil {
		return -1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := execCommandContext(ctx, ffprobePath,
		"-v", "error",
		"-show_entries", "chapter=id",
		"-of", "json",
		filePath,
	).Output()
	if err != nil {
		log.Printf("chapter probe failed for %s: %v", filePath, err)
		return -1
	}
	var output struct {
		Chapters []struct {
			ID int `json:"id"`
		} `json:"chapters"`
	}
	if err := json.Unmarshal(out, &output); err != nil {
		log.Printf("failed to parse ffprobe chapter json for %s: %v", filePath, err)
		return -1
	}
	return len(output.Chapters)
}

// parseStreamStartTime converts ffprobe's stream start_time field to
// seconds; ok is false when the field is absent or unparseable.
func parseStreamStartTime(startTime string) (sec float64, ok bool) {
	if startTime == "" {
		return 0, false
	}
	st, err := strconv.ParseFloat(startTime, 64)
	if err != nil {
		return 0, false
	}
	return st, true
}

func scoreAudioStream(codec, profile string, channels int) int {
	tier := 1
	c := strings.ToLower(codec)
	p := strings.ToLower(profile)

	switch {
	case c == "truehd" || strings.Contains(p, "truehd"):
		tier = 10
	case strings.Contains(c, "dts") && (strings.Contains(p, "ma") || strings.Contains(p, "master") || strings.Contains(p, "x")):
		tier = 9
	case c == "flac" || strings.HasPrefix(c, "pcm_"):
		tier = 8
	case c == "eac3" || strings.Contains(p, "atmos"):
		tier = 7
	case strings.Contains(c, "dts"):
		tier = 6
	case c == "ac3":
		tier = 5
	case c == "opus":
		tier = 4
	case c == "aac":
		tier = 3
	case c == "mp3":
		tier = 2
	}

	return tier*100 + channels*10
}

func formatAudioStream(codec, profile string, channels int, layout string) string {
	c := strings.ToLower(codec)
	p := strings.ToLower(profile)

	name := strings.ToUpper(codec)
	switch {
	case c == "truehd" || strings.Contains(p, "truehd"):
		name = "TrueHD"
	case strings.Contains(c, "dts") && (strings.Contains(p, "ma") || strings.Contains(p, "master")):
		name = "DTS-HD MA"
	case strings.Contains(c, "dts") && strings.Contains(p, "hra"):
		name = "DTS-HD"
	case strings.Contains(c, "dts"):
		name = "DTS"
	case c == "eac3":
		name = "E-AC3"
	case c == "ac3":
		name = "AC3"
	case c == "aac":
		name = "AAC"
	case c == "flac":
		name = "FLAC"
	case c == "opus":
		name = "Opus"
	case c == "mp3":
		name = "MP3"
	}

	chStr := ""
	switch channels {
	case 8:
		chStr = "7.1"
	case 6:
		chStr = "5.1"
	case 2:
		chStr = "2.0"
	case 1:
		chStr = "1.0"
	default:
		if channels > 0 {
			chStr = fmt.Sprintf("%dch", channels)
		} else if layout != "" {
			chStr = layout
		}
	}

	if chStr != "" {
		return name + " " + chStr
	}
	return name
}
