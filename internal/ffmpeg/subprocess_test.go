package ffmpeg

import (
	"context"
	"testing"
)

func TestCommandProcessGroupAndStdinAttributes(t *testing.T) {
	cmd := execCommand("echo", "test")
	if cmd == nil {
		t.Fatal("execCommand returned nil")
	}
	if cmd.Stdin != nil {
		t.Errorf("execCommand must have cmd.Stdin = nil, got %v", cmd.Stdin)
	}
	if cmd.SysProcAttr == nil {
		t.Fatal("execCommand must set SysProcAttr")
	}

	checkSysProcAttr(t, cmd)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmdCtx := execCommandContext(ctx, "echo", "test")
	if cmdCtx == nil {
		t.Fatal("execCommandContext returned nil")
	}
	if cmdCtx.Stdin != nil {
		t.Errorf("execCommandContext must have cmd.Stdin = nil, got %v", cmdCtx.Stdin)
	}
	if cmdCtx.SysProcAttr == nil {
		t.Fatal("execCommandContext must set SysProcAttr")
	}

	checkSysProcAttr(t, cmdCtx)

	if cmdCtx.Cancel == nil {
		t.Errorf("execCommandContext must configure cmd.Cancel")
	}
	if cmdCtx.WaitDelay <= 0 {
		t.Errorf("execCommandContext must configure nonzero cmd.WaitDelay")
	}
}

func TestInvocationsNeutralizeStdinAndIncludeNoStdin(t *testing.T) {
	// Verify builder base args contain -nostdin
	b := NewCommandBuilder("/in.mkv", "/out.mkv")
	_ = b.SetVideoCodec("libx264")
	args, err := b.Build()
	if err != nil {
		t.Fatalf("builder Build failed: %v", err)
	}
	hasNoStdin := false
	for _, a := range args {
		if a == "-nostdin" {
			hasNoStdin = true
			break
		}
	}
	if !hasNoStdin {
		t.Errorf("builder Build() must include -nostdin, got %v", args)
	}

	// Verify execCommand and execCommandContext explicitly set cmd.Stdin = nil
	cmd1 := execCommand("ffmpeg")
	if cmd1.Stdin != nil {
		t.Errorf("execCommand Stdin must be nil, got %v", cmd1.Stdin)
	}
	cmd2 := execCommandContext(context.Background(), "ffmpeg")
	if cmd2.Stdin != nil {
		t.Errorf("execCommandContext Stdin must be nil, got %v", cmd2.Stdin)
	}
}
