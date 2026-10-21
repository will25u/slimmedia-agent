package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateVendorTuning(t *testing.T) {
	if err := ValidateVendorTuning(nil); err != nil {
		t.Errorf("nil tuning must validate: %v", err)
	}
	if err := ValidateVendorTuning(&VendorTuning{}); err != nil {
		t.Errorf("empty tuning must validate: %v", err)
	}
	good := &VendorTuning{
		Tune:           strptr("hq"),
		Multipass:      strptr("fullres"),
		BRefMode:       strptr("middle"),
		SpatialAQ:      boolptr(true),
		TemporalAQ:     boolptr(false),
		Extbrc:         boolptr(true),
		LookAhead:      boolptr(false),
		LookAheadDepth: intptr(40),
		BStrategy:      boolptr(true),
		AdaptiveB:      boolptr(false),
		AdaptiveI:      boolptr(true),
	}
	if err := ValidateVendorTuning(good); err != nil {
		t.Errorf("valid selection rejected: %v", err)
	}
	for name, vt := range map[string]*VendorTuning{
		"tune":           {Tune: strptr("bogus")},
		"multipass":      {Multipass: strptr("bogus")},
		"b_ref_mode":     {BRefMode: strptr("bogus")},
		"depth too low":  {LookAheadDepth: intptr(0)},
		"depth too high": {LookAheadDepth: intptr(101)},
	} {
		if err := ValidateVendorTuning(vt); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

func TestStartJobPayloadVendorTuningRoundTrip(t *testing.T) {
	// v2 structured wire carries the selection in the video section.
	v2 := `{
		"job_id": "j1",
		"input_path": "/media/a.mkv",
		"spec_version": 2,
		"video": {"target_codec": "hevc", "encoder_engine": "nvenc", "vendor_tuning": {"tune": "ll", "spatial_aq": false}},
		"container": {"format": "mkv", "output_file_strategy": "alongside", "action_on_success": "keep"}
	}`
	var p StartJobPayload
	if err := json.Unmarshal([]byte(v2), &p); err != nil {
		t.Fatalf("v2 vendor_tuning payload failed to unmarshal: %v", err)
	}
	if p.VendorTuning == nil || p.VendorTuning.Tune == nil || *p.VendorTuning.Tune != "ll" {
		t.Errorf("flat vendor_tuning not decoded: %+v", p.VendorTuning)
	}
	if p.VendorTuning.SpatialAQ == nil || *p.VendorTuning.SpatialAQ {
		t.Errorf("explicit false spatial_aq not decoded: %+v", p.VendorTuning)
	}
	if p.Video == nil || p.Video.VendorTuning == nil {
		t.Errorf("video.vendor_tuning not decoded: %+v", p.Video)
	}

	marshaled, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if !strings.Contains(string(marshaled), `"vendor_tuning":{"tune":"ll","spatial_aq":false}`) {
		t.Errorf("marshaled v2 must retain vendor_tuning: %s", marshaled)
	}

	var rt StartJobPayload
	if err := json.Unmarshal(marshaled, &rt); err != nil {
		t.Fatalf("round-trip unmarshal failed: %v", err)
	}
	if rt.VendorTuning == nil || rt.VendorTuning.Tune == nil || *rt.VendorTuning.Tune != "ll" {
		t.Errorf("vendor_tuning lost in round trip: %+v", rt.VendorTuning)
	}

	// v1 flat wire carries the selection top-level.
	v1 := `{"job_id":"j1","input_path":"/media/a.mkv","spec_version":1,"encoder_engine":"nvenc","vendor_tuning":{"look_ahead_depth":60}}`
	var flat StartJobPayload
	if err := json.Unmarshal([]byte(v1), &flat); err != nil {
		t.Fatalf("v1 vendor_tuning payload failed to unmarshal: %v", err)
	}
	if flat.VendorTuning == nil || flat.VendorTuning.LookAheadDepth == nil || *flat.VendorTuning.LookAheadDepth != 60 {
		t.Errorf("v1 flat vendor_tuning not decoded: %+v", flat.VendorTuning)
	}

	// Flat-only population with a non-nil Video section must not lose the
	// selection on the wire (same backfill contract as quality_tier).
	flatOnly := StartJobPayload{
		JobID:        "j-flat",
		InputPath:    "/media/a.mkv",
		SpecVersion:  2,
		VendorTuning: &VendorTuning{Tune: strptr("ll")},
		Video:        &VideoConfig{TargetCodec: "hevc", EncoderEngine: "nvenc"},
	}
	b, err := json.Marshal(flatOnly)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var back StartJobPayload
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if back.VendorTuning == nil || back.VendorTuning.Tune == nil || *back.VendorTuning.Tune != "ll" {
		t.Errorf("flat-only vendor_tuning lost when Video section is non-nil: %+v", back.VendorTuning)
	}

	// Unset selection stays off the wire entirely.
	b, err = json.Marshal(StartJobPayload{JobID: "j1", InputPath: "/media/a.mkv", SpecVersion: 2})
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if strings.Contains(string(b), "vendor_tuning") {
		t.Errorf("unset vendor_tuning must be omitted from payload JSON: %s", b)
	}
}

func strptr(s string) *string { return &s }
func boolptr(b bool) *bool    { return &b }
func intptr(i int) *int       { return &i }
