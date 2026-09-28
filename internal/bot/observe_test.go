package bot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tigersoldier/pi-chat/internal/store"
)

// testThread returns the core's thread for a seeded row, which is what the
// observation path hangs off: a turn's connection is not needed to read a
// conversation.
func testThread(t *testing.T, b *Bot, st *store.Store) *thread {
	t.Helper()
	const ts = "1700000000.000100"
	seedThread(t, st, store.ThreadRow{ThreadKey: "T1:C1:" + ts, ThreadTS: ts, State: store.StateCold})
	th, err := b.threadFor(Thread{Workspace: "T1", Channel: "C1", ThreadTS: ts})
	if err != nil {
		t.Fatalf("threadFor: %v", err)
	}
	return th
}

// transcripts lists the files the observation path wrote, so a test can check
// both that it wrote one and that it kept the whole conversation in it.
func transcripts(t *testing.T, b *Bot) []string {
	t.Helper()
	dir := filepath.Join(b.stateDir(), threadDirName)
	var out []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // nothing written yet is a normal answer here
		}
		if !info.IsDir() {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the transcript directory: %v", err)
	}
	return out
}

func TestConversationRidesInFrontOfThePrompt(t *testing.T) {
	b, plat, st := newTestBot(t)
	th := testThread(t, b, st)
	plat.said = []Said{
		{TS: "1700000000.000100", UserID: "U2", Name: "Alice", Text: "the tests are failing since the merge"},
		{TS: "1700000000.000200", UserID: "U3", Name: "Bob", Text: "I will take a look"},
		{TS: "1700000000.000300", UserID: "U0", FromBot: true, Text: "on it"},
		{TS: "1700000000.000400", UserID: "U1", Name: "Dana", Text: "what do you think?"},
	}

	obs := th.observe(context.Background(), Message{TS: "1700000000.000400", Text: "what do you think?"})
	prompt := obs.prompt("what do you think?")

	for _, want := range []string{
		transcriptOpen,
		"[Alice (U2)] the tests are failing since the merge",
		"[Bob (U3)] I will take a look",
		transcriptClose,
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt is missing %q:\n%s", want, prompt)
		}
	}
	// The agent's own answer is already in its history, and the request it is
	// answering is the prompt itself: neither belongs in the transcript.
	for _, unwanted := range []string{"on it", "Dana", "[U1]"} {
		if strings.Contains(prompt, unwanted) {
			t.Errorf("the prompt repeats %q:\n%s", unwanted, prompt)
		}
	}
	if !strings.HasSuffix(prompt, "what do you think?") {
		t.Errorf("the request should come last:\n%s", prompt)
	}
	if want := "1700000000.000400"; obs.ts != want {
		t.Errorf("watermark = %q, want the newest message %q", obs.ts, want)
	}
	if oldest, calls := plat.conversationCall(); oldest != "" || calls != 1 {
		t.Errorf("fetch = (oldest %q, %d calls), want (\"\", 1): the first turn has no watermark", oldest, calls)
	}
	if files := transcripts(t, b); len(files) != 0 {
		t.Errorf("nothing was dropped, so nothing should be on disk: %v", files)
	}
}

func TestNoConversationMeansThePromptIsJustTheRequest(t *testing.T) {
	b, plat, st := newTestBot(t)
	th := testThread(t, b, st)

	obs := th.observe(context.Background(), Message{TS: "1700000000.000400", Text: "hello"})
	if obs.block != "" {
		t.Errorf("block = %q, want empty when nobody said anything", obs.block)
	}
	if got := obs.prompt("hello"); got != "hello" {
		t.Errorf("prompt = %q, want the request alone", got)
	}
	// The trigger is a message of the thread too, so the watermark still moves
	// past it: otherwise a later turn would present this request as somebody
	// else's remark.
	if want := "1700000000.000400"; obs.ts != want {
		t.Errorf("watermark = %q, want %q", obs.ts, want)
	}
	if _, calls := plat.conversationCall(); calls != 1 {
		t.Errorf("fetches = %d, want 1", calls)
	}
}

func TestAFailedFetchKeepsTheWatermarkAndThePrompt(t *testing.T) {
	b, plat, st := newTestBot(t)
	th := testThread(t, b, st)
	plat.observeErr = errors.New("slack is unhappy")

	obs := th.observe(context.Background(), Message{TS: "1700000000.000400", Text: "hello"})
	if obs.block != "" {
		t.Errorf("block = %q, want empty", obs.block)
	}
	// Moving the watermark here would drop the messages this fetch failed to
	// read, and the agent would never see them.
	if obs.ts != "" {
		t.Errorf("watermark = %q, want it left alone so the next turn retries", obs.ts)
	}
	if got := obs.prompt("hello"); got != "hello" {
		t.Errorf("prompt = %q, want the request alone", got)
	}
}

func TestLongConversationsDropTheOldestAndKeepThemInAFile(t *testing.T) {
	b, plat, st := newTestBot(t)
	th := testThread(t, b, st)

	// Thirty messages of a kilobyte each: far more than a prompt can carry.
	const count = 30
	for i := range count {
		plat.said = append(plat.said, Said{
			TS:     fmt.Sprintf("1700000000.%06d", 100+i),
			UserID: "U2",
			Name:   "Alice",
			Text:   fmt.Sprintf("message-%02d ", i) + strings.Repeat("x", 1000),
		})
	}
	// The newest message is the trigger, which is the prompt rather than part of
	// the transcript.
	plat.said = append(plat.said, Said{TS: "1700000000.000999", UserID: "U1", Name: "Dana", Text: "summarize"})

	obs := th.observe(context.Background(), Message{TS: "1700000000.000999", Text: "summarize"})
	if len(obs.block) > transcriptBudget+500 {
		t.Errorf("block is %d characters, want it near the %d budget", len(obs.block), transcriptBudget)
	}
	if !strings.Contains(obs.block, "message-29 ") {
		t.Error("the block dropped the newest message, which is the one that matters")
	}
	if strings.Contains(obs.block, "message-00 ") {
		t.Error("the block kept the oldest message instead of dropping it")
	}
	if !strings.Contains(obs.block, "earlier messages omitted") {
		t.Errorf("the block should say something was omitted:\n%s", obs.block)
	}

	files := transcripts(t, b)
	if len(files) != 1 {
		t.Fatalf("transcripts on disk = %v, want exactly one", files)
	}
	whole, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read the transcript: %v", err)
	}
	// Everything the block dropped has to be in the file, or the omission would
	// really be a loss.
	for _, want := range []string{"message-00 ", "message-29 "} {
		if !strings.Contains(string(whole), want) {
			t.Errorf("the transcript on disk is missing %q", want)
		}
	}
	if !strings.Contains(obs.block, files[0]) {
		t.Errorf("the block does not name the file it refers to:\n%s", obs.block)
	}
}

func TestOneHugeMessageIsShortenedAndKeptWholeInAFile(t *testing.T) {
	b, plat, st := newTestBot(t)
	th := testThread(t, b, st)
	plat.said = []Said{{
		TS:     "1700000000.000100",
		UserID: "U2",
		Name:   "Alice",
		Text:   "the log: " + strings.Repeat("x", 5000),
	}}

	obs := th.observe(context.Background(), Message{TS: "1700000000.000400", Text: "look at this"})
	// The block also carries the markers and the note naming the file, so what is
	// asserted is the length of the message inside it.
	if carried := strings.Count(obs.block, "x"); carried > saidLimit {
		t.Errorf("the block carries %d characters of the message, want it cut at %d",
			carried, saidLimit)
	}
	if !strings.Contains(obs.block, "shortened") {
		t.Errorf("the block should say a message was shortened:\n%s", obs.block)
	}
	// A shortened message is an omission like any other: the rest of it has to
	// be somewhere the agent can read.
	files := transcripts(t, b)
	if len(files) != 1 {
		t.Fatalf("transcripts on disk = %v, want exactly one", files)
	}
	whole, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read the transcript: %v", err)
	}
	if len(whole) < 5000 {
		t.Errorf("the transcript is %d bytes, want the whole %d-character message", len(whole), 5000)
	}
}

func TestTheWatermarkIsRecordedAndNeverMovesBackwards(t *testing.T) {
	b, _, st := newTestBot(t)
	th := testThread(t, b, st)
	ctx := context.Background()

	th.rememberObserved(ctx, observation{ts: "1700000000.000500"})
	if got := th.snapshot().ObservedTS; got != "1700000000.000500" {
		t.Fatalf("in-memory watermark = %q, want the value just recorded", got)
	}
	row, found, err := st.Thread(ctx, th.key)
	if err != nil || !found {
		t.Fatalf("read the row back: found=%v err=%v", found, err)
	}
	if row.ObservedTS != "1700000000.000500" {
		t.Errorf("stored watermark = %q, want the value just recorded", row.ObservedTS)
	}

	// A replayed or out-of-order turn must not make the bot read the same
	// conversation again.
	th.rememberObserved(ctx, observation{ts: "1700000000.000100"})
	if got := th.snapshot().ObservedTS; got != "1700000000.000500" {
		t.Errorf("watermark = %q after an older value, want it unchanged", got)
	}
	// And nothing at all is not a watermark.
	th.rememberObserved(ctx, observation{})
	if got := th.snapshot().ObservedTS; got != "1700000000.000500" {
		t.Errorf("watermark = %q after an empty observation, want it unchanged", got)
	}
}

func TestTheWatermarkIsWhatTheNextFetchAsksFrom(t *testing.T) {
	b, plat, st := newTestBot(t)
	th := testThread(t, b, st)
	th.rememberObserved(context.Background(), observation{ts: "1700000000.000500"})

	plat.said = []Said{{TS: "1700000000.000700", UserID: "U2", Name: "Alice", Text: "and another thing"}}
	obs := th.observe(context.Background(), Message{TS: "1700000000.000800", Text: "what?"})
	if oldest, _ := plat.conversationCall(); oldest != "1700000000.000500" {
		t.Errorf("the fetch asked from %q, want the remembered watermark", oldest)
	}
	if want := "1700000000.000800"; obs.ts != want {
		t.Errorf("watermark = %q, want the newest message %q", obs.ts, want)
	}
}

// plainPlatform is a platform that cannot read a thread back, so the core must
// not ask it to.
type plainPlatform struct{ started int }

func (p *plainPlatform) StartTurn(context.Context, Message) (Renderer, error) {
	p.started++
	return &recordingRenderer{}, nil
}
func (p *plainPlatform) Post(context.Context, Notice) error { return nil }
func (p *plainPlatform) OpenThread(context.Context, string, string) (Thread, error) {
	return Thread{Workspace: "T1", Channel: "C1", ThreadTS: "900.000001"}, nil
}

func TestAPlatformWithoutAnObserverIsNotAsked(t *testing.T) {
	b, _, st := newTestBot(t)
	b.plat = &plainPlatform{}
	th := testThread(t, b, st)

	obs := th.observe(context.Background(), Message{TS: "1700000000.000400", Text: "hello"})
	if obs.block != "" || obs.ts != "" {
		t.Errorf("obs = %+v, want the zero observation", obs)
	}
	if got := obs.prompt("hello"); got != "hello" {
		t.Errorf("prompt = %q, want the request alone", got)
	}
}

func TestCompareTSOrdersTimestampsAsNumbers(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1700000000.000100", "1700000000.000200", -1},
		{"1700000000.000200", "1700000000.000200", 0},
		{"1700000000.000300", "1700000000.000200", 1},
		// Lexicographically "100" sorts before "99", numerically after.
		{"1700000100.000000", "1700000099.000000", 1},
		{"", "1700000000.000100", -1},
	} {
		if got := compareTS(tc.a, tc.b); got != tc.want {
			t.Errorf("compareTS(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
