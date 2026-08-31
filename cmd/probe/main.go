// Command probe is the phase-0 Google plumbing spike for pi-gchat.
//
// It pulls interaction events for the Chat app from the Pub/Sub subscription,
// replies to each event through the Chat API (spaces.messages.create), acks
// the Pub/Sub message, and exits after -count events. It proves the full
// round-trip:
//
//	Chat message → Chat app → Pub/Sub → probe → spaces.messages.create → Chat
//
// Nothing else — no sessions, no SQLite, no allowlists. That is phase 1
// (see DESIGN.md).
//
// Configuration comes from the [gcp] section of the TOML config written by
// scripts/setup-gcp.sh (default ~/.pi/agent/pi-gchat.toml); flags override it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/pubsub"
	"github.com/BurntSushi/toml"
	"google.golang.org/api/chat/v1"
	"google.golang.org/api/option"
)

const defaultConfig = "~/.pi/agent/pi-gchat.toml"

type gcpConfig struct {
	ProjectID        string `toml:"project_id"`
	SubscriptionName string `toml:"subscription_name"`
	CredentialsFile  string `toml:"credentials_file"`
}

type config struct {
	GCP gcpConfig `toml:"gcp"`
}

// event is the subset of google.chat.v1.Event (JSON form) the spike needs.
// Defined locally rather than via the generated chat types: the generated
// chat.User has no Email field, and the wire format is all we depend on.
// See https://developers.google.com/workspace/chat/api/reference/rest/v1/Event
type event struct {
	Type      string `json:"type"`
	EventTime string `json:"eventTime"`
	Space     struct {
		Name string `json:"name"`
	} `json:"space"`
	Thread struct {
		Name string `json:"name"`
	} `json:"thread"`
	Message struct {
		Text   string `json:"text"`
		Sender struct {
			Email string `json:"email"`
		} `json:"sender"`
		Thread struct {
			Name string `json:"name"`
		} `json:"thread"`
	} `json:"message"`
	User struct {
		Email string `json:"email"`
	} `json:"user"`
}

func main() {
	log.SetPrefix("probe: ")
	var (
		count      = flag.Int("count", 1, "number of events to process before exiting")
		timeout    = flag.Duration("timeout", 10*time.Minute, "max time to wait for events")
		configPath = flag.String("config", defaultConfig, "path to the pi-gchat TOML config ([gcp] section)")
		projectID  = flag.String("project", "", "override config: GCP project id")
		subName    = flag.String("subscription", "", "override config: Pub/Sub subscription name")
		credFile   = flag.String("credentials", "", "override config: service account key JSON path")
	)
	flag.Parse()

	cfg := loadConfig(*configPath)
	project := firstNonEmpty(*projectID, os.Getenv("GOOGLE_CLOUD_PROJECT"), cfg.GCP.ProjectID)
	subscription := firstNonEmpty(*subName, cfg.GCP.SubscriptionName)
	credentials := expandPath(firstNonEmpty(*credFile, cfg.GCP.CredentialsFile))
	if project == "" || subscription == "" {
		log.Fatalf("project and subscription are required; pass flags or set them in %s", *configPath)
	}
	// Note: psClient.Subscription() below takes the *short* id — the client
	// always wraps it as projects/<project>/subscriptions/<id>.

	log.Printf("spike: pulling %d event(s) from %s (timeout %s)", *count, fullResourceName(project, "subscriptions", subscription), *timeout)
	if credentials != "" {
		log.Printf("spike: authenticating with service account key %s", credentials)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	opts := clientOpts(credentials)
	psClient, err := pubsub.NewClient(ctx, project, opts...)
	if err != nil {
		log.Fatalf("pubsub client: %v", err)
	}
	defer psClient.Close()

	chatSvc, err := chat.NewService(ctx, append(opts, option.WithScopes(chat.ChatBotScope))...)
	if err != nil {
		log.Fatalf("chat client: %v", err)
	}

	got, err := receive(ctx, cancel, psClient.Subscription(subscription), chatSvc, *count)
	if err != nil {
		log.Fatalf("receive: %v", err)
	}
	if got < *count {
		log.Fatalf("timed out: processed %d/%d events — send the bot a message in Google Chat and retry", got, *count)
	}
	log.Printf("done: %d event(s) round-tripped", got)
}

// receive processes up to count Pub/Sub messages, replying to each via the
// Chat API, and cancels ctx once the quota is reached (or on first failure).
// It returns the number of events processed and the first error, if any.
func receive(ctx context.Context, cancel context.CancelFunc, sub *pubsub.Subscription, chatSvc *chat.Service, count int) (int, error) {
	var (
		mu       sync.Mutex
		got      int
		firstErr error
	)
	err := sub.Receive(ctx, func(ctx context.Context, msg *pubsub.Message) {
		if err := processOne(ctx, chatSvc, msg); err != nil {
			log.Printf("event failed (%v); nacking for redelivery", err)
			msg.Nack()
			mu.Lock()
			if firstErr == nil {
				firstErr = err
			}
			mu.Unlock()
			cancel()
			return
		}
		// Wait for the ack to flush to the stream before cancelling, so the
		// message is not redelivered after we exit.
		if err := ackAndWait(msg); err != nil {
			log.Printf("warning: ack: %v", err)
		}
		mu.Lock()
		got++
		done := got >= count
		mu.Unlock()
		if done {
			cancel()
		}
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return got, err
	}
	mu.Lock()
	defer mu.Unlock()
	return got, firstErr
}

// ackAndWait acks the message and blocks until the ack is flushed to the
// Pub/Sub stream (bounded, so a wedged stream cannot hang the callback).
func ackAndWait(msg *pubsub.Message) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := msg.AckWithResult().Get(ctx)
	return err
}

// processOne decodes a Pub/Sub payload, replies to it via the Chat API, and
// returns nil if the message should be acked.
func processOne(ctx context.Context, chatSvc *chat.Service, msg *pubsub.Message) error {
	if len(msg.Attributes) > 0 {
		log.Printf("attributes: %v", msg.Attributes)
	}
	var evt event
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		return fmt.Errorf("parse event: %w", err)
	}
	if evt.Type == "" {
		return errors.New("payload is not a Chat event (no type field)")
	}
	log.Printf("event: type=%s space=%s thread=%s user=%s",
		evt.Type, evt.Space.Name, threadName(&evt), senderEmail(&evt))

	text, reply := replyText(&evt)
	if !reply {
		log.Printf("event type %q needs no reply; acking", evt.Type)
		return nil
	}
	if evt.Space.Name == "" {
		return errors.New("event has no space")
	}
	posted := &chat.Message{Text: text}
	if thread := threadName(&evt); thread != "" {
		posted.Thread = &chat.Thread{Name: thread}
	}
	res, err := chatSvc.Spaces.Messages.Create(evt.Space.Name, posted).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("spaces.messages.create(%s): %w", evt.Space.Name, err)
	}
	log.Printf("replied: %s", res.Name)
	return nil
}

func replyText(evt *event) (string, bool) {
	switch evt.Type {
	case "MESSAGE":
		return fmt.Sprintf("✅ pi-gchat probe: round-trip OK\n· from: %s\n· you said: %q",
			senderEmail(evt), truncate(strings.TrimSpace(evt.Message.Text), 200)), true
	case "ADDED_TO_SPACE":
		return "✅ pi-gchat probe: round-trip OK — I receive events and can reply from this space.", true
	default:
		return "", false
	}
}

func threadName(evt *event) string {
	if evt.Thread.Name != "" {
		return evt.Thread.Name
	}
	return evt.Message.Thread.Name
}

func senderEmail(evt *event) string {
	if evt.User.Email != "" {
		return evt.User.Email
	}
	if evt.Message.Sender.Email != "" {
		return evt.Message.Sender.Email
	}
	return "unknown"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func loadConfig(path string) config {
	var cfg config
	path = expandPath(path)
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		if os.IsNotExist(err) {
			log.Printf("config %s not found; using flags/env only", path)
		} else {
			log.Printf("warning: config %s: %v", path, err)
		}
	} else {
		log.Printf("config: %s", path)
	}
	return cfg
}

func clientOpts(credentials string) []option.ClientOption {
	if credentials == "" {
		return nil // application default credentials
	}
	return []option.ClientOption{option.WithCredentialsFile(credentials)}
}

func fullResourceName(project, kind, name string) string {
	if strings.HasPrefix(name, "projects/") {
		return name
	}
	return fmt.Sprintf("projects/%s/%s/%s", project, kind, name)
}

func expandPath(p string) string {
	if p == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
