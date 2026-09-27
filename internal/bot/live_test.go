package bot

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tigersoldier/pi-chat/internal/config"
)

// TestLiveTurnThroughGateway drives one real turn through pi-gatewayd with the
// tokens pi-chat is configured to use. It is the seam test that unit tests
// cannot cover: session creation through a throwaway admin connection,
// binding the thread connection, prompting, collecting the streamed answer,
// and reading back the authoritative one.
//
// It needs a running pi-gatewayd and a working pi, and it spends a few tokens,
// so it is opt-in:
//
//	PI_CHAT_LIVE=1 go test ./internal/bot -run Live -v
func TestLiveTurnThroughGateway(t *testing.T) {
	if os.Getenv("PI_CHAT_LIVE") == "" {
		t.Skip("set PI_CHAT_LIVE=1 to run against a live pi-gatewayd")
	}

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("load %s: %v", config.DefaultPath(), err)
	}
	// A debug logger to stderr, so the live run shows exactly what the gateway
	// sent.
	debugLog := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	platform := &countingPlatform{}
	b := New(cfg, debugLog, platform)
	defer b.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// A thread of its own, named so it is obvious where it came from in the
	// gateway's session catalog.
	thread := b.threadFor(Thread{Workspace: "T-LIVE", Channel: "C-LIVE", ThreadTS: "e2e"})
	renderer := &recordingRenderer{signal: make(chan struct{}, 16)}

	if err := thread.runTurn(ctx, Message{
		EventID: "live",
		Thread:  Thread{Workspace: "T-LIVE", Channel: "C-LIVE", ThreadTS: "e2e"},
		UserID:  "U-LIVE",
		Text:    "Reply with exactly the single word PONG and nothing else.",
	}, renderer); err != nil {
		t.Fatalf("turn failed: %v", err)
	}

	if len(renderer.finals) != 1 {
		t.Fatalf("rendered %d final answers, want 1", len(renderer.finals))
	}
	answer := renderer.finals[0]
	t.Logf("streamed %d delta(s), final answer %q", len(renderer.deltas), answer)
	if !strings.Contains(answer, "PONG") {
		t.Fatalf("final answer %q does not contain PONG", answer)
	}
}
