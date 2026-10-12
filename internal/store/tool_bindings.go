package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	ToolBindingActive  = "active"
	ToolBindingRevoked = "revoked"
)

// SessionToolBinding is the durable authorization record for one Pi session.
// It deliberately contains no messages, API responses, or credentials.
type SessionToolBinding struct {
	SessionPath      string
	Provider         string
	Surface          string
	WorkspaceID      string
	ChannelID        string
	ThreadTS         string
	Capabilities     []string
	ExtensionVersion string
	State            string
	CreatedAt        time.Time
}

// ErrToolBindingConflict means the session already has an active binding to a
// different chat origin. Rebinding must be an explicit, confirmed operation.
var ErrToolBindingConflict = errors.New("store: session already has a different chat-tools binding")

// CanonicalSessionPath canonicalizes the gateway's absolute session identifier
// without opening or resolving it on the filesystem. The broker never treats a
// model-supplied path as a file to inspect.
func CanonicalSessionPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return "", errors.New("store: session path must be absolute")
	}
	return filepath.Clean(path), nil
}

// SessionToolBinding reads the exact path-keyed binding. Unknown and revoked
// rows are returned distinctly so callers can fail closed without inferring an
// identity from request arguments.
func (s *Store) SessionToolBinding(ctx context.Context, sessionPath string) (SessionToolBinding, bool, error) {
	path, err := CanonicalSessionPath(sessionPath)
	if err != nil {
		return SessionToolBinding{}, false, err
	}
	var row SessionToolBinding
	var capabilities string
	var createdAt int64
	err = s.db.QueryRowContext(ctx, `
        SELECT session_path, provider, surface, workspace_id, channel_id, thread_ts,
               capabilities, extension_version, state, created_at
        FROM session_tool_bindings WHERE session_path = ?`, path).Scan(
		&row.SessionPath, &row.Provider, &row.Surface, &row.WorkspaceID,
		&row.ChannelID, &row.ThreadTS, &capabilities, &row.ExtensionVersion,
		&row.State, &createdAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return SessionToolBinding{}, false, nil
	case err != nil:
		return SessionToolBinding{}, false, fmt.Errorf("store: read tool binding: %w", err)
	}
	if err := json.Unmarshal([]byte(capabilities), &row.Capabilities); err != nil {
		return SessionToolBinding{}, false, fmt.Errorf("store: decode tool binding capabilities: %w", err)
	}
	row.CreatedAt = time.Unix(createdAt, 0).UTC()
	return row, true, nil
}

// BindSessionToolBinding stores an active binding. An identical active binding
// is idempotent; a different or revoked binding can be replaced only when the
// caller has obtained explicit confirmation.
func (s *Store) BindSessionToolBinding(ctx context.Context, row SessionToolBinding, confirmedRebind bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin tool binding: %w", err)
	}
	defer tx.Rollback()
	if err := bindToolBinding(ctx, tx, row, confirmedRebind); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit tool binding: %w", err)
	}
	return nil
}

// PutThreadWithToolBinding atomically commits the normal thread/session owner
// row and the separate tool authorization. A prompt cannot start with only one
// durable half of a newly created tool-enabled session.
func (s *Store) PutThreadWithToolBinding(ctx context.Context, thread ThreadRow, binding SessionToolBinding, confirmedRebind bool) error {
	threadPath, err := CanonicalSessionPath(thread.SessionPath)
	if err != nil {
		return fmt.Errorf("store: thread/tool binding needs a canonical thread session path: %w", err)
	}
	bindingPath, err := CanonicalSessionPath(binding.SessionPath)
	if err != nil {
		return err
	}
	if threadPath != bindingPath || thread.Surface != binding.Surface ||
		thread.WorkspaceID != binding.WorkspaceID || thread.ChannelID != binding.ChannelID || thread.ThreadTS != binding.ThreadTS {
		return errors.New("store: thread owner and tool binding identity differ")
	}
	thread.SessionPath, binding.SessionPath = threadPath, bindingPath
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin thread/tool binding transaction: %w", err)
	}
	defer tx.Rollback()
	if err := putThread(ctx, tx, thread); err != nil {
		return err
	}
	if err := bindToolBinding(ctx, tx, binding, confirmedRebind); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit thread/tool binding: %w", err)
	}
	return nil
}

func bindToolBinding(ctx context.Context, exec execContext, row SessionToolBinding, confirmedRebind bool) error {
	path, err := CanonicalSessionPath(row.SessionPath)
	if err != nil {
		return err
	}
	row.SessionPath = path
	if strings.TrimSpace(row.Provider) == "" || strings.TrimSpace(row.Surface) == "" ||
		strings.TrimSpace(row.WorkspaceID) == "" || strings.TrimSpace(row.ChannelID) == "" ||
		strings.TrimSpace(row.ThreadTS) == "" || strings.TrimSpace(row.ExtensionVersion) == "" {
		return errors.New("store: incomplete tool binding")
	}
	if row.State == "" {
		row.State = ToolBindingActive
	}
	if row.State != ToolBindingActive {
		return fmt.Errorf("store: new tool binding state must be %q", ToolBindingActive)
	}
	caps := slices.Clone(row.Capabilities)
	if len(caps) == 0 {
		return errors.New("store: tool binding needs at least one capability")
	}
	for i, capability := range caps {
		if strings.TrimSpace(capability) == "" {
			return errors.New("store: empty tool binding capability")
		}
		for j := range i {
			if caps[j] == capability {
				return fmt.Errorf("store: duplicate tool binding capability %q", capability)
			}
		}
	}
	slices.Sort(caps)
	encoded, err := json.Marshal(caps)
	if err != nil {
		return fmt.Errorf("store: encode tool binding capabilities: %w", err)
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now()
	}

	var old SessionToolBinding
	var oldCaps string
	err = queryToolBinding(ctx, exec, path, &old, &oldCaps)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = exec.ExecContext(ctx, `
            INSERT INTO session_tool_bindings
                (session_path, provider, surface, workspace_id, channel_id, thread_ts,
                 capabilities, extension_version, state, created_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			path, row.Provider, row.Surface, row.WorkspaceID, row.ChannelID, row.ThreadTS,
			string(encoded), row.ExtensionVersion, ToolBindingActive, row.CreatedAt.Unix())
		if err != nil {
			return fmt.Errorf("store: insert tool binding: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("store: inspect existing tool binding: %w", err)
	}
	if old.State == ToolBindingActive && sameBindingOwner(old, row) {
		_, err = exec.ExecContext(ctx, `
            UPDATE session_tool_bindings
            SET capabilities = ?, extension_version = ? WHERE session_path = ?`,
			string(encoded), row.ExtensionVersion, path)
		if err != nil {
			return fmt.Errorf("store: update tool binding: %w", err)
		}
		return nil
	}
	if !confirmedRebind {
		return ErrToolBindingConflict
	}
	_, err = exec.ExecContext(ctx, `
        UPDATE session_tool_bindings
        SET provider = ?, surface = ?, workspace_id = ?, channel_id = ?, thread_ts = ?,
            capabilities = ?, extension_version = ?, state = ?, created_at = ?
        WHERE session_path = ?`,
		row.Provider, row.Surface, row.WorkspaceID, row.ChannelID, row.ThreadTS,
		string(encoded), row.ExtensionVersion, ToolBindingActive, row.CreatedAt.Unix(), path)
	if err != nil {
		return fmt.Errorf("store: rebind session tool binding: %w", err)
	}
	return nil
}

func queryToolBinding(ctx context.Context, exec interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, path string, row *SessionToolBinding, capabilities *string) error {
	var createdAt int64
	err := exec.QueryRowContext(ctx, `
        SELECT provider, surface, workspace_id, channel_id, thread_ts,
               capabilities, extension_version, state, created_at
        FROM session_tool_bindings WHERE session_path = ?`, path).Scan(
		&row.Provider, &row.Surface, &row.WorkspaceID, &row.ChannelID, &row.ThreadTS,
		capabilities, &row.ExtensionVersion, &row.State, &createdAt)
	if err == nil {
		row.SessionPath = path
		row.CreatedAt = time.Unix(createdAt, 0).UTC()
	}
	return err
}

func sameBindingOwner(a, b SessionToolBinding) bool {
	return a.Provider == b.Provider && a.Surface == b.Surface &&
		a.WorkspaceID == b.WorkspaceID && a.ChannelID == b.ChannelID && a.ThreadTS == b.ThreadTS
}

// RevokeSessionToolBinding leaves the metadata for audit/recovery but disables
// the session's tool authority. Revoking an unknown path is an idempotent no-op.
func (s *Store) RevokeSessionToolBinding(ctx context.Context, sessionPath string) error {
	path, err := CanonicalSessionPath(sessionPath)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE session_tool_bindings SET state = ? WHERE session_path = ?`, ToolBindingRevoked, path); err != nil {
		return fmt.Errorf("store: revoke tool binding: %w", err)
	}
	return nil
}
