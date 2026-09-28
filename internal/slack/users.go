package slack

import (
	"context"
	"log/slog"
	"sync"
)

// labeler resolves user IDs to the names shown beside their messages in a
// transcript, one call per person per process.
//
// Names need `users:read`. Reading the thread needs the `*:history` scopes
// instead, so a transcript arrives either way: without the name scope its lines
// read `[U123] text`, which is what the mention form `<@U123>` is made of, and
// the instruction says so. That fallback is the design rather than an accident —
// a name is a nicety, and it must not cost a failed request per message per
// turn, so the first refusal stops the asking for the life of the process
// (DESIGN.md §4).
type labeler struct {
	api *API
	log *slog.Logger

	mu      sync.Mutex
	names   map[string]string
	refused bool
}

func newLabeler(api *API, log *slog.Logger) *labeler {
	return &labeler{api: api, log: log, names: map[string]string{}}
}

// name returns the display name of a user, or "" when there is none to give.
func (l *labeler) name(ctx context.Context, userID string) string {
	if userID == "" {
		return ""
	}
	l.mu.Lock()
	name, cached := l.names[userID]
	refused := l.refused
	l.mu.Unlock()
	if cached {
		return name
	}
	if refused {
		return ""
	}

	name, err := l.api.UserInfo(ctx, userID)
	switch {
	case err == nil:
		l.remember(userID, name)
		return name
	case capabilityUnavailable(err):
		l.mu.Lock()
		l.refused = true
		l.mu.Unlock()
		l.log.Warn("transcripts will show user IDs rather than names: "+
			"the app cannot call users.info", "error", err)
		return ""
	case IsCode(err, "user_not_found"):
		// A deactivated account, or somebody from another workspace. Remember
		// the answer: asking again every turn would not change it.
		l.remember(userID, "")
		return ""
	default:
		// A transient failure. Do not cache it — the next turn can ask again,
		// and the transcript for this one uses the ID.
		l.log.Debug("cannot resolve a user's name", "user", userID, "error", err)
		return ""
	}
}

func (l *labeler) remember(userID, name string) {
	l.mu.Lock()
	l.names[userID] = name
	l.mu.Unlock()
}
