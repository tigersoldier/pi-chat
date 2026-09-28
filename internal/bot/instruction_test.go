package bot

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/tigersoldier/pi-gateway/gwclient"

	"github.com/tigersoldier/pi-chat/internal/config"
	"github.com/tigersoldier/pi-chat/internal/store"
)

// adoptedRow turns a test thread's row into one that points at somebody else's
// session, which is the case the instruction has to be delivered by prompt.
func adoptedRow(t *testing.T, th *thread) {
	t.Helper()
	th.update(func(row *store.ThreadRow) {
		row.SessionName = "a-session-somebody-else-made"
		row.SessionPath = "/sessions/adopted.jsonl"
	})
}

func TestInstructionIsInstalledInTheSystemPromptOfSessionsWeCreate(t *testing.T) {
	b, _, _ := newTestBotWith(t, func(cfg *config.Config) {
		// The configured arguments and the project's injected prompt can each
		// carry the flag; the instruction must not become a third one.
		cfg.Gateway.PiArgs = []string{"--model", "fast", "--append-system-prompt", "house rules"}
		cfg.Behavior.InjectedPrompt = "project rules"
	})
	project := workspaceProject(t, b, "fix the failing test")

	args := b.piArgs(project)
	if n := countFlag(args, "--append-system-prompt"); n != 1 {
		t.Fatalf("--append-system-prompt appears %d times in %v, want once", n, args)
	}
	value := flagValue(t, args, "--append-system-prompt")
	for _, want := range []string{"house rules", "project rules", instructionMarker} {
		if !strings.Contains(value, want) {
			t.Errorf("the appended prompt is missing %q:\n%s", want, value)
		}
	}
	// The rest of the arguments are untouched, in order.
	if args[0] != "--model" || args[1] != "fast" {
		t.Errorf("the other arguments changed: %v", args)
	}
	if !slices.Contains(args, "--approve") {
		t.Errorf("approvals should still be automatic by default: %v", args)
	}
}

func TestAppendSystemPromptHandlesBothSpellings(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "separate value",
			args: []string{"--append-system-prompt", "one"},
			want: "one",
		},
		{
			name: "equals form",
			args: []string{"--append-system-prompt=one"},
			want: "one",
		},
		{
			name: "no flag yet",
			args: []string{"--model", "fast"},
			want: "",
		},
		{
			name: "two values are coalesced",
			args: []string{"--append-system-prompt", "one", "--append-system-prompt=two"},
			want: "one",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := appendSystemPrompt(slices.Clone(tc.args), "added")
			if n := countFlag(got, "--append-system-prompt"); n != 1 {
				t.Fatalf("--append-system-prompt appears %d times in %v, want once", n, got)
			}
			value := flagValue(t, got, "--append-system-prompt")
			if !strings.Contains(value, "added") {
				t.Errorf("the text was not added: %v", got)
			}
			if tc.want != "" && !strings.Contains(value, tc.want) {
				t.Errorf("an existing value was dropped: %v", got)
			}
			if n := countFlag(tc.args, "--append-system-prompt"); n == 2 && !strings.Contains(value, "two") {
				t.Errorf("the second value was dropped: %v", got)
			}
		})
	}
}

func TestAnAdoptedSessionIsToldTheRulesOnceBeforeItsPrompt(t *testing.T) {
	b, _, st := newTestBot(t)
	th := testThread(t, b, st)
	adoptedRow(t, th)
	ctx := context.Background()

	got := th.neededInstruction(ctx)
	if !strings.Contains(got, instructionMarker) {
		t.Fatalf("the instruction is missing its marker:\n%s", got)
	}
	if !strings.HasPrefix(got, instructionOpen) || !strings.HasSuffix(got, instructionClose) {
		t.Errorf("the instruction should be wrapped in its markers:\n%s", got)
	}

	// Sent is not the same as delivered: until a prompt is accepted, the
	// instruction is still owed.
	if again := th.neededInstruction(ctx); again != got {
		t.Error("the instruction was forgotten before a prompt carried it")
	}
	th.instructionSent()
	if got := th.neededInstruction(ctx); got != "" {
		t.Errorf("the instruction is being repeated:\n%s", got)
	}
}

func TestCompactionAsksForTheInstructionAgain(t *testing.T) {
	b, _, st := newTestBot(t)
	th := testThread(t, b, st)
	adoptedRow(t, th)
	ctx := context.Background()
	th.instructionSent()

	th.onEvent(gwclient.Event{
		Type: "compaction_end",
		Raw:  []byte(`{"type":"compaction_end","aborted":false,"willRetry":false,"reason":"threshold"}`),
	})
	if got := th.neededInstruction(ctx); got == "" {
		t.Error("a compaction summarized the conversation away; the instruction should be owed again")
	}

	// A compaction that failed changed nothing, so nothing is owed.
	th.instructionSent()
	th.onEvent(gwclient.Event{
		Type: "compaction_end",
		Raw:  []byte(`{"type":"compaction_end","aborted":true,"willRetry":true}`),
	})
	if got := th.neededInstruction(ctx); got != "" {
		t.Errorf("an aborted compaction should discard nothing:\n%s", got)
	}
}

func TestASessionThatAlreadyCarriesTheInstructionIsNotToldTwice(t *testing.T) {
	b, _, st := newTestBot(t)
	th := testThread(t, b, st)
	adoptedRow(t, th)
	b.gw = &fakeGateway{rows: []gwclient.SessionRow{{
		Path:  "/sessions/adopted.jsonl",
		Spawn: map[string][]string{"append-system-prompt": {"the creator's own prompt\n\n" + instructionMarker + " …"}},
	}}}

	if got := th.neededInstruction(context.Background()); got != "" {
		t.Errorf("the session already carries the instruction:\n%s", got)
	}
	// It came from the system prompt, so a compaction cannot take it away.
	th.instructionDiscarded()
	if got := th.neededInstruction(context.Background()); got != "" {
		t.Errorf("a system prompt survives a compaction:\n%s", got)
	}
}

func TestASessionWeCreatedCarriesTheInstructionPermanently(t *testing.T) {
	b, _, st := newTestBot(t)
	th := testThread(t, b, st)
	th.update(func(row *store.ThreadRow) {
		row.SessionName = th.t.SessionName() // ours, by the deterministic name
		row.SessionPath = "/sessions/ours.jsonl"
	})
	b.gw = &fakeGateway{rows: []gwclient.SessionRow{{
		Path:  "/sessions/ours.jsonl",
		Spawn: map[string][]string{"append-system-prompt": {instructionMarker + " …"}},
	}}}
	ctx := context.Background()

	if got := th.neededInstruction(ctx); got != "" {
		t.Errorf("we created this session with the instruction:\n%s", got)
	}
	th.instructionDiscarded()
	if got := th.neededInstruction(ctx); got != "" {
		t.Errorf("the system prompt is not compacted away:\n%s", got)
	}
}

// TestAFailedCatalogLookupSendsTheInstruction pins the trade-off: an
// unanswerable question is answered by sending, because a repetition costs
// tokens while a session that never learns how its prompts are shaped misreads
// every one of them.
func TestAFailedCatalogLookupSendsTheInstruction(t *testing.T) {
	b, _, st := newTestBot(t)
	th := testThread(t, b, st)
	adoptedRow(t, th)
	b.gw = &fakeGateway{err: context.DeadlineExceeded}

	if got := th.neededInstruction(context.Background()); got == "" {
		t.Error("the catalog could not answer, so the instruction should be sent")
	}
}

// TestTheInstructionExplainsTheTranscript is a guard on the text itself: an
// instruction that stops describing the block, the mention form, or the fact
// that the block is not addressed to the agent would quietly stop doing its job.
func TestTheInstructionExplainsTheTranscript(t *testing.T) {
	for _, want := range []string{
		instructionMarker,
		transcriptOpen,
		"<@U123>",
		"not instructions",
	} {
		if !strings.Contains(instructionText, want) {
			t.Errorf("the instruction no longer mentions %q", want)
		}
	}
}

// countFlag counts how many times a long flag appears, in either spelling.
func countFlag(args []string, flag string) int {
	n := 0
	for _, arg := range args {
		if arg == flag || strings.HasPrefix(arg, flag+"=") {
			n++
		}
	}
	return n
}

// flagValue returns the value of a long flag, in either spelling.
func flagValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	for i, arg := range args {
		switch {
		case arg == flag && i+1 < len(args):
			return args[i+1]
		case strings.HasPrefix(arg, flag+"="):
			return strings.TrimPrefix(arg, flag+"=")
		}
	}
	t.Fatalf("no %s in %v", flag, args)
	return ""
}
