package workspace

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// git runs a git command in dir, failing the test if it does not succeed.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

// newRepo creates a git repository with one commit and returns its path. The
// tests in this file need real repositories: the cleanup path is git's
// behaviour as much as pi-chat's.
func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := filepath.Join(t.TempDir(), "main")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	git(t, dir, "init", "--quiet", "-b", "main", ".")
	git(t, dir, "config", "user.email", "pi-chat@example.invalid")
	git(t, dir, "config", "user.name", "pi-chat test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "--quiet", "-m", "init")
	return dir
}

// newProjectWithWorktree provisions a project and puts a worktree of repo
// inside it, the way the injected prompt tells the agent to.
func newProjectWithWorktree(t *testing.T, repo, branch string) (*Provisioner, Project, string) {
	t.Helper()
	p, _, _ := newProvisioner(t, "")
	project, err := p.Provision("work on something")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	worktree := filepath.Join(project.Dir, "repos", filepath.Base(repo))
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	git(t, repo, "worktree", "add", "--quiet", "-b", branch, worktree, "main")
	return p, project, worktree
}

func TestCleanupRemovesWorktreeAndBranch(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)
	branch := "pi/2026-09-27-work-on-something"
	p, project, worktree := newProjectWithWorktree(t, repo, branch)

	result, err := p.Cleanup(ctx, project.Dir)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(result.RemovedWorktrees) != 1 || result.RemovedWorktrees[0] != worktree {
		t.Errorf("RemovedWorktrees = %v, want [%s]", result.RemovedWorktrees, worktree)
	}
	if len(result.PrunedBranches) != 1 || result.PrunedBranches[0] != branch {
		t.Errorf("PrunedBranches = %v, want [%s]", result.PrunedBranches, branch)
	}
	if !result.RemovedDir {
		t.Errorf("the project directory survived: %v", result.LeftBehind)
	}
	if _, err := os.Stat(project.Dir); !os.IsNotExist(err) {
		t.Errorf("the project directory still exists")
	}
	// The branch is really gone from the main repository, and the repository
	// itself is untouched.
	branches := git(t, repo, "branch", "--list", branch)
	if branches != "" {
		t.Errorf("branch %s still exists: %q", branch, branches)
	}
	if _, err := os.Stat(filepath.Join(repo, "README.md")); err != nil {
		t.Errorf("the main checkout was damaged: %v", err)
	}
}

func TestCleanupLeavesAWorktreeOnAForeignBranch(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)
	p, project, worktree := newProjectWithWorktree(t, repo, "feature/someone-elses-work")

	result, err := p.Cleanup(ctx, project.Dir)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(result.RemovedWorktrees) != 0 {
		t.Errorf("Cleanup removed %v", result.RemovedWorktrees)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Errorf("the worktree was removed anyway: %v", err)
	}
	if result.RemovedDir {
		t.Error("the project directory was removed although work remained")
	}
	if len(result.LeftBehind) == 0 {
		t.Error("Cleanup did not report what it left behind")
	}
	if branches := git(t, repo, "branch", "--list", "feature/someone-elses-work"); branches == "" {
		t.Error("the foreign branch was pruned")
	}
}

func TestCleanupLeavesAMainCheckout(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)
	p, _, _ := newProvisioner(t, "")
	project, err := p.Provision("clone something")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// The agent cloned a repository instead of adding a worktree. That clone
	// is a main checkout, not pi-chat's worktree, and must survive.
	reposDir := filepath.Join(project.Dir, "repos")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	clone := filepath.Join(reposDir, "clone")
	git(t, reposDir, "clone", "--quiet", repo, clone)
	uncommitted := filepath.Join(clone, "scratch.txt")
	if err := os.WriteFile(uncommitted, []byte("work in progress"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := p.Cleanup(ctx, project.Dir)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(result.RemovedWorktrees) != 0 {
		t.Errorf("Cleanup removed %v", result.RemovedWorktrees)
	}
	if _, err := os.Stat(uncommitted); err != nil {
		t.Errorf("the clone's contents were deleted: %v", err)
	}
	if result.RemovedDir {
		t.Error("the project directory was removed although a clone remained")
	}
}

func TestCleanupRemovesAnEmptyProjectWithoutGit(t *testing.T) {
	ctx := context.Background()
	p, _, _ := newProvisioner(t, "")
	project, err := p.Provision("nothing was created")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// A directory the agent made but never turned into a repository.
	stray := filepath.Join(project.Dir, "repos", "not-a-repo")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stray, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := p.Cleanup(ctx, project.Dir)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if result.RemovedDir {
		t.Error("Cleanup removed a directory that still held a file")
	}
	// What it declines to remove is the directory it could not classify; the
	// file inside is still there, which is the point.
	if len(result.LeftBehind) != 1 || !strings.HasSuffix(result.LeftBehind[0], "not-a-repo") {
		t.Errorf("LeftBehind = %v, want the directory it kept", result.LeftBehind)
	}
	if _, err := os.Stat(filepath.Join(stray, "file.txt")); err != nil {
		t.Errorf("the file inside was deleted: %v", err)
	}
}

func TestCleanupOnAMissingDirectory(t *testing.T) {
	p, projects, _ := newProvisioner(t, "")
	result, err := p.Cleanup(context.Background(), filepath.Join(projects, "2026-01-01-gone"))
	if err != nil {
		t.Fatalf("Cleanup on a missing directory: %v", err)
	}
	if !result.RemovedDir {
		t.Error("a directory that is not there should count as removed")
	}
}

func TestSweepUsesTheProjectsRootOnly(t *testing.T) {
	p, projects, _ := newProvisioner(t, "")
	// A projects root that does not exist yet is not an error: nothing to
	// sweep is the normal state on a fresh install.
	if err := os.RemoveAll(projects); err != nil {
		t.Fatalf("remove: %v", err)
	}
	removed, err := p.Sweep(context.Background(), nil)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("Sweep removed %v from a missing root", removed)
	}
}
