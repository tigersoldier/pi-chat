// Package workspace provisions the directories pi works in, and cleans them
// up (DESIGN.md §9).
//
// A session gets a fresh project directory. Inside it the agent creates a git
// worktree of a real repository, so two threads working on one repository
// never collide and the user's own checkout is never touched. Nothing here
// talks to pi or to a chat platform: it is filesystem and git work.
//
// The convention is expressed to the agent as an injected system prompt — the
// template names the directories and the branch to use — so the layout is
// stated once, in configuration, rather than assumed by the code.
package workspace

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
)

// reposDirName is the directory inside a project where worktrees live. The
// injected prompt tells the agent to put them here, and Cleanup looks for them
// here and nowhere else.
const reposDirName = "repos"

// Config is the workspace convention, from the [paths] and [behavior] sections.
type Config struct {
	ProjectsRoot string // one subdirectory per session
	ReposRoot    string // where the real repositories live
	// InjectedPrompt is a template appended to the agent's system prompt. An
	// empty template disables injection entirely: the agent then decides where
	// to work, and pi-chat only owns the project directory.
	InjectedPrompt string
}

// Provisioner creates and removes project directories.
type Provisioner struct {
	cfg Config
	log *slog.Logger

	// now is a test seam for the date in a project directory's name.
	now func() time.Time
}

// New builds a Provisioner. A nil logger discards.
func New(cfg Config, log *slog.Logger) *Provisioner {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Provisioner{cfg: cfg, log: log, now: time.Now}
}

// Project is one provisioned directory.
type Project struct {
	Dir  string // absolute path, which becomes the session's working directory
	Slug string // the prompt-derived part of the name
	Date string // YYYY-MM-DD, local time
}

// Name is the directory's base name, as the injected prompt's {slug} and
// {date} describe it.
func (p Project) Name() string { return filepath.Base(p.Dir) }

// Repos lists the repositories directly under the repos root, sorted by name.
//
// It is what makes a suggested prompt reflect this machine instead of a
// hard-coded list: the suggestions name repositories that are actually here.
// Only directories that look like a repository count — a `.git` file (a linked
// worktree) or a `.git` directory — and a root that cannot be read is simply no
// repositories, not an error worth failing an interaction over.
func (p *Provisioner) Repos() []string {
	root := p.cfg.ReposRoot
	if root == "" {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		p.log.Debug("cannot list the repos root", "root", root, "error", err)
		return nil
	}
	var repos []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, name, ".git")); err != nil {
			continue
		}
		repos = append(repos, name)
	}
	slices.Sort(repos)
	return repos
}

// Provision creates a project directory for a new session. The name comes from
// the first prompt and the date, and a collision gets a counter rather than a
// shared directory: two threads must never share one working tree.
func (p *Provisioner) Provision(prompt string) (Project, error) {
	project := Project{
		Slug: slugFromPrompt(prompt),
		Date: p.now().Format("2006-01-02"),
	}
	if p.cfg.ProjectsRoot == "" {
		return Project{}, fmt.Errorf("workspace: no projects root configured")
	}
	if err := os.MkdirAll(p.cfg.ProjectsRoot, 0o755); err != nil {
		return Project{}, fmt.Errorf("workspace: create %s: %w", p.cfg.ProjectsRoot, err)
	}

	// Mkdir, not MkdirAll: the loop needs to know whether *this* name was free,
	// so it can try the next one instead of silently joining another session's
	// directory.
	for attempt := 1; attempt <= 100; attempt++ {
		name := fmt.Sprintf("%s-%s", project.Date, project.Slug)
		if attempt > 1 {
			name = fmt.Sprintf("%s-%s-%d", project.Date, project.Slug, attempt)
		}
		dir := filepath.Join(p.cfg.ProjectsRoot, name)
		if err := os.Mkdir(dir, 0o755); err == nil {
			project.Dir = dir
			return project, nil
		} else if !os.IsExist(err) {
			return Project{}, fmt.Errorf("workspace: create %s: %w", dir, err)
		}
	}
	return Project{}, fmt.Errorf("workspace: no free directory name for %s-%s",
		project.Date, project.Slug)
}

// PiArgs returns the extra pi arguments a project needs: the injected prompt,
// rendered with this project's values. The caller adds the user's own
// gateway.pi_args on top.
func (p *Provisioner) PiArgs(project Project) []string {
	if strings.TrimSpace(p.cfg.InjectedPrompt) == "" || project.Dir == "" {
		return nil
	}
	return []string{"--append-system-prompt", p.render(project)}
}

// render fills the template's placeholders. The set is fixed and documented in
// DESIGN.md §9 and pi-chat.toml.example: an unknown {name} is left as it is,
// because the prompt may well contain braces of its own.
func (p *Provisioner) render(project Project) string {
	return strings.NewReplacer(
		"{projectsRoot}", p.cfg.ProjectsRoot,
		"{reposRoot}", p.cfg.ReposRoot,
		"{date}", project.Date,
		"{slug}", project.Slug,
	).Replace(p.cfg.InjectedPrompt)
}

// slugFromPrompt builds a short, filesystem- and branch-safe name from the
// first words of a prompt, so a project directory says what it is for.
func slugFromPrompt(prompt string) string {
	const (
		maxWords = 6
		maxLen   = 40
	)
	var words []string
	for _, field := range strings.Fields(prompt) {
		if word := slugWord(field); word != "" {
			words = append(words, word)
		}
		if len(words) == maxWords {
			break
		}
	}
	slug := truncateRunes(strings.Join(words, "-"), maxLen)
	// A prompt of punctuation, or one in a script whose words reduce to
	// nothing, still needs a directory.
	if slug == "" {
		return "session"
	}
	return slug
}

// slugWord reduces one word to letters, digits and inner dashes. Leading and
// trailing punctuation falls away, which is what makes "-fix" out of a quoted
// or emphasised prompt.
func slugWord(word string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(word) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// truncateRunes cuts s to at most n runes, dropping a dash the cut exposed.
func truncateRunes(s string, n int) string {
	if len([]rune(s)) <= n {
		return strings.Trim(s, "-")
	}
	runes := []rune(s)[:n]
	return strings.Trim(string(runes), "-")
}
