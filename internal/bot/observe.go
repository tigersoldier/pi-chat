package bot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tigersoldier/pi-chat/internal/store"
)

// What one prompt may carry of the conversation around it (DESIGN.md §4).
//
// The budget is in characters rather than in messages, because a message can be
// anything from "ok" to a pasted stack trace. The newest text is the part that
// matters, so when the budget runs out it is the oldest lines that go — and when
// anything goes, the whole transcript is written to a file and named in the
// block, so the agent can read what the prompt could not carry.
const (
	transcriptBudget = 10_000
	saidLimit        = 2_000
)

// The block the transcript travels in. It is a marker rather than prose because
// the instruction that explains it is installed once, not repeated per turn
// (DESIGN.md §4, §6).
const (
	transcriptOpen  = "<thread-conversation>"
	transcriptClose = "</thread-conversation>"
)

// Bounds on the transcripts a prompt may point at. They are the one place
// pi-chat writes conversation text to disk, so both the number of files per
// thread and the time a fetch may take are bounded here.
const (
	threadDirName  = "threads"
	transcriptKeep = 20
	observeTimeout = 10 * time.Second
)

// observation is the conversation a turn should be told about.
type observation struct {
	// block is what goes in front of the prompt, empty when there is nothing to
	// tell: no observer on this platform, no new messages, or a fetch that
	// failed.
	block string
	// ts is the watermark to remember once the prompt is accepted: the newest
	// message this turn has been shown. Empty means "do not move it", which is
	// how a failed fetch keeps everything for the next turn.
	ts string
}

// prompt is what a turn sends: the conversation it should know about, then the
// request it is answering. The order carries the meaning — the request is what
// the agent answers, and the block only tells it what the room has been saying.
func (o observation) prompt(trigger string) string {
	if o.block == "" {
		return trigger
	}
	return o.block + "\n\n" + trigger
}

// observe reads the thread's conversation since the last turn, and renders it
// for the prompt about to be sent.
//
// Nothing here fails a turn. A platform that cannot read threads back, or that
// fails while being asked, means the prompt is the trigger alone — exactly what
// it was before observation existed — and the watermark stays where it was, so
// the messages are still there to be picked up next time.
func (th *thread) observe(ctx context.Context, m Message) observation {
	observer, ok := th.b.plat.(Observer)
	if !ok {
		return observation{}
	}
	ctx, cancel := context.WithTimeout(ctx, observeTimeout)
	defer cancel()

	watermark := th.snapshot().ObservedTS
	said, err := observer.Conversation(ctx, th.t, watermark)
	if err != nil {
		th.log.Warn("cannot read what the thread has been saying; prompting without it", "error", err)
		return observation{}
	}
	// The watermark is an exclusive lower bound in intent, but a platform may
	// treat it as the start of the range and hand the boundary message back —
	// Slack's `oldest` is inclusive. Dropping it here rather than trusting the
	// boundary is what keeps a request the previous turn already carried from
	// returning as a remark somebody made (DESIGN.md §4).
	said = slices.DeleteFunc(said, func(s Said) bool { return compareTS(s.TS, watermark) <= 0 })
	if len(said) == 0 {
		// Nothing new, but the trigger is a message of this thread too: moving
		// the watermark past it is what stops a later turn from presenting the
		// request it already answered as a remark somebody made.
		return observation{ts: m.TS}
	}
	return th.compose(said, m.TS)
}

// compose turns what the platform reported into the block, bounds it, and
// decides the watermark.
func (th *thread) compose(said []Said, triggerTS string) observation {
	ordered := slices.Clone(said)
	slices.SortStableFunc(ordered, func(a, b Said) int { return compareTS(a.TS, b.TS) })

	// The watermark covers every message this fetch saw, the trigger included.
	watermark := triggerTS
	lines := make([]string, 0, len(ordered))
	full := make([]string, 0, len(ordered))
	truncated := false
	for _, s := range ordered {
		if compareTS(s.TS, watermark) > 0 {
			watermark = s.TS
		}
		switch {
		case s.FromBot, s.TS == triggerTS:
			// The agent's own answers are already in its history, and the
			// trigger is the prompt: neither is news.
			continue
		case strings.TrimSpace(s.Text) == "":
			// A message with no text at all (an attachment this build cannot
			// read). It is still covered by the watermark.
			continue
		}
		whole := label(s) + " " + oneLine(s.Text)
		shown := label(s) + " " + truncateSaid(s.Text)
		truncated = truncated || shown != whole
		full = append(full, whole)
		lines = append(lines, shown)
	}
	if len(lines) == 0 {
		return observation{ts: watermark}
	}

	// From the newest line backwards, so the budget keeps what was said most
	// recently. A line that does not fit is dropped whole rather than halved.
	kept := make([]string, 0, len(lines))
	budget := transcriptBudget
	dropped := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if len(lines[i])+1 > budget {
			dropped = i + 1
			break
		}
		budget -= len(lines[i]) + 1
		kept = append(kept, lines[i])
	}
	slices.Reverse(kept)

	note := ""
	if dropped > 0 || truncated {
		switch {
		case dropped > 0 && truncated:
			note = fmt.Sprintf("[%d earlier messages omitted and some shortened", dropped)
		case dropped > 0:
			note = fmt.Sprintf("[%d earlier messages omitted", dropped)
		default:
			note = "[some messages shortened"
		}
		path, err := th.writeTranscript(strings.Join(full, "\n"), watermark)
		if err != nil {
			// A truncated prompt still beats a failed turn, so the block says
			// what happened without a file to point at.
			th.log.Warn("cannot write the thread transcript", "error", err)
			note += "]"
		} else {
			note += "; the whole thread is at " + path + "]"
		}
	}
	return observation{block: transcriptBlock(note, kept), ts: watermark}
}

// transcriptBlock renders the section that goes in front of a prompt.
func transcriptBlock(note string, lines []string) string {
	var b strings.Builder
	b.WriteString(transcriptOpen)
	b.WriteByte('\n')
	if note != "" {
		b.WriteString(note)
		b.WriteByte('\n')
	}
	for _, line := range lines {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteString(transcriptClose)
	return b.String()
}

// label is who a line is attributed to: their name and ID when the platform
// resolved a name, and just the ID when it did not.
//
// The ID is always there because it is what a mention is made of: `<@U123>` is
// how the agent brings that person back into the conversation, and a name alone
// would leave it nothing to write (DESIGN.md §4).
func label(s Said) string {
	name := strings.TrimSpace(s.Name)
	switch {
	case name == "" && s.UserID == "":
		return "[someone]"
	case name == "":
		return "[" + s.UserID + "]"
	case s.UserID == "":
		return "[" + name + "]"
	}
	return "[" + name + " (" + s.UserID + ")]"
}

// oneLine flattens a message for the full transcript on disk, which keeps the
// same line-per-message shape as the block.
func oneLine(text string) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.TrimSpace(text), "\n", " "))
}

// truncateSaid bounds one message inside the block. The full text is in the file
// whenever the block had to cut something.
func truncateSaid(text string) string {
	line := oneLine(text)
	if len(line) <= saidLimit {
		return line
	}
	return cutAtRune(line, saidLimit) + " …"
}

// cutAtRune cuts a string to at most n bytes without splitting a rune.
func cutAtRune(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// compareTS orders two platform timestamps.
//
// They are decimal strings ("1700000000.000500"), and they are compared as
// numbers rather than as text: lexicographically "10" sorts before "9", and the
// two formats do occur. Microseconds are the finest granularity the format
// carries, and float64 holds a modern epoch to well under that, so the
// comparison is exact where it matters; anything unparseable falls back to text.
func compareTS(a, b string) int {
	af, aerr := strconv.ParseFloat(a, 64)
	bf, berr := strconv.ParseFloat(b, 64)
	switch {
	case aerr != nil || berr != nil:
		return strings.Compare(a, b)
	case af < bf:
		return -1
	case af > bf:
		return 1
	}
	return 0
}

// writeTranscript puts the conversation this fetch saw under the state
// directory, named for the watermark it ends at, and returns the path to name in
// the block.
//
// This is the one place pi-chat writes conversation text to disk, and it is
// deliberate: a prompt that had to drop messages should still leave the agent a
// way to read them. It is bounded — one file per turn, at most transcriptKeep of
// them per thread — and the directory belongs to the session, so it goes when
// the session does (DESIGN.md §7).
func (th *thread) writeTranscript(text, ts string) (string, error) {
	// Both parts of this path come from platform identifiers, so both are
	// reduced to a slug. Checking that a slug is what they are makes it a
	// property of this function rather than a belief about its callers.
	thread, name := th.t.SessionName(), slugName(ts)
	if thread != slugName(thread) || name != slugName(name) {
		return "", fmt.Errorf("refusing to write a transcript under %q/%q", thread, name)
	}
	dir := filepath.Join(th.b.stateDir(), threadDirName, thread)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if name == "" {
		name = strconv.FormatInt(time.Now().Unix(), 10)
	}
	path := filepath.Join(dir, "observed-"+name+".md")
	if err := os.WriteFile(path, []byte(text+"\n"), 0o600); err != nil {
		return "", err
	}
	th.pruneTranscripts(dir)
	return path, nil
}

// pruneTranscripts keeps the newest transcripts of a thread, so a long
// conversation cannot fill the state directory.
func (th *thread) pruneTranscripts(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type transcript struct {
		path string
		mod  time.Time
	}
	var files []transcript
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "observed-") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, transcript{filepath.Join(dir, entry.Name()), info.ModTime()})
	}
	if len(files) <= transcriptKeep {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	for _, old := range files[transcriptKeep:] {
		if err := os.Remove(old.path); err != nil {
			th.log.Debug("cannot remove an old transcript", "path", old.path, "error", err)
		}
	}
}

// rememberObserved advances the watermark, and only ever after a prompt was
// accepted: a prompt that failed must leave the messages it carried for the next
// turn to pick up again. It never moves it backwards, so an out-of-order or
// replayed turn cannot make the bot read the same conversation twice.
func (th *thread) rememberObserved(ctx context.Context, o observation) {
	current := th.snapshot().ObservedTS
	if o.ts == "" || compareTS(o.ts, current) <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancel()
	if err := th.b.store.SetThreadObservedTS(ctx, th.key, o.ts); err != nil {
		th.log.Warn("cannot record how far the thread has been read", "error", err)
		return
	}
	th.update(func(row *store.ThreadRow) { row.ObservedTS = o.ts })
}

// stateDir is where pi-chat keeps the state that is not the database: the
// per-thread transcripts a prompt may point at (DESIGN.md §7).
func (b *Bot) stateDir() string {
	return filepath.Dir(b.cfg.Paths.DBPath)
}
