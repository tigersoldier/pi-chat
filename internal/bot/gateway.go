package bot

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/tigersoldier/pi-gateway/gwclient"

	"github.com/tigersoldier/pi-chat/internal/config"
	"github.com/tigersoldier/pi-chat/internal/store"
)

// gatewayTimeout bounds a read-only gateway call. Every one of them is on the
// path between a command and its answer.
const gatewayTimeout = 10 * time.Second

// gateway is the read-only view of pi-gatewayd the core needs.
//
// It is an interface for two reasons: the core can be tested without a running
// daemon, and the read path is kept apart from the lifecycle operations, which
// dial with the admin token and bind sessions (DESIGN.md §10). A session is
// created only through thread.create, which is deliberately not part of this.
type gateway interface {
	// Sessions lists the session catalog, newest first.
	Sessions(ctx context.Context) ([]gwclient.SessionRow, error)
	// Status describes the daemon for /pi status.
	Status(ctx context.Context) string
}

// connector is the real gateway: one throwaway connection per read.
//
// It dials with the thread token, not the admin one — reading the catalog
// needs only `observe`, and it deliberately issues nothing that would bind a
// session, because an extra attached client is what makes a session
// unevictable (DESIGN.md §8, §10).
type connector struct {
	cfg *config.Config
	log *slog.Logger
}

// connect opens one session-less connection; the caller closes it.
func (c *connector) connect(ctx context.Context, what string) (*gwclient.Client, error) {
	ctx, cancel := context.WithTimeout(ctx, gatewayTimeout)
	defer cancel()
	client, err := gwclient.Dial(ctx, gwclient.Config{
		StateDir:  c.cfg.Gateway.StateDir,
		TokenFile: c.cfg.Gateway.ThreadTokenFile,
		Name:      "pi-chat " + what,
		Kind:      kind,
	})
	if err != nil {
		return nil, err
	}
	return client, nil
}

// Sessions reads the catalog.
func (c *connector) Sessions(ctx context.Context) ([]gwclient.SessionRow, error) {
	client, err := c.connect(ctx, "catalog")
	if err != nil {
		return nil, err
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(ctx, gatewayTimeout)
	defer cancel()
	rows, err := client.ListSessions(ctx, gwclient.SessionFilter{})
	if err != nil {
		return nil, fmt.Errorf("read the session catalog: %w", err)
	}
	return rows, nil
}

// Status is the gateway half of /pi status: whether it answers, which pi it
// runs, and what its catalog holds. It reports failures in the string rather
// than as an error: a status message that cannot say "unreachable" is not much
// of a status message.
func (c *connector) Status(ctx context.Context) string {
	client, err := c.connect(ctx, "status")
	if err != nil {
		return "gateway: *unreachable* — " + err.Error()
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(ctx, gatewayTimeout)
	defer cancel()
	rows, err := client.ListSessions(ctx, gwclient.SessionFilter{})
	if err != nil {
		return "gateway: reachable, but the catalog did not answer — " + err.Error()
	}
	var live, mine int
	for _, row := range rows {
		if row.Live {
			live++
		}
		if row.CreatedBy != nil && row.CreatedBy.Kind == kind {
			mine++
		}
	}
	return fmt.Sprintf("gateway: reachable, pi %s, %d sessions (%d live, %d created by pi-chat)",
		client.PiVersion(), len(rows), live, mine)
}

// adoptable lists the sessions `/pi resume` could take over: sessions the
// gateway knows, that no thread of pi-chat owns, and that pi-chat did not
// create itself.
func (b *Bot) adoptable(ctx context.Context) ([]gwclient.SessionRow, error) {
	rows, err := b.gw.Sessions(ctx)
	if err != nil {
		return nil, err
	}
	owned, err := b.ownedSessions(ctx)
	if err != nil {
		return nil, err
	}

	var out []gwclient.SessionRow
	for _, row := range rows {
		if owned[row.Path] {
			continue
		}
		if b.ownProject(row.Cwd) {
			// A session of ours whose thread is not in this database: either
			// the database was lost or the row was deleted. Adopting it would
			// put a second thread on one session (DESIGN.md §9).
			continue
		}
		out = append(out, row)
	}
	if len(out) > resumeLimit {
		out = out[:resumeLimit]
	}
	return out, nil
}

// ownedSessions is the set of session paths the threads table accounts for.
// A row in the deleted state keeps its key but no longer owns its session, so
// it does not hold one here.
func (b *Bot) ownedSessions(ctx context.Context) (map[string]bool, error) {
	rows, err := b.store.Threads(ctx)
	if err != nil {
		return nil, err
	}
	owned := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row.SessionPath != "" && row.State != store.StateDeleted {
			owned[row.SessionPath] = true
		}
	}
	return owned, nil
}

// ownProject reports whether a directory is one pi-chat provisions. It is the
// structural test for "this session is already reachable in Slack", and the
// reason pi-chat's own sessions are never offered for adoption (DESIGN.md §9).
func (b *Bot) ownProject(dir string) bool {
	root := b.cfg.Paths.ProjectsRoot
	if dir == "" || root == "" {
		return false
	}
	return dir == root || strings.HasPrefix(dir, root+string(filepath.Separator))
}
