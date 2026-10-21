package ffmpeg

import (
	"strings"
	"testing"
)

// Suite 1: Direct Token Rejection Matrix (Negative Cases)
func TestCollisionSuite1_DirectTokenRejectionMatrix(t *testing.T) {
	cases := []string{
		"-nostdin",
		"-hide_banner",
		"-y",
		"-n",
		"-progress pipe:1",
		"-progress /tmp/prog",
		"-nostats",
		"-ignore_unknown",
	}

	for _, customFlags := range cases {
		t.Run(customFlags, func(t *testing.T) {
			b := NewCommandBuilder("/media/in.mkv", "/media/out.mkv")
			_ = b.SetVideoCodec("libx264")
			err := b.SetCustomFlags("", customFlags)
			if err == nil {
				t.Fatalf("expected rejection for %q, got nil error", customFlags)
			}
			if !strings.Contains(err.Error(), "immutable and cannot be overridden") && !strings.Contains(err.Error(), "forbidden token") {
				t.Errorf("expected immutable error, got: %v", err)
			}
		})
	}
}

// Suite 2: Valued & Option Variant Collision Matrix (Negative Cases)
func TestCollisionSuite2_ValuedAndOptionVariantCollisionMatrix(t *testing.T) {
	cases := []string{
		"-protocol_whitelist all",
		"-protocol_whitelist=file,http,tcp",
		"-protocol_whitelist 'file,pipe,http'",
		"-max_muxing_queue_size 1024",
		"-max_muxing_queue_size=8192",
		"-fflags nobuffer",
		"-fflags=+discardcorrupt",
	}

	for _, customFlags := range cases {
		t.Run(customFlags, func(t *testing.T) {
			b := NewCommandBuilder("/media/in.mkv", "/media/out.mkv")
			_ = b.SetVideoCodec("libx264")
			err := b.SetCustomFlags("", customFlags)
			if err == nil {
				t.Fatalf("expected rejection for %q, got nil error", customFlags)
			}
			if !strings.Contains(err.Error(), "immutable and cannot be overridden") && !strings.Contains(err.Error(), "forbidden token") {
				t.Errorf("expected immutable error, got: %v", err)
			}
		})
	}
}

// Suite 3: Prefix & Stream Topology Collision Matrix (Negative Cases)
func TestCollisionSuite3_PrefixAndStreamTopologyCollisionMatrix(t *testing.T) {
	cases := []string{
		"-map 0",
		"-map 0:v:0",
		"-map 1:a:0",
		"-map_metadata -1",
		"-map_chapters 0",
		"-vf scale=1920:1080",
		"-filter:v fps=30",
		"-filter_complex [0:v]scale[v]",
	}

	for _, customFlags := range cases {
		t.Run(customFlags, func(t *testing.T) {
			b := NewCommandBuilder("/media/in.mkv", "/media/out.mkv")
			_ = b.SetVideoCodec("libx264")
			err := b.SetCustomFlags("", customFlags)
			if err == nil {
				t.Fatalf("expected rejection for %q, got nil error", customFlags)
			}
			if !strings.Contains(err.Error(), "immutable and cannot be overridden") && !strings.Contains(err.Error(), "forbidden token") {
				t.Errorf("expected immutable or forbidden token error, got: %v", err)
			}
		})
	}
}

// Suite 4: Sandbox Bypass & Multi-Output Hijack Matrix (Security Negative Cases)
func TestCollisionSuite4_SandboxBypassAndMultiOutputHijackMatrix(t *testing.T) {
	cases := []string{
		"-dump_attachment:t:0 /etc/shadow",
		"-vstats_file /tmp/stats.log",
		"-passlogfile /tmp/pass.log",
		"-f null -",
		"-target vcd",
		"-i /etc/passwd",
	}

	for _, customFlags := range cases {
		t.Run(customFlags, func(t *testing.T) {
			b := NewCommandBuilder("/media/in.mkv", "/media/out.mkv")
			_ = b.SetVideoCodec("libx264")
			err := b.SetCustomFlags("", customFlags)
			if err == nil {
				t.Fatalf("expected rejection for %q, got nil error", customFlags)
			}
		})
	}
}

// Suite 7: Valid Overrides & Safe Flags Matrix (Positive Cases)
func TestCollisionSuite7_ValidOverridesAndSafeFlagsMatrix(t *testing.T) {
	cases := []struct {
		flags      string
		wantInArgs string
	}{
		{"-tune film", "-tune"},
		{"-level 5.1", "-level"},
		{"-rc-lookahead 40", "-rc-lookahead"},
		{"-bf 4", "-bf"},
		{"-crf 18", "18"},
		{"-preset slower", "slower"},
		{"-b:v 8M", "8M"},
	}

	for _, tc := range cases {
		t.Run(tc.flags, func(t *testing.T) {
			b := NewCommandBuilder("/media/in.mkv", "/media/out.mkv")
			_ = b.SetVideoCodec("libx264")
			err := b.SetCustomFlags("", tc.flags)
			if err != nil {
				t.Fatalf("unexpected rejection for legal flag %q: %v", tc.flags, err)
			}
			args, err := b.Build()
			if err != nil {
				t.Fatalf("failed to build args: %v", err)
			}
			found := false
			for _, a := range args {
				if a == tc.wantInArgs {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected %q in compiled args %v", tc.wantInArgs, args)
			}
		})
	}
}
