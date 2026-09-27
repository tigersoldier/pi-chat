package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// gitTimeout bounds one git invocation: cleanup runs on delete and on startup,
// and a hung git must not hang either.
const gitTimeout = 30 * time.Second

// CleanupResult reports what a cleanup did, so callers can log it and tests
// can assert it.
type CleanupResult struct {
	RemovedWorktrees []string
	PrunedBranches   []string
	RemovedDir       bool
	// LeftBehind is what remains: worktrees pi-chat declined to touch (a real
	// checkout, or a branch that is not pi-chat's), files the agent wrote, and
	// the project directory when it did not end up empty. A non-empty
	// LeftBehind is normal, never an error.
	LeftBehind []string
}

// Cleanup removes the worktrees under a project directory, prunes the branch
// each one was on, and removes the directories that end up empty.
//
// It deliberately touches only linked worktrees sitting on a `pi/` branch: a
// real clone the agent made, or a branch someone else named, may be work worth
// keeping, and a cleanup path that deletes the wrong repository is worse than
// a directory that outlives its session.
func (p *Provisioner) Cleanup(ctx context.Context, dir string) (CleanupResult, error) {
	var result CleanupResult
	var errs []error

	reposDir := filepath.Join(dir, reposDirName)
	entries, err := os.ReadDir(reposDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return result, fmt.Errorf("workspace: read %s: %w", reposDir, err)
	}
	for _, entry := range entries {
		path := filepath.Join(reposDir, entry.Name())
		if !entry.IsDir() {
			result.LeftBehind = append(result.LeftBehind, path)
			continue
		}
		removed, branch, err := p.removeWorktree(ctx, path)
		switch {
		case removed:
			result.RemovedWorktrees = append(result.RemovedWorktrees, path)
			if branch != "" {
				result.PrunedBranches = append(result.PrunedBranches, branch)
			}
		default:
			result.LeftBehind = append(result.LeftBehind, path)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}

	p.removeDirIfEmpty(reposDir)
	result.RemovedDir = p.removeDirIfEmpty(dir)
	if !result.RemovedDir {
		// Leftovers inside repos/ are already listed one by one; naming the
		// directory as well would just repeat them.
		result.LeftBehind = append(result.LeftBehind,
			listDirExcept(dir, reposDirName)...)
	}
	return result, errors.Join(errs...)
}

// Sweep removes project directories that no live thread owns and returns the
// directories it removed.
//
// keep is the set of project directories the caller's state still refers to,
// read from the threads table. This is the crash-recovery path: a session
// created just before a crash, or the remains of an interrupted delete, has no
// row and would otherwise sit in projects_root forever. Only directories that
// look like pi-chat's own are considered, so an unrelated directory dropped
// into projects_root is never touched.
func (p *Provisioner) Sweep(ctx context.Context, keep map[string]bool) ([]string, error) {
	if p.cfg.ProjectsRoot == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(p.cfg.ProjectsRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("workspace: read %s: %w", p.cfg.ProjectsRoot, err)
	}

	var (
		removed []string
		errs    []error
	)
	for _, entry := range entries {
		if !entry.IsDir() || !isProjectName(entry.Name()) {
			continue
		}
		dir := filepath.Join(p.cfg.ProjectsRoot, entry.Name())
		if keep[dir] {
			continue
		}
		result, err := p.Cleanup(ctx, dir)
		if err != nil {
			errs = append(errs, err)
		}
		switch {
		case result.RemovedDir:
			removed = append(removed, dir)
			p.log.Info("swept an orphaned project directory",
				"dir", dir,
				"worktrees", len(result.RemovedWorktrees),
				"branches", result.PrunedBranches)
		case len(result.LeftBehind) > 0:
			p.log.Warn("an orphaned project directory is not empty; leaving it",
				"dir", dir, "left", result.LeftBehind)
		}
	}
	return removed, errors.Join(errs...)
}

// isProjectName reports whether a directory name is one Provision could have
// created: <YYYY-MM-DD>-<slug>.
func isProjectName(name string) bool {
	if len(name) < 12 || name[10] != '-' {
		return false
	}
	if _, err := time.Parse("2006-01-02", name[:10]); err != nil {
		return false
	}
	return strings.Trim(name[11:], "-") != ""
}

// removeWorktree removes one linked worktree and prunes the branch it was on.
// It reports whether the worktree went and which branch was pruned.
func (p *Provisioner) removeWorktree(ctx context.Context, path string) (bool, string, error) {
	gitDir, err := p.git(ctx, path, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		// Not a git checkout: not pi-chat's to remove.
		p.log.Debug("not a git checkout; leaving it", "path", path)
		return false, "", nil
	}
	commonDir, err := p.git(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return false, "", err
	}
	if samePath(gitDir, commonDir) {
		// A main checkout rather than a worktree: the agent cloned a
		// repository here and may be using it directly.
		p.log.Debug("a main checkout, not a worktree; leaving it", "path", path)
		return false, "", nil
	}
	branch, err := p.git(ctx, path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return false, "", err
	}
	if !strings.HasPrefix(branch, "pi/") {
		p.log.Warn("leaving a worktree whose branch is not pi-chat's",
			"path", path, "branch", branch)
		return false, "", nil
	}

	mainRepo := filepath.Dir(commonDir)
	if _, err := p.git(ctx, mainRepo, "worktree", "remove", "--force", path); err != nil {
		return false, "", err
	}
	// The branch is pruned after the worktree that had it checked out. -D, not
	// -d: a session's branch is expected to be unmerged work, which is the
	// whole point of the worktree, and the agent was told to push it.
	if _, err := p.git(ctx, mainRepo, "branch", "-D", branch); err != nil {
		return true, "", err
	}
	return true, branch, nil
}

// git runs one git command in dir and returns its trimmed standard output.
func (p *Provisioner) git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("workspace: git %s in %s: %s", strings.Join(args, " "), dir, message)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// removeDirIfEmpty removes dir when it has no entries, and reports whether it
// did. A missing directory counts as removed.
func (p *Provisioner) removeDirIfEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return errors.Is(err, fs.ErrNotExist)
	}
	if len(entries) > 0 {
		return false
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.log.Warn("cannot remove an empty directory", "dir", dir, "error", err)
		return false
	}
	return true
}

// listDirExcept lists the paths directly inside dir, skipping one entry by
// base name and reporting nothing for a directory that is gone.
func listDirExcept(dir, skip string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var paths []string
	for _, entry := range entries {
		if entry.Name() == skip {
			continue
		}
		paths = append(paths, filepath.Join(dir, entry.Name()))
	}
	return paths
}

// samePath compares two paths as git reports them.
func samePath(a, b string) bool {
	if a == b {
		return true
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	return errA == nil && errB == nil && absA == absB
}
