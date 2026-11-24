// Package source provides desired state from git or a directory.
package source

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Source provides the desired-state directory.
type Source interface {
	// Sync updates the directory and returns its revision.
	Sync(ctx context.Context) (string, error)
	// Read runs fn while the directory is stable.
	Read(fn func(dir string) error) error
}

// Proposer can push branches for review.
type Proposer interface {
	Source
	// Propose always returns the checkout to the tracked branch.
	Propose(ctx context.Context, branch, message string, files map[string][]byte) (bool, error)
	// DeleteBranch removes a pushed branch.
	DeleteBranch(ctx context.Context, branch string) error
	// Base is the tracked branch.
	Base() string
}

// Dir reads a plain directory; its revision is a content hash.
type Dir struct{ Path string }

// Sync hashes the directory.
func (d Dir) Sync(context.Context) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(d.Path, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() || !isYAML(p) {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", p, len(b))
		h.Write(b)
		return nil
	})
	if err != nil {
		return "", err
	}
	return "dir:" + hex.EncodeToString(h.Sum(nil))[:12], nil
}

// Read calls fn with the directory.
func (d Dir) Read(fn func(string) error) error { return fn(d.Path) }

// Git keeps its own clone, hard-reset on every sync.
type Git struct {
	URL     string
	Branch  string
	Subdir  string // desired-state directory in the repo
	Workdir string
	Token   string // HTTPS token
	Author  string // "Name <email>"
	mu      sync.Mutex
}

// Base is the tracked branch.
func (g *Git) Base() string { return g.Branch }

// Sync clones or fetches, then resets to the remote branch.
func (g *Git) Sync(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := os.Stat(filepath.Join(g.Workdir, ".git")); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(g.Workdir), 0o755); err != nil {
			return "", err
		}
		if _, err := g.git(ctx, "", "clone", "--quiet", "--branch", g.Branch, g.URL, g.Workdir); err != nil {
			return "", err
		}
	} else {
		if _, err := g.git(ctx, g.Workdir, "fetch", "--quiet", "--prune", "origin", g.Branch); err != nil {
			return "", err
		}
	}
	if err := g.resetToBase(ctx); err != nil {
		return "", err
	}
	rev, err := g.git(ctx, g.Workdir, "rev-parse", "--short=12", "HEAD")
	if err != nil {
		return "", err
	}
	return rev, nil
}

func (g *Git) resetToBase(ctx context.Context) error {
	if _, err := g.git(ctx, g.Workdir, "checkout", "--quiet", "--force", "-B", g.Branch, "origin/"+g.Branch); err != nil {
		return err
	}
	_, err := g.git(ctx, g.Workdir, "clean", "--quiet", "-fd")
	return err
}

// Read calls fn with the desired-state directory.
func (g *Git) Read(fn func(string) error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return fn(filepath.Join(g.Workdir, g.Subdir))
}

// Propose commits files on a fresh branch and force-pushes it, leaving the
// checkout back on the tracked branch whatever happens.
func (g *Git) Propose(ctx context.Context, branch, message string, files map[string][]byte) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	defer g.resetToBase(context.WithoutCancel(ctx)) //nolint:errcheck // best effort; the next Sync retries

	if _, err := g.git(ctx, g.Workdir, "checkout", "--quiet", "--force", "-B", branch, "origin/"+g.Branch); err != nil {
		return false, err
	}
	for rel, b := range files {
		p := filepath.Join(g.Workdir, g.Subdir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return false, err
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return false, err
		}
		if _, err := g.git(ctx, g.Workdir, "add", "--", filepath.Join(g.Subdir, rel)); err != nil {
			return false, err
		}
	}
	if _, err := g.git(ctx, g.Workdir, "diff", "--cached", "--quiet"); err == nil {
		return false, nil
	}
	name, email := splitAuthor(g.Author)
	if _, err := g.git(ctx, g.Workdir, "-c", "user.name="+name, "-c", "user.email="+email, "commit", "--quiet", "-m", message); err != nil {
		return false, err
	}
	if _, err := g.git(ctx, g.Workdir, "push", "--quiet", "--force", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return false, err
	}
	return true, nil
}

// DeleteBranch deletes a remote branch.
func (g *Git) DeleteBranch(ctx context.Context, branch string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, err := g.git(ctx, g.Workdir, "push", "--quiet", "origin", "--delete", branch)
	if err != nil && strings.Contains(err.Error(), "remote ref does not exist") {
		return nil
	}
	return err
}

func splitAuthor(a string) (string, string) {
	if a == "" {
		return "netflux", "netflux@localhost"
	}
	if i := strings.Index(a, "<"); i > 0 && strings.HasSuffix(a, ">") {
		return strings.TrimSpace(a[:i]), a[i+1 : len(a)-1]
	}
	return a, "netflux@localhost"
}

// git runs git, passing the token via environment config so it never hits
// argv, .git/config or errors.
func (g *Git) git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if g.Token != "" && strings.HasPrefix(g.URL, "https://") {
		auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + g.Token))
		cmd.Env = append(cmd.Env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+auth,
		)
	}
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}
