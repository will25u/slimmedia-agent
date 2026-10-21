package protocol

import (
	"strings"
	"testing"
)

func TestSanitizeCustomFlags_Valid(t *testing.T) {
	validCases := []string{
		"",
		"   ",
		"-bf 4 -rc-lookahead 30",
		"-tune hq -spatial-aq 1",
		`-metadata comment="a b"`,
		"-preset=medium -b:v=2M",
		"-crf 22 -preset slow",
		"-an -c:v libx264",
		"-r 24000/1001",
		"-aspect 16/9",
		"-x265-params keyint=60:bframes=4",
		"-x264-params ref=4:me=hex",
		"keyint=60",
		"keyint=60:bframes=4:no-sao",
	}

	for _, tc := range validCases {
		t.Run(tc, func(t *testing.T) {
			_, err := SanitizeCustomFlags(tc)
			if err != nil {
				t.Fatalf("expected valid flags for %q, got: %v", tc, err)
			}
		})
	}
}

func TestSanitizeCustomFlags_RejectsBareTokens(t *testing.T) {
	cases := []struct {
		flags       string
		errContains string
	}{
		{"-preset ultrafast /tmp/extra.mkv", "bare non-flag token"},
		{"/tmp/extra.mkv -preset ultrafast", "bare non-flag token"},
		{"-bf 4 -rc-lookahead 30 out.mkv", "bare non-flag token"},
		{"-an /tmp/extra.mkv", "bare non-flag token"},
		{"-vn output.mp4", "bare non-flag token"},
		{"-sn output.srt", "bare non-flag token"},
		{"-bf 4 foo bar", "bare non-flag token"},
	}

	for _, tc := range cases {
		t.Run(tc.flags, func(t *testing.T) {
			_, err := SanitizeCustomFlags(tc.flags)
			if err == nil {
				t.Fatalf("expected error for %q, got nil", tc.flags)
			}
			if !strings.Contains(err.Error(), tc.errContains) {
				t.Fatalf("expected error containing %q, got: %v", tc.errContains, err)
			}
		})
	}
}

func TestSanitizeCustomFlags_RejectsForbiddenAndOutputTargets(t *testing.T) {
	cases := []struct {
		flags       string
		errContains string
	}{
		{"-f null /dev/null", "forbidden token"},
		{"-map 0:v", "forbidden token"},
		{"-map_metadata 0", "forbidden token"},
		{"-i /tmp/in.mkv", "forbidden token"},
		{"-nostats", "forbidden token"},
		{"-progress pipe:1", "forbidden token"},
		{"-nostdin", "forbidden token"},
		{"-dump_attachment:t:0 /etc/shadow", "forbidden token"},
		{"-passlogfile /tmp/pwn", "forbidden token"},
		{"-vstats_file /tmp/pwn", "forbidden token"},
		{"-print_graphs_file /tmp/pwn", "forbidden token"},
		{"-tee out1|out2", "forbidden token"},
		{"-target vcd", "forbidden token"},
		{"-attach font.ttf", "forbidden token"},
		{"-filter_complex [0:v]scale[v]", "forbidden token"},
		{"-filter_complex_script /tmp/graph", "forbidden token"},
		{"-lavfi color", "forbidden token"},
		{"-vf subtitles=/etc/shadow", "forbidden token"},
		{"-vf scale=1280:720", "forbidden token"},
		{"-af loudnorm", "forbidden token"},
		{"-filter:v scale=1280:720", "forbidden token"},
		{"-filter:a loudnorm", "forbidden token"},
		{"-filter scale=1280:720", "forbidden token"},
		{"-filter_script /tmp/graph", "forbidden token"},
		{"-filter_script:v /tmp/graph", "forbidden token"},
		{"-filter_script:a /tmp/graph", "forbidden token"},
		{"-report", "forbidden token"},
		{"-y /tmp/out.mkv", "forbidden token"},
		{"-n /tmp/out.mkv", "forbidden token"},
		{"-hide_banner", "forbidden token"},
		{"-fflags +genpts", "forbidden token"},
		{"-copyts", "forbidden token"},
		{"-copyts -start_at_zero", "forbidden token"},
		{"-protocol_whitelist file,pipe", "forbidden token"},
		{"-max_muxing_queue_size 4096", "forbidden token"},
		{"-ignore_unknown", "forbidden token"},
		{"-bf 4 -", "unauthorized output target"},
		{"-", "unauthorized output target"},
		{"-bf 'unterminated", "unterminated quote"},
		{"-bf", "missing an argument"},
		{"-i=/path", "forbidden token"},
		{"-report=file=log", "forbidden token"},
		{"-attach=file", "forbidden token"},
		{"-map=0:v", "forbidden token"},
		{"-vf=scale=1280:720", "forbidden token"},
		{"-vf:0 scale=1280:720", "forbidden token"},
		{"-af:a loudnorm", "forbidden token"},
		{"-lavfi:0 color", "forbidden token"},
		{"-vf:0 movie=/path/to/secret[ov];[in][ov]overlay", "forbidden token"},
		{"-lavfi:0 movie=secret.mp4", "forbidden token"},
		{"-x264-params csv=/path/file", "unauthorized path token"},
		{"-x265-params dump_yuv=/path/file", "unauthorized path token"},
		{"-x265-params dump_yuv=dump.yuv", "forbidden encoder parameter"},
		{"-x265-params dump-yuv=dump.yuv", "forbidden encoder parameter"},
		{"-x264-params csv=foo.csv", "forbidden encoder parameter"},
		{"-preset ../evil", "unauthorized path token"},
		{"-preset foo/bar", "unauthorized path token"},
	}

	for _, tc := range cases {
		t.Run(tc.flags, func(t *testing.T) {
			_, err := SanitizeCustomFlags(tc.flags)
			if err == nil {
				t.Fatalf("expected error for %q, got nil", tc.flags)
			}
			if !strings.Contains(err.Error(), tc.errContains) {
				t.Fatalf("expected error containing %q, got: %v", tc.errContains, err)
			}
		})
	}
}

func TestSanitizeCustomFlags_LegacyRejections(t *testing.T) {
	cases := []string{
		"/tmp/evil.mkv",
		"keyint=60 /tmp/evil.mkv",
		"keyint=60:out.mkv",
		"keyint=60:/etc/passwd",
		"keyint=60:csv=owned.csv",
		"keyint=60:analysis-save=analysis.dat",
		"keyint=60:log-file=x265.log",
		"keyint=60:stats=1",
		"keyint=60:passlogfile=pf",
		"keyint=60:dump-yuv=dump.yuv",
		"keyint=60:recon=recon.yuv",
		"csv=owned.csv",
		"analysis-save=analysis.dat",
	}

	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			_, err := SanitizeCustomFlags(tc)
			if err == nil {
				t.Fatalf("expected legacy rejection for %q, got nil", tc)
			}
		})
	}
}

func TestSanitizeCustomFlags_DisallowsVideoCopy(t *testing.T) {
	cases := []string{
		"-c:v copy",
		"-vcodec copy",
		"-c:v=copy",
		"-vcodec=copy",
		"-an -c:v copy",
		"-preset fast -c:v copy",
	}

	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			_, err := SanitizeCustomFlags(tc)
			if err == nil {
				t.Fatalf("expected rejection for %q, got nil", tc)
			}
			if !strings.Contains(err.Error(), "video codec \"copy\" is not permitted") {
				t.Fatalf("expected video copy rejection message, got: %v", err)
			}
		})
	}
}

func TestSanitizeCustomFlags_InputLimits(t *testing.T) {
	// Exceeds 2000 characters
	longFlags := "-tune " + strings.Repeat("a", 2005)
	if _, err := SanitizeCustomFlags(longFlags); err == nil {
		t.Fatalf("expected error for flags exceeding 2000 characters, got nil")
	} else if !strings.Contains(err.Error(), "exceed maximum length of 2000 characters") {
		t.Errorf("expected max length error, got: %v", err)
	}

	// Exceeds 100 argument tokens
	var tokens []string
	for i := 0; i < 55; i++ {
		tokens = append(tokens, "-tune", "film")
	}
	manyTokens := strings.Join(tokens, " ") // 110 tokens
	if _, err := SanitizeCustomFlags(manyTokens); err == nil {
		t.Fatalf("expected error for flags exceeding 100 tokens, got nil")
	} else if !strings.Contains(err.Error(), "exceed maximum of 100 argument tokens") {
		t.Errorf("expected max tokens error, got: %v", err)
	}
}

func TestCheckInvariantCollision_Matrix(t *testing.T) {
	// 1. Direct Hard Immutable tokens
	hardInvariants := []string{
		"-nostdin",
		"-hide_banner",
		"-y",
		"-n",
		"-progress",
		"-nostats",
		"-i",
		"-f",
		"-target",
		"-tee",
		"-protocol_whitelist",
		"-max_muxing_queue_size",
		"-ignore_unknown",
	}
	for _, flag := range hardInvariants {
		col := CheckInvariantCollision(flag, ScopeEncodeAll)
		if col == nil {
			t.Errorf("expected collision for hard invariant %q, got nil", flag)
		} else if col.Immutability != ClassHardImmutable {
			t.Errorf("expected ClassHardImmutable for %q, got %s", flag, col.Immutability)
		}
	}

	// 2. Prefix match for stream mapping
	streamPrefixes := []string{"-map", "-map:0", "-map_metadata", "-map_chapters"}
	for _, flag := range streamPrefixes {
		col := CheckInvariantCollision(flag, ScopeEncodeAll)
		if col == nil {
			t.Errorf("expected collision for stream topology flag %q, got nil", flag)
		}
	}

	// 3. Security sandbox tokens & prefixes
	sandboxFlags := []string{
		"-vf",
		"-vf:0",
		"-af",
		"-af:a",
		"-filter",
		"-filter:v",
		"-filter_complex",
		"-lavfi",
		"-lavfi:0",
		"-vstats_file",
		"-passlogfile",
		"-dump_attachment:t:0",
		"-attach",
		"-report",
	}
	for _, flag := range sandboxFlags {
		col := CheckInvariantCollision(flag, ScopeEncodeAll)
		if col == nil {
			t.Errorf("expected sandbox collision for %q, got nil", flag)
		} else if col.Immutability != ClassHardImmutable {
			t.Errorf("expected ClassHardImmutable for sandbox token %q, got %s", flag, col.Immutability)
		}
	}

	// 4. Chunk transcode phase locked invariants
	chunkFlags := []string{"-fps_mode", "-an", "-sn", "-dn"}
	for _, flag := range chunkFlags {
		// Should NOT collide in generic ScopeEncodeAll for -an, but collide in ScopeEncodeChunk
		colChunk := CheckInvariantCollision(flag, ScopeEncodeChunk)
		if colChunk == nil {
			t.Errorf("expected chunk phase collision for %q in ScopeEncodeChunk, got nil", flag)
		} else if colChunk.Immutability != ClassPhaseLocked {
			t.Errorf("expected ClassPhaseLocked for %q in chunk scope, got %s", flag, colChunk.Immutability)
		}
	}

	// 5. Safe non-invariant flags should return nil
	safeFlags := []string{"-tune", "-level", "-rc-lookahead", "-bf", "-crf", "-preset", "-c:v", "-b:a"}
	for _, flag := range safeFlags {
		col := CheckInvariantCollision(flag, ScopeEncodeAll)
		if col != nil {
			t.Errorf("unexpected collision for safe flag %q: %v", flag, col)
		}
	}
}

func TestVerifyDefenseInDepth_RejectsBypassFlags(t *testing.T) {
	testCases := []struct {
		name string
		args []string
	}{
		{"vf with stream specifier", []string{"-vf:0", "scale=1280:720"}},
		{"vf movie overlay", []string{"-vf:0", "movie=/etc/passwd[ov];[in][ov]overlay"}},
		{"af with stream specifier", []string{"-af:a", "loudnorm"}},
		{"lavfi with stream specifier", []string{"-lavfi:0", "color"}},
		{"lavfi movie", []string{"-lavfi:0", "movie=secret.mp4"}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyDefenseInDepth(tc.args, ScopeEncodeAll)
			if err == nil {
				t.Fatalf("expected VerifyDefenseInDepth to reject %v, but passed", tc.args)
			}
			if !strings.Contains(err.Error(), "sandbox violation detected") {
				t.Errorf("expected error to mention sandbox violation, got: %v", err)
			}
		})
	}
}

func TestStartJobPayload_MarshalJSON_NoCallerMutation(t *testing.T) {
	cfg := &ContainerConfig{
		Format:             "mkv",
		OutputFileStrategy: "",
		ActionOnSuccess:    "",
	}
	p := StartJobPayload{
		JobID:              "test-job-1",
		InputPath:          "/path/to/media.mkv",
		SpecVersion:        2,
		OutputFileStrategy: "replace",
		ActionOnSuccess:    "delete",
		ContainerConfig:    cfg,
	}

	data, err := p.MarshalJSON()
	if err != nil {
		t.Fatalf("failed to marshal StartJobPayload: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("empty marshaled payload")
	}

	if cfg.OutputFileStrategy != "" {
		t.Errorf("expected caller's OutputFileStrategy to remain empty, got %q", cfg.OutputFileStrategy)
	}
	if cfg.ActionOnSuccess != "" {
		t.Errorf("expected caller's ActionOnSuccess to remain empty, got %q", cfg.ActionOnSuccess)
	}
}

func TestRegistryImmutability(t *testing.T) {
	// Mutating returned copies must not mutate internal package state
	invariants := SystemInvariantRegistry()
	if len(invariants) == 0 {
		t.Fatal("SystemInvariantRegistry returned empty slice")
	}
	originalFlag := invariants[0].Flag
	invariants[0].Flag = "-malicious-override"

	invariants2 := SystemInvariantRegistry()
	if invariants2[0].Flag != originalFlag {
		t.Fatalf("SystemInvariantRegistry mutated: got %q, want %q", invariants2[0].Flag, originalFlag)
	}

	boolFlags := BooleanRawFlags()
	boolFlags["-fake-boolean"] = true
	boolFlags2 := BooleanRawFlags()
	if boolFlags2["-fake-boolean"] {
		t.Fatal("BooleanRawFlags mutated by caller")
	}

	tokens := SecuritySandboxTokens()
	tokens["-fake-token"] = "fake rationale"
	tokens2 := SecuritySandboxTokens()
	if _, exists := tokens2["-fake-token"]; exists {
		t.Fatal("SecuritySandboxTokens mutated by caller")
	}
}

