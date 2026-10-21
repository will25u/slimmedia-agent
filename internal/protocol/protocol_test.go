package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestEnvelopeSerialization(t *testing.T) {
	now := time.Now().Truncate(time.Second) // JSON serialization truncates to some extent depending on format, keep it simple
	env := Envelope{
		Type:    "transcode.progress",
		ID:      "uuid-v7",
		TS:      now,
		Payload: json.RawMessage(`{"status":"working"}`),
	}

	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Failed to marshal Envelope: %v", err)
	}

	var env2 Envelope
	if err := json.Unmarshal(b, &env2); err != nil {
		t.Fatalf("Failed to unmarshal Envelope: %v", err)
	}

	if env2.Type != env.Type {
		t.Errorf("Expected Type %s, got %s", env.Type, env2.Type)
	}
	if env2.ID != env.ID {
		t.Errorf("Expected ID %s, got %s", env.ID, env2.ID)
	}
	if !env2.TS.Equal(env.TS) {
		t.Errorf("Expected TS %v, got %v", env.TS, env2.TS)
	}
	if string(env2.Payload) != string(env.Payload) {
		t.Errorf("Expected Payload %s, got %s", string(env.Payload), string(env2.Payload))
	}
}

func TestTelemetrySerialization(t *testing.T) {
	live := LiveTelemetry{
		JobID:       "job-123",
		FPS:         60.5,
		ETASeconds:  120,
		Bitrate:     5000,
		ProgressPct: 45.2,
		GPUUtilPct:  85.5,
	}

	b, err := json.Marshal(live)
	if err != nil {
		t.Fatalf("Failed to marshal LiveTelemetry: %v", err)
	}

	var live2 LiveTelemetry
	if err := json.Unmarshal(b, &live2); err != nil {
		t.Fatalf("Failed to unmarshal LiveTelemetry: %v", err)
	}

	if live2.JobID != live.JobID || live2.FPS != live.FPS {
		t.Errorf("Serialization mismatch for LiveTelemetry: %+v != %+v", live, live2)
	}
}

// TestStartJobPayloadBackwardCompat verifies the 000012 tuning fields are
// pure additions: a pre-tuning payload unmarshals cleanly into the extended
// struct (this is what old agents do with unknown keys — drop them), and a
// payload without tuning fields omits them entirely on the wire.
func TestStartJobPayloadBackwardCompat(t *testing.T) {
	legacy := `{"job_id":"j1","input_path":"/media/a.mkv","target_codec":"hevc","crf":23,"preset_speed":"medium","audio_codec":"copy"}`
	var p StartJobPayload
	if err := json.Unmarshal([]byte(legacy), &p); err != nil {
		t.Fatalf("legacy payload failed to unmarshal: %v", err)
	}
	if p.JobID != "j1" || p.CRF != 23 || p.PresetSpeed != "medium" {
		t.Errorf("legacy fields not decoded: %+v", p)
	}
	if p.EncoderEngine != "" || p.QualityTarget != 0 || p.AudioHandling != "" ||
		p.SubtitleHandling != "" || p.ColorDepth != "" || p.FilmGrainSao ||
		p.AqMode != 0 || p.ContentTuning != "" || p.CustomFlags != "" {
		t.Errorf("legacy payload must leave tuning fields zero-valued, got %+v", p)
	}
	if p.SpecVersion != 0 {
		t.Errorf("legacy payload must leave spec_version zero-valued, got %+v", p)
	}

	full := `{"job_id":"j1","input_path":"/media/a.mkv","encoder_engine":"nvenc","quality_target":22,"audio_handling":"transcode_aac","subtitle_handling":"strip_all","color_depth":"10bit","film_grain_sao":true,"aq_mode":3,"content_tuning":"film","custom_flags":"keyint=60"}`
	var q StartJobPayload
	if err := json.Unmarshal([]byte(full), &q); err != nil {
		t.Fatalf("tuning payload failed to unmarshal: %v", err)
	}
	if q.EncoderEngine != "nvenc" || q.QualityTarget != 22 || q.AudioHandling != "transcode_aac" ||
		q.SubtitleHandling != "strip_all" || q.ColorDepth != "10bit" || !q.FilmGrainSao ||
		q.AqMode != 3 || q.ContentTuning != "film" || q.CustomFlags != "keyint=60" {
		t.Errorf("tuning fields not decoded: %+v", q)
	}

	// Old agents receive payloads from new APIs too: unset tuning fields must
	// be absent from the wire entirely (omitempty), so an old agent's
	// unknown-key-drop semantics apply and nothing breaks.
	b, err := json.Marshal(StartJobPayload{JobID: "j1", InputPath: "/media/a.mkv"})
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	for _, key := range []string{
		"encoder_engine", "quality_target", "audio_handling", "subtitle_handling",
		"color_depth", "film_grain_sao", "aq_mode", "content_tuning", "custom_flags",
		"spec_version",
	} {
		if strings.Contains(string(b), key) {
			t.Errorf("unset tuning field %q must be omitted from payload JSON: %s", key, b)
		}
	}

	// A modern payload carries spec_version alongside the tuning fields; the
	// agent reduces legacy detection to this field.
	modern := `{"job_id":"j1","input_path":"/media/a.mkv","spec_version":1,"encoder_engine":"nvenc"}`
	var m StartJobPayload
	if err := json.Unmarshal([]byte(modern), &m); err != nil {
		t.Fatalf("modern payload failed to unmarshal: %v", err)
	}
	if m.SpecVersion != 1 {
		t.Errorf("spec_version not decoded: %+v", m)
	}
}

func TestStartJobPayloadWireContractV2(t *testing.T) {
	v2JSON := `{
		"job_id": "00000000-0000-0000-0000-000000000000",
		"media_item_id": "00000000-0000-0000-0000-000000000000",
		"input_path": "/media/movies/sample.mkv",
		"relative_path": "sample.mkv",
		"local_path": "/mnt/storage/media",
		"spec_version": 2,
		"video": {
			"target_codec": "hevc",
			"encoder_engine": "nvenc",
			"crf": 20,
			"preset_speed": "medium",
			"quality_target": 20,
			"color_depth": "10bit",
			"film_grain_sao": false,
			"aq_mode": 3,
			"content_tuning": "film"
		},
		"audio": {
			"codec": "aac",
			"codec_policy": "transcode_aac",
			"language_mode": "whitelist",
			"preferred_languages": ["eng", "jpn"]
		},
		"subtitles": {
			"format_policy": "keep_all",
			"language_mode": "whitelist",
			"preferred_languages": ["eng"],
			"forced_policy": "keep_forced",
			"delivery_mode": "soft_stream"
		},
		"container": {
			"format": "mkv",
			"strip_metadata": true,
			"output_file_strategy": "alongside",
			"action_on_success": "keep"
		},
		"execution": {
			"custom_flags": ["-tune", "film", "-rc-lookahead", "40"],
			"resolution_warnings": [],
			"post_transcode_verification": true
		}
	}`

	var p StartJobPayload
	if err := json.Unmarshal([]byte(v2JSON), &p); err != nil {
		t.Fatalf("v2 payload failed to unmarshal: %v", err)
	}

	if p.SpecVersion != 2 {
		t.Errorf("expected spec_version 2, got %d", p.SpecVersion)
	}
	if p.TargetCodec != "hevc" || p.EncoderEngine != "nvenc" || p.CRF != 20 {
		t.Errorf("video fields not populated into flat fields: %+v", p)
	}
	if p.AudioCodec != "aac" || p.AudioHandling != "transcode_aac" {
		t.Errorf("audio fields not populated into flat fields: %+v", p)
	}
	if p.Container != "mkv" || !p.StripMetadata {
		t.Errorf("container fields not populated into flat fields: %+v", p)
	}
	if p.CustomFlags != "-tune film -rc-lookahead 40" {
		t.Errorf("custom flags not populated into flat string: %q", p.CustomFlags)
	}

	// Verify serialization produces valid v2 shape with nested objects
	marshaled, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("failed to marshal v2 payload: %v", err)
	}
	str := string(marshaled)
	if !strings.Contains(str, `"spec_version":2`) {
		t.Errorf("marshaled v2 missing spec_version 2: %s", str)
	}
	if !strings.Contains(str, `"video":{`) || !strings.Contains(str, `"container":{`) {
		t.Errorf("marshaled v2 missing structured video or container objects: %s", str)
	}
}

func TestStartJobPayloadQualityTierRoundTrip(t *testing.T) {
	// v2 structured wire carries the tier in the video section.
	v2 := `{
		"job_id": "j1",
		"input_path": "/media/a.mkv",
		"spec_version": 2,
		"video": {"target_codec": "hevc", "encoder_engine": "nvenc", "quality_tier": "archivist"},
		"container": {"format": "mkv", "output_file_strategy": "alongside", "action_on_success": "keep"}
	}`
	var p StartJobPayload
	if err := json.Unmarshal([]byte(v2), &p); err != nil {
		t.Fatalf("v2 quality_tier payload failed to unmarshal: %v", err)
	}
	if p.QualityTier != QualityTierArchivist {
		t.Errorf("expected flat quality_tier %q, got %q", QualityTierArchivist, p.QualityTier)
	}
	if p.Video == nil || p.Video.QualityTier != QualityTierArchivist {
		t.Errorf("expected video.quality_tier %q, got %+v", QualityTierArchivist, p.Video)
	}
	marshaled, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if !strings.Contains(string(marshaled), `"quality_tier":"archivist"`) {
		t.Errorf("marshaled v2 must retain quality_tier: %s", marshaled)
	}

	// Re-unmarshal the marshaled form: the tier must survive a full round trip.
	var rt StartJobPayload
	if err := json.Unmarshal(marshaled, &rt); err != nil {
		t.Fatalf("round-trip unmarshal failed: %v", err)
	}
	if rt.QualityTier != QualityTierArchivist || rt.Video == nil || rt.Video.QualityTier != QualityTierArchivist {
		t.Errorf("quality_tier lost in round trip: %+v", rt)
	}

	// v1 flat wire carries the tier top-level.
	v1 := `{"job_id":"j1","input_path":"/media/a.mkv","spec_version":1,"encoder_engine":"nvenc","quality_tier":"archivist"}`
	var flat StartJobPayload
	if err := json.Unmarshal([]byte(v1), &flat); err != nil {
		t.Fatalf("v1 quality_tier payload failed to unmarshal: %v", err)
	}
	if flat.QualityTier != QualityTierArchivist {
		t.Errorf("v1 flat quality_tier not decoded: %+v", flat)
	}
	if flat.Video == nil || flat.Video.QualityTier != QualityTierArchivist {
		t.Errorf("v1 flat tier must populate the Video section: %+v", flat.Video)
	}

	// A caller that populates only the flat field with a non-nil Video
	// section must not lose the tier on the wire.
	flatOnly := StartJobPayload{
		JobID:       "j-flat",
		InputPath:   "/media/a.mkv",
		SpecVersion: 2,
		QualityTier: QualityTierArchivist,
		Video:       &VideoConfig{TargetCodec: "hevc", EncoderEngine: "nvenc"},
	}
	b, err := json.Marshal(flatOnly)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var back StartJobPayload
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if back.QualityTier != QualityTierArchivist || back.Video == nil || back.Video.QualityTier != QualityTierArchivist {
		t.Errorf("flat-only quality_tier lost when Video section is non-nil: %+v", back)
	}

	// Unset tier stays off the wire entirely (old agents must not see it).
	b, err = json.Marshal(StartJobPayload{JobID: "j1", InputPath: "/media/a.mkv", SpecVersion: 2})
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if strings.Contains(string(b), "quality_tier") {
		t.Errorf("unset quality_tier must be omitted from payload JSON: %s", b)
	}
}

func TestTruncateFileCompleteError(t *testing.T) {
	short := "ffmpeg failed: exit status 1"
	if got := TruncateFileCompleteError(short); got != short {
		t.Fatalf("short message modified: %q", got)
	}

	exact := strings.Repeat("a", MaxFileCompleteErrorLen)
	if got := TruncateFileCompleteError(exact); got != exact {
		t.Fatalf("message at cap modified: len=%d", len(got))
	}

	long := strings.Repeat("x", MaxFileCompleteErrorLen+5000)
	got := TruncateFileCompleteError(long)
	if len(got) <= MaxFileCompleteErrorLen {
		t.Fatalf("long message not capped: len=%d", len(got))
	}
	if !strings.HasPrefix(got, long[:100]) {
		t.Fatalf("truncated message lost its head: %q...", got[:50])
	}
	if !strings.Contains(got, "(truncated)") {
		t.Fatalf("truncated message missing marker: %q", got[len(got)-40:])
	}
}

func TestOutputFileStrategyAndActionAllowlistValidation(t *testing.T) {
	// Valid values should succeed
	validStrategies := []string{"alongside", "replace", "alternate"}
	validActions := []string{"keep", "trash", "delete", "archive"}

	for _, strat := range validStrategies {
		for _, act := range validActions {
			v2 := fmt.Sprintf(`{"spec_version": 2, "container": {"output_file_strategy": %q, "action_on_success": %q}}`, strat, act)
			var p2 StartJobPayload
			if err := json.Unmarshal([]byte(v2), &p2); err != nil {
				t.Errorf("v2 unmarshal failed for valid strat=%q act=%q: %v", strat, act, err)
			}
			v1 := fmt.Sprintf(`{"output_file_strategy": %q, "action_on_success": %q}`, strat, act)
			var p1 StartJobPayload
			if err := json.Unmarshal([]byte(v1), &p1); err != nil {
				t.Errorf("v1 unmarshal failed for valid strat=%q act=%q: %v", strat, act, err)
			}
		}
	}

	// Invalid strategy should fail
	invalidStrats := []string{"hacked", "default", "overwrite", "../escape"}
	for _, strat := range invalidStrats {
		v2 := fmt.Sprintf(`{"spec_version": 2, "container": {"output_file_strategy": %q}}`, strat)
		var p2 StartJobPayload
		if err := json.Unmarshal([]byte(v2), &p2); err == nil {
			t.Errorf("expected v2 unmarshal error for invalid strategy %q, got nil", strat)
		}
		v1 := fmt.Sprintf(`{"output_file_strategy": %q}`, strat)
		var p1 StartJobPayload
		if err := json.Unmarshal([]byte(v1), &p1); err == nil {
			t.Errorf("expected v1 unmarshal error for invalid strategy %q, got nil", strat)
		}
	}

	// Invalid action should fail
	invalidActions := []string{"rm_rf", "destroy", "wipe", "truncate"}
	for _, act := range invalidActions {
		v2 := fmt.Sprintf(`{"spec_version": 2, "container": {"action_on_success": %q}}`, act)
		var p2 StartJobPayload
		if err := json.Unmarshal([]byte(v2), &p2); err == nil {
			t.Errorf("expected v2 unmarshal error for invalid action %q, got nil", act)
		}
		v1 := fmt.Sprintf(`{"action_on_success": %q}`, act)
		var p1 StartJobPayload
		if err := json.Unmarshal([]byte(v1), &p1); err == nil {
			t.Errorf("expected v1 unmarshal error for invalid action %q, got nil", act)
		}
	}
}

func TestSanitizeTelemetryPath(t *testing.T) {
	cases := []struct {
		inputPath string
		relPath   string
		want      string
	}{
		{"", "", ""},
		{"/home/alice/media/movie.mkv", "", "movie.mkv"},
		{"/home/bob/downloads/tv/s01e01.mp4", "tv/s01e01.mp4", "tv/s01e01.mp4"},
		{"C:\\Users\\admin\\Videos\\sample.avi", "", "sample.avi"},
		{"/mnt/shares/media/movies/action/film.mkv", "movies/action/film.mkv", "movies/action/film.mkv"},
	}

	for _, tc := range cases {
		got := SanitizeTelemetryPath(tc.inputPath, tc.relPath)
		if got != tc.want {
			t.Errorf("SanitizeTelemetryPath(%q, %q) = %q, want %q", tc.inputPath, tc.relPath, got, tc.want)
		}
	}
}

func TestSetTelemetryRate(t *testing.T) {
	// Mode validation
	validModes := []string{TelemetryModeLive, TelemetryModeBackground, TelemetryModeIdle}
	for _, mode := range validModes {
		if !IsValidTelemetryMode(mode) {
			t.Errorf("expected mode %q to be valid", mode)
		}
	}

	invalidModes := []string{"", "FAST", "slow", "UNKNOWN", "stream"}
	for _, mode := range invalidModes {
		if IsValidTelemetryMode(mode) {
			t.Errorf("expected mode %q to be invalid", mode)
		}
	}

	// Serialization and Deserialization
	p := SetTelemetryRatePayload{
		Mode:   TelemetryModeLive,
		RateMS: 1000,
	}

	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("failed to marshal SetTelemetryRatePayload: %v", err)
	}

	var decoded SetTelemetryRatePayload
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("failed to unmarshal SetTelemetryRatePayload: %v", err)
	}

	if decoded.Mode != p.Mode || decoded.RateMS != p.RateMS {
		t.Errorf("decoded %+v does not match original %+v", decoded, p)
	}

	// Envelope with TypeSetTelemetryRate
	env := Envelope{
		Type:    TypeSetTelemetryRate,
		ID:      "rate-control-1",
		TS:      time.Now().Truncate(time.Second),
		Payload: b,
	}
	envBytes, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("failed to marshal envelope: %v", err)
	}

	var envDecoded Envelope
	if err := json.Unmarshal(envBytes, &envDecoded); err != nil {
		t.Fatalf("failed to unmarshal envelope: %v", err)
	}
	if envDecoded.Type != TypeSetTelemetryRate {
		t.Errorf("expected envelope type %q, got %q", TypeSetTelemetryRate, envDecoded.Type)
	}

	// Depth limit validation
	deep := strings.Repeat(`{"mode":`, 70) + `"LIVE"` + strings.Repeat(`}`, 70)
	var deepPayload SetTelemetryRatePayload
	if err := UnmarshalWithDepthLimit([]byte(deep), &deepPayload, DefaultMaxJSONDepth); err == nil {
		t.Errorf("expected depth limit error for deeply nested JSON, got nil")
	}

	// UnmarshalJSON validates Mode
	invalidJSON := `{"mode":"INVALID","rate_ms":5000}`
	var invalidPayload SetTelemetryRatePayload
	if err := json.Unmarshal([]byte(invalidJSON), &invalidPayload); err == nil {
		t.Errorf("expected unmarshal error for invalid mode, got nil")
	}
}

func TestScrubCommandArgs(t *testing.T) {
	scrubber := PathScrubber{
		MediaDir:      "/mnt/storage/media",
		ScratchDir:    "/home/alice/.local/state/slimmedia/scratch",
		QuarantineDir: "/home/alice/.local/state/slimmedia/quarantine",
		HomeDir:       "/home/alice",
		InputPath:     "/mnt/storage/media/movies/action/film.mkv",
		RelativePath:  "movies/action/film.mkv",
	}

	argv := []string{
		"-y",
		"-v", "error",
		"-i", "/mnt/storage/media/movies/action/film.mkv",
		"-c:v", "libx265",
		"-vf", "subtitles=/home/alice/.local/state/slimmedia/scratch/job1/sub.srt",
		"/home/alice/.local/state/slimmedia/scratch/job1/chunk-000.mkv",
	}

	scrubbed := ScrubCommandArgs(argv, scrubber)

	for _, arg := range scrubbed {
		if strings.Contains(arg, "/home/alice") {
			t.Errorf("scrubbed arg %q leaked home directory", arg)
		}
		if strings.Contains(arg, "/mnt/storage/media") {
			t.Errorf("scrubbed arg %q leaked media directory", arg)
		}
	}

	// Verify specific replacements
	if scrubbed[4] != "<MEDIA_ROOT>/movies/action/film.mkv" {
		t.Errorf("input arg = %q, want <MEDIA_ROOT>/movies/action/film.mkv", scrubbed[4])
	}
	if scrubbed[8] != "subtitles=<SCRATCH_DIR>/job1/sub.srt" {
		t.Errorf("filter arg = %q, want subtitles=<SCRATCH_DIR>/job1/sub.srt", scrubbed[8])
	}
	if scrubbed[9] != "<SCRATCH_DIR>/job1/chunk-000.mkv" {
		t.Errorf("output arg = %q, want <SCRATCH_DIR>/job1/chunk-000.mkv", scrubbed[9])
	}

	// Test StripMediaRoot option
	scrubberStrip := scrubber
	scrubberStrip.StripMediaRoot = true
	scrubbedStrip := ScrubCommandArgs([]string{"-i", "/mnt/storage/media/movies/action/film.mkv"}, scrubberStrip)
	if scrubbedStrip[1] != "movies/action/film.mkv" {
		t.Errorf("stripped media root = %q, want movies/action/film.mkv", scrubbedStrip[1])
	}

	// Test quarantine path scrubbing
	quarantineArgs := ScrubCommandArgs([]string{"/home/alice/.local/state/slimmedia/quarantine/film.mkv"}, scrubber)
	if quarantineArgs[0] != "<QUARANTINE_DIR>/film.mkv" {
		t.Errorf("quarantine arg = %q, want <QUARANTINE_DIR>/film.mkv", quarantineArgs[0])
	}

	// Test zero-config regex fallbacks
	zeroConfig := ScrubCommandArgs([]string{
		"-i", "/home/bob/videos/movie.mkv",
		"-o", "/tmp/scratch/movie.tmp.mkv",
		"C:\\Users\\charlie\\AppData\\Local\\Temp\\scratch\\test.mkv",
	})
	if strings.Contains(zeroConfig[1], "/home/bob") {
		t.Errorf("zero-config leaked /home/bob: %s", zeroConfig[1])
	}
	if !strings.Contains(zeroConfig[3], "<SCRATCH_DIR>") {
		t.Errorf("zero-config scratch output = %q, want containing <SCRATCH_DIR>", zeroConfig[3])
	}
	if !strings.Contains(zeroConfig[4], "<SCRATCH_DIR>") || strings.Contains(zeroConfig[4], "charlie") {
		t.Errorf("zero-config windows path = %q, want containing <SCRATCH_DIR> without username", zeroConfig[4])
	}
}

func TestSanitizeErrorMessage(t *testing.T) {
	scrubber := PathScrubber{
		MediaDir:      "/mnt/storage/media",
		ScratchDir:    "/home/alice/.local/state/slimmedia/scratch",
		QuarantineDir: "/home/alice/.local/state/slimmedia/quarantine",
		HomeDir:       "/home/alice",
	}

	errStderr := "ffmpeg error: [matroska,webm @ 0x5555] /home/alice/.local/state/slimmedia/scratch/job1/chunk-000.mkv: Invalid data found when processing input"
	sanitized := SanitizeErrorMessage(errStderr, scrubber)
	if strings.Contains(sanitized, "/home/alice") {
		t.Errorf("sanitized error leaked home path: %s", sanitized)
	}
	if !strings.Contains(sanitized, "<SCRATCH_DIR>/job1/chunk-000.mkv") {
		t.Errorf("sanitized error = %q, want containing <SCRATCH_DIR>/job1/chunk-000.mkv", sanitized)
	}
	if !strings.Contains(sanitized, "Invalid data found when processing input") {
		t.Errorf("sanitized error lost diagnostic message: %s", sanitized)
	}

	// Test rejection error with home path
	rejectErr := `job input rejected: "/home/bob/Downloads/movie.mkv" is outside the scanned media library`
	sanitizedReject := SanitizeErrorMessage(rejectErr)
	if strings.Contains(sanitizedReject, "/home/bob") {
		t.Errorf("sanitized rejection leaked /home/bob: %s", sanitizedReject)
	}
	if !strings.Contains(sanitizedReject, "outside the scanned media library") {
		t.Errorf("sanitized rejection lost diagnostic reason: %s", sanitizedReject)
	}

	// Test ErrFileNotFound and ErrLibraryNotMounted preservation
	fileNotFound := "ErrFileNotFound: /mnt/storage/media/movies/ghost.mkv not found"
	sanitizedNotFound := SanitizeErrorMessage(fileNotFound, scrubber)
	if !strings.Contains(sanitizedNotFound, "ErrFileNotFound") {
		t.Errorf("sanitized error lost ErrFileNotFound marker: %s", sanitizedNotFound)
	}
	if strings.Contains(sanitizedNotFound, "/mnt/storage/media") {
		t.Errorf("sanitized error leaked media root: %s", sanitizedNotFound)
	}
}

func TestSanitizeFileCompleteError_MasksPriorToTruncation(t *testing.T) {
	// Construct a long message (>4000 chars) that starts with sensitive path info
	pathPrefix := "/home/alice/secret_project/.local/state/slimmedia/scratch/job123/chunk-999.mkv: "
	longDiagnostic := strings.Repeat("A", 4500)
	msg := pathPrefix + longDiagnostic

	sanitized := TruncateFileCompleteError(msg)
	if strings.Contains(sanitized, "/home/alice") {
		t.Errorf("TruncateFileCompleteError leaked home directory: %s", sanitized[:100])
	}
	if !strings.Contains(sanitized, "<SCRATCH_DIR>") {
		t.Errorf("TruncateFileCompleteError missing <SCRATCH_DIR>: %s", sanitized[:100])
	}
	if len(sanitized) > MaxFileCompleteErrorLen+len("… (truncated)") {
		t.Errorf("sanitized error len = %d exceeds max cap %d", len(sanitized), MaxFileCompleteErrorLen)
	}
	if !strings.HasSuffix(sanitized, "… (truncated)") {
		t.Errorf("expected truncated suffix, got: %s", sanitized[len(sanitized)-20:])
	}
}

func TestSerializationScrubbing(t *testing.T) {
	// Test FileCompletePayload serialization
	complete := FileCompletePayload{
		JobID:        "test-job-123",
		ErrorMessage: "error in /home/alice/.local/state/slimmedia/scratch/job1/out.mkv: failed to write",
		FfmpegCommands: []FfmpegCommandRecord{
			{
				Phase: "encode",
				Argv: []string{
					"-i", "/home/alice/media/movie.mkv",
					"-c:v", "libx265",
					"/home/alice/.local/state/slimmedia/scratch/job1.tmp.mkv",
				},
				Count: 1,
			},
		},
	}

	data, err := json.Marshal(complete)
	if err != nil {
		t.Fatalf("json.Marshal(FileCompletePayload) failed: %v", err)
	}
	var unmarshaledComplete FileCompletePayload
	if err := json.Unmarshal(data, &unmarshaledComplete); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	if strings.Contains(unmarshaledComplete.ErrorMessage, "/home/alice") {
		t.Errorf("serialized FileCompletePayload leaked /home/alice: %s", unmarshaledComplete.ErrorMessage)
	}
	if !strings.Contains(unmarshaledComplete.ErrorMessage, "<SCRATCH_DIR>") {
		t.Errorf("serialized FileCompletePayload missing <SCRATCH_DIR>: %s", unmarshaledComplete.ErrorMessage)
	}
	for _, cmd := range unmarshaledComplete.FfmpegCommands {
		for _, arg := range cmd.Argv {
			if strings.Contains(arg, "/home/alice") {
				t.Errorf("serialized FileCompletePayload command arg leaked /home/alice: %s", arg)
			}
		}
	}

	// Test JobTracePayload serialization
	trace := JobTracePayload{
		JobID: "trace-job-456",
		Commands: []FfmpegCommandRecord{
			{
				Phase: "concat",
				Argv: []string{
					"-f", "concat",
					"-i", "/tmp/scratch/concat.txt",
					"/home/bob/quarantine/backup.mkv",
				},
			},
		},
	}

	traceData, err := json.Marshal(trace)
	if err != nil {
		t.Fatalf("json.Marshal(JobTracePayload) failed: %v", err)
	}
	var unmarshaledTrace JobTracePayload
	if err := json.Unmarshal(traceData, &unmarshaledTrace); err != nil {
		t.Fatalf("json.Unmarshal trace failed: %v", err)
	}
	for _, cmd := range unmarshaledTrace.Commands {
		for _, arg := range cmd.Argv {
			if strings.Contains(arg, "/home/bob") || strings.Contains(arg, "/tmp/scratch") {
				t.Errorf("serialized JobTracePayload leaked local paths: %s", arg)
			}
		}
	}
}

func TestLibraryPathStatusPayload(t *testing.T) {
	validPayload := LibraryPathStatusPayload{
		LibraryID: "lib-123",
		Path:      "/mnt/media/movies",
		Status:    "mounted",
		Valid:     true,
	}
	data, err := json.Marshal(validPayload)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	var unmarshaledValid LibraryPathStatusPayload
	if err := json.Unmarshal(data, &unmarshaledValid); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	if unmarshaledValid.LibraryID != "lib-123" || !unmarshaledValid.Valid || unmarshaledValid.Status != "mounted" {
		t.Errorf("unexpected unmarshaledValid: %+v", unmarshaledValid)
	}

	errPayload := LibraryPathStatusPayload{
		LibraryID: "lib-456",
		Path:      "/mnt/nonexistent",
		Status:    "error",
		Valid:     false,
		Reason:    "Directory does not exist",
		Error:     "Directory does not exist",
	}
	data, err = json.Marshal(errPayload)
	if err != nil {
		t.Fatalf("json.Marshal error payload failed: %v", err)
	}
	var unmarshaledErr LibraryPathStatusPayload
	if err := json.Unmarshal(data, &unmarshaledErr); err != nil {
		t.Fatalf("json.Unmarshal error payload failed: %v", err)
	}
	if unmarshaledErr.LibraryID != "lib-456" || unmarshaledErr.Valid || unmarshaledErr.Reason != "Directory does not exist" {
		t.Errorf("unexpected unmarshaledErr: %+v", unmarshaledErr)
	}
}

