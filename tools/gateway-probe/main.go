// Command m0check is a throwaway M0.5 verification: it drives pi-gatewayd
// through gwclient with the two token files pi-chat will use, and checks the
// capability split and the delete/unbind behaviour DESIGN.md §3 depends on.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tigersoldier/pi-gateway/gwclient"
	"github.com/tigersoldier/pi-gateway/protocol"
)

const (
	stateDir      = "/home/pi/.config/pi-gateway"
	adminToken    = "/home/pi/.config/pi-chat/gateway-admin.token"
	threadToken   = "/home/pi/.config/pi-chat/gateway-thread.token"
	sessionName   = "m0-check"
	sessionCwd    = "/tmp/m0check"
	adminExpected = "observe,interject,prompt,ui,control,admin"
	threadExpect  = "observe,interject,prompt,ui,control"
)

type counter struct {
	mu     sync.Mutex
	events map[string]int
	ui     []string
}

func (c *counter) note(t string, raw []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.events == nil {
		c.events = map[string]int{}
	}
	c.events[t]++
	if t == "extension_ui_request" && len(c.ui) < 4 {
		c.ui = append(c.ui, string(raw))
	}
}

func (c *counter) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]string, 0, len(c.events))
	for k := range c.events {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, c.events[k]))
	}
	return strings.Join(parts, " ")
}

func dial(ctx context.Context, name, tokenFile string) (*gwclient.Client, *counter, error) {
	c, err := gwclient.Dial(ctx, gwclient.Config{
		Name:      name,
		Kind:      "m0check",
		StateDir:  stateDir,
		TokenFile: tokenFile,
	})
	if err != nil {
		return nil, nil, err
	}
	counts := &counter{}
	go func() {
		for e := range c.Events() {
			counts.note(e.Type, e.Raw)
		}
	}()
	return c, counts, nil
}

func caps(c *gwclient.Client) string { return strings.Join(c.Granted(), ",") }

// prompt runs one turn and returns the assistant text it produced.
func prompt(ctx context.Context, c *gwclient.Client, text string) (string, error) {
	before, _ := c.GetLastAssistantText(ctx)
	if _, err := c.Prompt(ctx, text); err != nil {
		return "", err
	}
	deadline := time.Now().Add(20 * time.Second)
	for c.TurnRunning() == false && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := c.AwaitSettled(ctx); err != nil {
		return "", err
	}
	for {
		got, err := c.GetLastAssistantText(ctx)
		if err != nil {
			return "", err
		}
		if got != before {
			return got, nil
		}
		if time.Now().After(deadline) {
			return got, fmt.Errorf("assistant text never changed (still %q)", got)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func head(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func main() {
	if code := run(); code != 0 {
		os.Exit(code)
	}
}

func run() int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := os.MkdirAll(sessionCwd, 0o755); err != nil {
		return fail("mkdir "+sessionCwd, err)
	}

	// --- admin connection: lifecycle only, never prompts -------------------
	admin, adminEvents, err := dial(ctx, "pi-chat-admin", adminToken)
	if err != nil {
		return fail("dial admin", err)
	}
	defer admin.Close()
	fmt.Printf("admin  granted: %s\n", caps(admin))
	if got := caps(admin); got != adminExpected {
		return fail("admin capabilities", fmt.Errorf("got %q want %q", got, adminExpected))
	}
	if admin.Session() != nil {
		return fail("admin is session-less", fmt.Errorf("bound to %+v", admin.Session()))
	}

	ref, err := admin.NewSession(ctx, gwclient.NewSessionRequest{
		Name:   sessionName,
		Cwd:    sessionCwd,
		PiArgs: []string{"--approve"},
		Tags:   map[string]string{"origin": "m0check"},
	})
	if err != nil {
		return fail("gw_new_session as admin", err)
	}
	fmt.Printf("admin  created: name=%s id=%s cwd=%s path=%s\n", ref.Name, ref.ID, sessionCwd, ref.Path)

	first, err := prompt(ctx, admin, "Reply with exactly one word: PONG")
	if err != nil {
		return fail("prompt as admin", err)
	}
	fmt.Printf("admin  answer: %q\n", head(first, 120))
	if !strings.Contains(strings.ToUpper(first), "PONG") {
		return fail("admin answer", fmt.Errorf("expected PONG, got %q", head(first, 200)))
	}
	fmt.Printf("admin  events: %s\n", adminEvents)
	for _, u := range adminEvents.uiDump() {
		fmt.Printf("admin  ui:     %s\n", u)
	}

	// --- thread connection: operator + control, no admin -------------------
	thread, threadEvents, err := dial(ctx, "pi-chat-thread", threadToken)
	if err != nil {
		return fail("dial thread", err)
	}
	defer thread.Close()
	fmt.Printf("thread granted: %s\n", caps(thread))
	if got := caps(thread); got != threadExpect {
		return fail("thread capabilities", fmt.Errorf("got %q want %q", got, threadExpect))
	}
	if thread.Can(protocol.CapAdmin) {
		return fail("thread lacks admin", errors.New("Can(admin) is true"))
	}

	_, err = thread.NewSession(ctx, gwclient.NewSessionRequest{Name: "m0-check-denied", Cwd: sessionCwd})
	var rerr *gwclient.ResponseError
	switch {
	case err == nil:
		return fail("thread NewSession", errors.New("succeeded; it must be denied"))
	case errors.As(err, &rerr):
		fmt.Printf("thread denied:  gw_new_session → %s (%s)\n", rerr.Message, rerr.Code)
	default:
		return fail("thread NewSession", err)
	}

	// The attach-before-prompt invariant from DESIGN.md §4, on a second
	// connection and a second token.
	if _, err := thread.SwitchSession(ctx, ref.Path); err != nil {
		return fail("thread SwitchSession", err)
	}
	if s := thread.Session(); s == nil || s.Path != ref.Path {
		return fail("thread bound", fmt.Errorf("session=%+v want path %s", s, ref.Path))
	}
	second, err := prompt(ctx, thread, "Reply with exactly one word: OK")
	if err != nil {
		return fail("prompt as thread", err)
	}
	fmt.Printf("thread answer:  %q\n", head(second, 120))

	if _, err := thread.SetThinkingLevel(ctx, "low"); err != nil {
		return fail("control capability", err)
	}
	fmt.Printf("thread control: set_thinking_level accepted\n")
	fmt.Printf("thread events:  %s\n", threadEvents)

	// --- delete: unbind both connections, refuse later work ---------------
	rows, err := admin.ListSessions(ctx, gwclient.SessionFilter{Cwd: sessionCwd})
	if err != nil {
		return fail("gw_list_sessions", err)
	}
	for _, r := range rows {
		fmt.Printf("catalog:        name=%s live=%v clients=%d createdBy=%v\n",
			r.Name, r.Live, len(r.Clients), r.CreatedBy)
	}

	if _, err := admin.DeleteSession(ctx, ref.Path, false); err != nil {
		return fail("gw_delete_session", err)
	}
	fmt.Printf("admin  delete:  accepted (force=false)\n")

	unboundAt := time.Now().Add(15 * time.Second)
	for (admin.Session() != nil || thread.Session() != nil) && time.Now().Before(unboundAt) {
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Printf("after delete:   admin.Session()=%v thread.Session()=%v\n", admin.Session(), thread.Session())
	if admin.Session() != nil || thread.Session() != nil {
		return fail("delete unbinds every client", errors.New("a connection is still bound"))
	}

	_, err = thread.Prompt(ctx, "this must not start a new session")
	switch {
	case err == nil:
		return fail("prompt after delete", errors.New("accepted; it must be refused"))
	case errors.As(err, &rerr):
		fmt.Printf("after delete:   prompt refused → %s (%s)\n", rerr.Message, rerr.Code)
		if rerr.Code != protocol.CodeUnknownSession {
			return fail("prompt after delete", fmt.Errorf("code %q want %q", rerr.Code, protocol.CodeUnknownSession))
		}
	default:
		return fail("prompt after delete", err)
	}

	rows, err = admin.ListSessions(ctx, gwclient.SessionFilter{Cwd: sessionCwd})
	if err != nil {
		return fail("gw_list_sessions after delete", err)
	}
	fmt.Printf("catalog after:  %d row(s), registered session did not come back\n", len(rows))

	fmt.Println("\nM0.5 PASS")
	return 0
}

func fail(what string, err error) int {
	fmt.Fprintf(os.Stderr, "\nM0.5 FAIL at %s: %v\n", what, err)
	return 1
}

func (c *counter) uiDump() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.ui))
	for _, u := range c.ui {
		if len(u) > 400 {
			u = u[:400] + "…"
		}
		out = append(out, u)
	}
	return out
}
