package workspace

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// discardLogger is a logger that throws its output away.
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// newProvisioner returns a provisioner rooted in two temporary directories.
func newProvisioner(t *testing.T, template string) (*Provisioner, string, string) {
	t.Helper()
	projects := filepath.Join(t.TempDir(), "work")
	repos := filepath.Join(t.TempDir(), "code")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(repos, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p := New(Config{ProjectsRoot: projects, ReposRoot: repos, InjectedPrompt: template}, discardLogger())
	return p, projects, repos
}

func TestSlugFromPrompt(t *testing.T) {
	tests := []struct{ prompt, want string }{
		{"fix the failing test", "fix-the-failing-test"},
		{"  Fix   the FAILING test!  ", "fix-the-failing-test"},
		{`"quoted prompt" and 'more'`, "quoted-prompt-and-more"},
		{"one two three four five six seven eight", "one-two-three-four-five-six"},
		{"", "session"},
		{"!!! ??? ...", "session"},
		{"averyveryveryverylongsinglewordthatkeepsgoing", "averyveryveryverylongsinglewordthatkeeps"},
		{"--leading and trailing--", "leading-and-trailing"},
		{"多字节 字符 é", "多字节-字符-é"},
	}
	for _, test := range tests {
		if got := slugFromPrompt(test.prompt); got != test.want {
			t.Errorf("slugFromPrompt(%q) = %q, want %q", test.prompt, got, test.want)
		}
	}
}

func TestProvisionCreatesAFreshDirectoryPerSession(t *testing.T) {
	p, projects, _ := newProvisioner(t, "")

	first, err := p.Provision("fix the tests")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !strings.HasPrefix(first.Dir, projects+string(filepath.Separator)) {
		t.Errorf("project dir %s is outside %s", first.Dir, projects)
	}
	if !strings.HasSuffix(first.Dir, "-fix-the-tests") {
		t.Errorf("project dir %s does not carry the prompt slug", first.Dir)
	}
	if info, err := os.Stat(first.Dir); err != nil || !info.IsDir() {
		t.Fatalf("the project directory was not created: %v", err)
	}

	// The same prompt twice must not share a working tree.
	second, err := p.Provision("fix the tests")
	if err != nil {
		t.Fatalf("Provision (second): %v", err)
	}
	if second.Dir == first.Dir {
		t.Fatalf("two sessions got the same directory %s", first.Dir)
	}
	if !strings.HasSuffix(second.Dir, "-2") {
		t.Errorf("the second directory is %s, want a -2 suffix", second.Dir)
	}
}

func TestProvisionNeedsAProjectsRoot(t *testing.T) {
	p := New(Config{}, discardLogger())
	if _, err := p.Provision("anything"); err == nil {
		t.Fatal("Provision worked without a projects root")
	}
}

func TestPiArgsRenderEveryPlaceholder(t *testing.T) {
	p, projects, repos := newProvisioner(t,
		"work in {projectsRoot}/repos/<repo>; repos live in {reposRoot}; "+
			"branch pi/{date}-{slug}")

	project, err := p.Provision("add a retry")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	args := p.PiArgs(project)
	if len(args) != 2 || args[0] != "--append-system-prompt" {
		t.Fatalf("PiArgs = %v, want --append-system-prompt and the prompt", args)
	}
	prompt := args[1]
	for _, want := range []string{projects, repos, project.Date, "add-a-retry"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the rendered prompt is missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "{") {
		t.Errorf("a placeholder was left unrendered:\n%s", prompt)
	}
}

func TestPiArgsWithoutATemplate(t *testing.T) {
	for _, template := range []string{"", "   \n"} {
		p, _, _ := newProvisioner(t, template)
		project, err := p.Provision("anything")
		if err != nil {
			t.Fatalf("Provision: %v", err)
		}
		if args := p.PiArgs(project); args != nil {
			t.Errorf("PiArgs with template %q = %v, want none", template, args)
		}
	}
}

func TestSweepRemovesOnlyOrphansThatLookOurs(t *testing.T) {
	ctx := context.Background()
	p, projects, _ := newProvisioner(t, "")

	// A live thread's directory: kept because the caller's state names it.
	live, err := p.Provision("live thread")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// An orphan: no row, no content.
	orphan, err := p.Provision("orphan")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// An orphan with a file the agent wrote outside repos/: kept, because
	// deleting it would delete work.
	dirty, err := p.Provision("dirty orphan")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirty.Dir, "notes.md"), []byte("my notes"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// A directory that is not ours: never touched, whatever it contains.
	stray := filepath.Join(projects, "not-a-project")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A kept directory whose *name* looks like ours but predates our rows.
	old := filepath.Join(projects, "2020-01-01-old")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	removed, err := p.Sweep(ctx, map[string]bool{live.Dir: true})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(removed) != 2 {
		t.Errorf("Sweep removed %v, want the orphan and the old directory", removed)
	}
	for _, path := range []string{live.Dir, dirty.Dir, stray} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("Sweep removed %s, which it should have kept", path)
		}
	}
	if _, err := os.Stat(orphan.Dir); !os.IsNotExist(err) {
		t.Errorf("Sweep left the orphan %s", orphan.Dir)
	}
}

func TestIsProjectName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"2026-09-27-fix-the-tests", true},
		{"2026-09-27-session", true},
		{"2026-09-27-fix-2", true},
		{"not-a-project", false},
		{"2026-9-27-short-month", false},
		{"2026-09-27-", false},
		{"", false},
	}
	for _, test := range tests {
		if got := isProjectName(test.name); got != test.want {
			t.Errorf("isProjectName(%q) = %v, want %v", test.name, got, test.want)
		}
	}
}
