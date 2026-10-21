package jobspec

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/protocol"
)

func TestFromWireMapsEveryTuningField(t *testing.T) {
	p := protocol.StartJobPayload{
		JobID:            "j1",
		InputPath:        "/media/a.mkv",
		TargetCodec:      "hevc",
		SpecVersion:      protocol.CurrentJobSpecVersion,
		Container:        "mkv",
		CRF:              23,
		PresetSpeed:      "medium",
		AudioCodec:       "copy",
		EncoderEngine:    "nvenc",
		QualityTarget:    22,
		AudioHandling:    "copy_all",
		SubtitleHandling: "strip_all",
		ColorDepth:       "10bit",
		FilmGrainSao:     true,
		AqMode:           3,
		ContentTuning:    "film",
		CustomFlags:      "keyint=60",
		StreamHygienePolicy: protocol.StreamHygienePolicy{
			PreferredAudioLanguages:    []string{"eng", "jpn"},
			AudioLanguageMode:          "whitelist",
			SubtitleFormatPolicy:       "text_only",
			ForcedSubtitlePolicy:       "keep_always",
			PreferredSubtitleLanguages: []string{"eng"},
			SubtitleLanguageMode:       "whitelist",
			StripMetadata:              true,
		},
		OutputFileStrategy: "alongside",
		ActionOnSuccess:    "keep",
	}

	got, err := FromWire(p)
	if err != nil {
		t.Fatalf("FromWire failed: %v", err)
	}
	want := JobSpec{
		Version:                    protocol.CurrentJobSpecVersion,
		CRF:                        23,
		PresetSpeed:                "medium",
		AudioCodec:                 "copy",
		Container:                  "mkv",
		EncoderEngine:              "nvenc",
		QualityTarget:              22,
		AudioHandling:              "copy_all",
		SubtitleHandling:           "strip_all",
		ColorDepth:                 "10bit",
		FilmGrainSao:               true,
		AqMode:                     3,
		ContentTuning:              "film",
		CustomFlags:                "keyint=60",
		PreferredAudioLanguages:    []string{"eng", "jpn"},
		AudioLanguageMode:          "whitelist",
		SubtitleFormatPolicy:       "text_only",
		ForcedSubtitlePolicy:       "keep_always",
		PreferredSubtitleLanguages: []string{"eng"},
		SubtitleLanguageMode:       "whitelist",
		StripMetadata:              true,
		OutputFileStrategy:         "alongside",
		ActionOnSuccess:            "keep",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FromWire mismatch:\n got: %+v\nwant: %+v", got, want)
	}
}

func TestFromWireV2StructuredPayload(t *testing.T) {
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

	var p protocol.StartJobPayload
	if err := json.Unmarshal([]byte(v2JSON), &p); err != nil {
		t.Fatalf("unmarshal v2 payload: %v", err)
	}

	got, err := FromWire(p)
	if err != nil {
		t.Fatalf("FromWire failed: %v", err)
	}
	want := JobSpec{
		Version:                    2,
		CRF:                        20,
		PresetSpeed:                "medium",
		AudioCodec:                 "aac",
		Container:                  "mkv",
		EncoderEngine:              "nvenc",
		QualityTarget:              20,
		AudioHandling:              "transcode_aac",
		SubtitleHandling:           "keep_all",
		ColorDepth:                 "10bit",
		FilmGrainSao:               false,
		AqMode:                     3,
		ContentTuning:              "film",
		CustomFlags:                "-tune film -rc-lookahead 40",
		PreferredAudioLanguages:    []string{"eng", "jpn"},
		AudioLanguageMode:          "whitelist",
		SubtitleFormatPolicy:       "keep_all",
		ForcedSubtitlePolicy:       "keep_forced",
		PreferredSubtitleLanguages: []string{"eng"},
		SubtitleLanguageMode:       "whitelist",
		StripMetadata:              true,
		OutputFileStrategy:         "alongside",
		ActionOnSuccess:            "keep",
		PostTranscodeVerification:  true,
		RelativePath:               "sample.mkv",
		LocalPath:                  "/mnt/storage/media",
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("FromWire v2 mismatch:\n got: %+v\nwant: %+v", got, want)
	}
}

func TestFromWireLegacyPayloadLeavesSpecZeroValued(t *testing.T) {
	var p protocol.StartJobPayload
	legacy := `{"job_id":"j1","input_path":"/media/a.mkv","crf":23,"preset_speed":"medium"}`
	if err := json.Unmarshal([]byte(legacy), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	spec, err := FromWire(p)
	if err != nil {
		t.Fatalf("FromWire failed: %v", err)
	}
	if !spec.Legacy() {
		t.Errorf("pre-tuning payload must produce a legacy spec, got %+v", spec)
	}
	if spec.CRF != 23 || spec.PresetSpeed != "medium" {
		t.Errorf("base fields must still map on legacy payloads: %+v", spec)
	}
}

func TestFromWireQualityTier(t *testing.T) {
	// Structured v2 payload carries the tier in the Video section.
	var p protocol.StartJobPayload
	v2JSON := `{
		"job_id": "j1",
		"input_path": "/media/a.mkv",
		"spec_version": 2,
		"video": {"target_codec": "hevc", "encoder_engine": "nvenc", "quality_tier": "archivist"},
		"container": {"format": "mkv", "output_file_strategy": "alongside", "action_on_success": "keep"}
	}`
	if err := json.Unmarshal([]byte(v2JSON), &p); err != nil {
		t.Fatalf("unmarshal v2 payload: %v", err)
	}
	spec, err := FromWire(p)
	if err != nil {
		t.Fatalf("FromWire failed: %v", err)
	}
	if spec.QualityTier != protocol.QualityTierArchivist {
		t.Errorf("expected quality tier %q from video section, got %q", protocol.QualityTierArchivist, spec.QualityTier)
	}

	// Flat top-level field wins when both are populated (post-unmarshal
	// callers may set only one).
	p2 := protocol.StartJobPayload{
		JobID:       "j2",
		InputPath:   "/media/b.mkv",
		SpecVersion: protocol.CurrentJobSpecVersion,
		QualityTier: protocol.QualityTierArchivist,
		Video:       &protocol.VideoConfig{QualityTier: ""},
	}
	spec2, err := FromWire(p2)
	if err != nil {
		t.Fatalf("FromWire failed: %v", err)
	}
	if spec2.QualityTier != protocol.QualityTierArchivist {
		t.Errorf("expected top-level quality tier to win, got %q", spec2.QualityTier)
	}

	// No tier anywhere: plain preset behavior.
	spec3, err := FromWire(protocol.StartJobPayload{JobID: "j3", InputPath: "/media/c.mkv"})
	if err != nil {
		t.Fatalf("FromWire failed: %v", err)
	}
	if spec3.QualityTier != "" {
		t.Errorf("expected empty quality tier for plain payloads, got %q", spec3.QualityTier)
	}
}

func TestLegacyReportsOnVersionOnly(t *testing.T) {
	if !(JobSpec{}).Legacy() {
		t.Error("zero-value spec (no wire version) must be legacy")
	}
	if (JobSpec{Version: protocol.CurrentJobSpecVersion}).Legacy() {
		t.Error("current-version spec must not be legacy")
	}
}

func TestFromWireHygieneFromStreamHygienePolicy(t *testing.T) {
	p := protocol.StartJobPayload{
		JobID:       "j2",
		InputPath:   "/media/b.mkv",
		SpecVersion: protocol.CurrentJobSpecVersion,
		StreamHygienePolicy: protocol.StreamHygienePolicy{
			PreferredAudioLanguages:    []string{"eng", "jpn"},
			AudioLanguageMode:          "whitelist",
			SubtitleFormatPolicy:       "text_only",
			ForcedSubtitlePolicy:       "keep_always",
			PreferredSubtitleLanguages: []string{"eng", "und"},
			SubtitleLanguageMode:       "whitelist",
			StripMetadata:              true,
		},
	}
	spec, err := FromWire(p)
	if err != nil {
		t.Fatalf("FromWire failed: %v", err)
	}
	if !spec.StripMetadata {
		t.Errorf("expected StripMetadata true")
	}
	if !reflect.DeepEqual(spec.PreferredAudioLanguages, []string{"eng", "jpn"}) {
		t.Errorf("expected preferred audio [eng jpn], got %v", spec.PreferredAudioLanguages)
	}
	if spec.AudioLanguageMode != "whitelist" {
		t.Errorf("expected audio mode whitelist, got %s", spec.AudioLanguageMode)
	}
	if spec.SubtitleFormatPolicy != "text_only" {
		t.Errorf("expected subtitle format policy text_only, got %s", spec.SubtitleFormatPolicy)
	}
	if spec.ForcedSubtitlePolicy != "keep_always" {
		t.Errorf("expected forced subtitle policy keep_always, got %s", spec.ForcedSubtitlePolicy)
	}
	if !reflect.DeepEqual(spec.PreferredSubtitleLanguages, []string{"eng", "und"}) {
		t.Errorf("expected preferred subtitles [eng und], got %v", spec.PreferredSubtitleLanguages)
	}
	if spec.SubtitleLanguageMode != "whitelist" {
		t.Errorf("expected subtitle mode whitelist, got %s", spec.SubtitleLanguageMode)
	}
}

func TestFromWireRejectsInvalidStrategyAndAction(t *testing.T) {
	invalidStrategies := []string{"hacked", "default", "overwrite", "../source", "foo"}
	for _, strat := range invalidStrategies {
		p := protocol.StartJobPayload{
			JobID:              "j_bad_strat",
			OutputFileStrategy: strat,
		}
		if _, err := FromWire(p); err == nil {
			t.Errorf("expected FromWire to reject invalid OutputFileStrategy %q", strat)
		}
	}

	invalidActions := []string{"rm_rf", "wipe", "destroy"}
	for _, act := range invalidActions {
		p := protocol.StartJobPayload{
			JobID:           "j_bad_act",
			ActionOnSuccess: act,
		}
		if _, err := FromWire(p); err == nil {
			t.Errorf("expected FromWire to reject invalid ActionOnSuccess %q", act)
		}
	}
}

func TestChunkingOverride(t *testing.T) {
	cases := []struct {
		name        string
		chunking    string
		wantChunked bool
		wantOK      bool
	}{
		{"auto empty", "", false, false},
		{"auto explicit", "auto", false, false},
		{"force chunked", "chunked", true, true},
		{"force single pass", "single_pass", false, true},
		{"unknown value falls back to auto", "banana", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := JobSpec{Chunking: tc.chunking}
			gotChunked, gotOK := s.ChunkingOverride()
			if gotChunked != tc.wantChunked || gotOK != tc.wantOK {
				t.Errorf("ChunkingOverride(%q) = (%v, %v), want (%v, %v)",
					tc.chunking, gotChunked, gotOK, tc.wantChunked, tc.wantOK)
			}
		})
	}
}

func TestFromWireLeavesChunkingAuto(t *testing.T) {
	// The override is spec-level only (tests, future presets): the wire
	// payload has no field for it, so every wire-built spec stays "auto".
	spec, err := FromWire(protocol.StartJobPayload{
		JobID:         "j1",
		TargetCodec:   "hevc",
		SpecVersion:   protocol.CurrentJobSpecVersion,
		EncoderEngine: "nvenc",
	})
	if err != nil {
		t.Fatalf("FromWire: %v", err)
	}
	if _, ok := spec.ChunkingOverride(); ok {
		t.Errorf("FromWire-built spec must stay chunking-auto, got override %q", spec.Chunking)
	}
}

func TestFromWireVendorTuning(t *testing.T) {
	mk := func(p protocol.StartJobPayload) JobSpec {
		t.Helper()
		s, err := FromWire(p)
		if err != nil {
			t.Fatalf("FromWire failed: %v", err)
		}
		return s
	}

	// Flat field populates the spec.
	spatial := false
	flat := mk(protocol.StartJobPayload{
		JobID:        "j1",
		InputPath:    "/media/a.mkv",
		SpecVersion:  protocol.CurrentJobSpecVersion,
		VendorTuning: &protocol.VendorTuning{Tune: strPtr("ll"), SpatialAQ: &spatial},
	})
	if flat.VendorTuning == nil || flat.VendorTuning.Tune == nil || *flat.VendorTuning.Tune != "ll" {
		t.Errorf("flat vendor_tuning not mapped: %+v", flat.VendorTuning)
	}
	if flat.VendorTuning.SpatialAQ == nil || *flat.VendorTuning.SpatialAQ {
		t.Errorf("explicit false spatial_aq not mapped: %+v", flat.VendorTuning)
	}

	// Structured Video section populates the spec when the flat field is nil.
	videoOnly := mk(protocol.StartJobPayload{
		JobID:       "j1",
		InputPath:   "/media/a.mkv",
		SpecVersion: protocol.CurrentJobSpecVersion,
		Video:       &protocol.VideoConfig{TargetCodec: "hevc", VendorTuning: &protocol.VendorTuning{Extbrc: boolPtr(true)}},
	})
	if videoOnly.VendorTuning == nil || videoOnly.VendorTuning.Extbrc == nil || !*videoOnly.VendorTuning.Extbrc {
		t.Errorf("video.vendor_tuning not mapped: %+v", videoOnly.VendorTuning)
	}

	// Absent and empty selections map to nil (agent defaults).
	if s := mk(protocol.StartJobPayload{JobID: "j1", InputPath: "/media/a.mkv", SpecVersion: protocol.CurrentJobSpecVersion}); s.VendorTuning != nil {
		t.Errorf("absent vendor_tuning must map to nil, got %+v", s.VendorTuning)
	}
	if s := mk(protocol.StartJobPayload{
		JobID: "j1", InputPath: "/media/a.mkv", SpecVersion: protocol.CurrentJobSpecVersion,
		VendorTuning: &protocol.VendorTuning{},
	}); s.VendorTuning != nil {
		t.Errorf("empty vendor_tuning must map to nil, got %+v", s.VendorTuning)
	}
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }
func intPtr(i int) *int       { return &i }

func TestFromWireVendorTuningRejectedWhenInvalid(t *testing.T) {
	bad := []protocol.StartJobPayload{
		{JobID: "j1", InputPath: "/media/a.mkv", SpecVersion: protocol.CurrentJobSpecVersion, VendorTuning: &protocol.VendorTuning{Tune: strPtr("bogus")}},
		{JobID: "j1", InputPath: "/media/a.mkv", SpecVersion: protocol.CurrentJobSpecVersion, VendorTuning: &protocol.VendorTuning{LookAheadDepth: intPtr(500)}},
		{JobID: "j1", InputPath: "/media/a.mkv", SpecVersion: protocol.CurrentJobSpecVersion, Video: &protocol.VideoConfig{TargetCodec: "hevc", VendorTuning: &protocol.VendorTuning{Multipass: strPtr("bogus")}}},
	}
	for i, p := range bad {
		if _, err := FromWire(p); err == nil {
			t.Errorf("case %d: invalid vendor_tuning must be rejected", i)
		}
	}
}
