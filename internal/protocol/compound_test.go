package protocol

import (
	"strings"
	"testing"
)

func TestParseCompoundParams(t *testing.T) {
	s := "aq-mode=2:no-sao:hdr10=1:master-display=G(1,2)B(3,4)R(5,6)WP(7,8)L(9,10):max-cll=1000,400"
	params := ParseCompoundParams(s)
	if len(params) != 5 {
		t.Fatalf("expected 5 params, got %d: %+v", len(params), params)
	}

	if params[0].Key != "aq-mode" || params[0].Val != "2" || !params[0].HasVal {
		t.Errorf("unexpected param 0: %+v", params[0])
	}
	if params[1].Key != "no-sao" || params[1].Val != "" || params[1].HasVal {
		t.Errorf("unexpected param 1: %+v", params[1])
	}
	if params[2].Key != "hdr10" || params[2].Val != "1" || !params[2].HasVal {
		t.Errorf("unexpected param 2: %+v", params[2])
	}
	if params[3].Key != "master-display" || params[3].Val != "G(1,2)B(3,4)R(5,6)WP(7,8)L(9,10)" {
		t.Errorf("unexpected param 3: %+v", params[3])
	}
	if params[4].Key != "max-cll" || params[4].Val != "1000,400" {
		t.Errorf("unexpected param 4: %+v", params[4])
	}
}

func TestMergeCompoundParams_HDR10Protected(t *testing.T) {
	base := "aq-mode=2:hdr10=1:hdr10_opt=1:master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,5):max-cll=1000,400:sao=0"
	custom := "aq-mode=1:hdr10=0:hdr10_opt=0:master-display=bogus:max-cll=0,0:sao=1:crf=18:keyint=60"

	merged := MergeCompoundParams(base, custom, HDR10ProtectedKeys)

	// Protected keys must be strictly preserved
	if !strings.Contains(merged, "aq-mode=2") {
		t.Errorf("expected aq-mode=2 preserved in %q", merged)
	}
	if strings.Contains(merged, "aq-mode=1") {
		t.Errorf("aq-mode=1 should not overwrite aq-mode=2 in %q", merged)
	}
	if !strings.Contains(merged, "hdr10=1") || strings.Contains(merged, "hdr10=0") {
		t.Errorf("expected hdr10=1 preserved in %q", merged)
	}
	if !strings.Contains(merged, "hdr10_opt=1") || strings.Contains(merged, "hdr10_opt=0") {
		t.Errorf("expected hdr10_opt=1 preserved in %q", merged)
	}
	if !strings.Contains(merged, "master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,5)") {
		t.Errorf("expected original master-display preserved in %q", merged)
	}
	if strings.Contains(merged, "master-display=bogus") {
		t.Errorf("master-display=bogus should not overwrite in %q", merged)
	}
	if !strings.Contains(merged, "max-cll=1000,400") || strings.Contains(merged, "max-cll=0,0") {
		t.Errorf("expected max-cll=1000,400 preserved in %q", merged)
	}

	// Non-protected keys must be merged / overwritten
	if !strings.Contains(merged, "sao=1") || strings.Contains(merged, "sao=0") {
		t.Errorf("expected sao=1 overwritten in %q", merged)
	}
	if !strings.Contains(merged, "crf=18") {
		t.Errorf("expected crf=18 appended in %q", merged)
	}
	if !strings.Contains(merged, "keyint=60") {
		t.Errorf("expected keyint=60 appended in %q", merged)
	}
}

func TestMergeCompoundParams_EmptyAndNewKeys(t *testing.T) {
	if got := MergeCompoundParams("", "tune=0:film-grain=10", HDR10ProtectedKeys); got != "tune=0:film-grain=10" {
		t.Errorf("expected custom flags returned when base empty, got %q", got)
	}
	if got := MergeCompoundParams("tune=0", "", HDR10ProtectedKeys); got != "tune=0" {
		t.Errorf("expected base flags returned when custom empty, got %q", got)
	}

	// If protected key was NOT in base, custom CAN set it
	base := "tune=0"
	custom := "master-display=G(1,2):aq-mode=3"
	merged := MergeCompoundParams(base, custom, HDR10ProtectedKeys)
	if !strings.Contains(merged, "master-display=G(1,2)") || !strings.Contains(merged, "aq-mode=3") {
		t.Errorf("expected new keys to be added when not in base: %q", merged)
	}
}

func TestIsManagedPresetFlag(t *testing.T) {
	managed := []string{"-crf", "-crf=20", "-preset", "-preset=slow", "-c:v", "-vcodec", "-c:a", "-acodec", "-b:v", "-b:a", "-ab", "-pix_fmt", "-profile:v"}
	for _, f := range managed {
		if !IsManagedPresetFlag(f) {
			t.Errorf("expected %q to be recognized as managed preset flag", f)
		}
	}

	unmanaged := []string{"-tune", "-rc-lookahead", "-bf", "-x265-params", "-svtav1-params", "-g"}
	for _, f := range unmanaged {
		if IsManagedPresetFlag(f) {
			t.Errorf("expected %q to NOT be recognized as managed preset flag", f)
		}
	}
}
