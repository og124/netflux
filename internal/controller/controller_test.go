package controller_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/og124/netflux/internal/controller"
	"github.com/og124/netflux/internal/device"
	"github.com/og124/netflux/internal/github"
	"github.com/og124/netflux/internal/sim"
	"github.com/og124/netflux/internal/source"
	"github.com/og124/netflux/internal/tree"
)

const descPath = "/interfaces/interface[name=Ethernet1]/config/description"

// startSims runs simulated leaves and points the inventory at them.
func startSims(t *testing.T) map[string]*sim.Target {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sims := map[string]*sim.Target{}
	for _, name := range []string{"leaf1", "leaf2"} {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		s := sim.New(name, "admin", "admin", log)
		go s.Serve(lis) //nolint:errcheck
		t.Cleanup(func() { lis.Close() })
		sims[name] = s
		t.Setenv(strings.ToUpper(name)+"_ADDR", lis.Addr().String())
	}
	t.Setenv("NETFLUX_PASSWORD", "admin")
	return sims
}

func copyExample(t *testing.T, dst string) {
	t.Helper()
	src := filepath.Join("..", "..", "examples", "lab")
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func fastOptions(src source.Source) controller.Options {
	return controller.Options{
		Source:   src,
		Poll:     100 * time.Millisecond,
		Resync:   300 * time.Millisecond,
		Settle:   50 * time.Millisecond,
		Debounce: 50 * time.Millisecond,
		Log:      slog.New(slog.NewTextHandler(testLog(), nil)),
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func applied(c *controller.Controller, name string) bool {
	for _, d := range c.Status().Devices {
		if d.Name == name {
			return d.Applies > 0
		}
	}
	return false
}

func value(s *sim.Target, path string) any {
	l, ok := s.Config()[tree.MustParse(path).Canon()]
	if !ok {
		return nil
	}
	return l.Value
}

// handEdit changes a description, as gnmic would.
func handEdit(t *testing.T, addrEnv, desc string) {
	t.Helper()
	c, err := device.Dial(device.Target{Address: os.Getenv(addrEnv), Username: "admin", Password: "admin", Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	err = c.Set(context.Background(), []device.Op{{Kind: "update", Path: tree.MustParse(descPath), Value: desc}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStrictHealsHandChange(t *testing.T) {
	sims := startSims(t)
	dir := t.TempDir()
	copyExample(t, dir)

	c := controller.New(fastOptions(source.Dir{Path: dir}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx) //nolint:errcheck

	leaf1 := sims["leaf1"]
	eventually(t, "initial apply", func() bool { return applied(c, "leaf1") })

	// Preserved through the /interfaces replace.
	if value(leaf1, "/interfaces/interface[name=Management0]/subinterfaces/subinterface[index=0]/ipv4/addresses/address[ip=172.20.20.2]/config/prefix-length") == nil {
		t.Fatal("management address was wiped")
	}
	if got := value(leaf1, "/network-instances/network-instance[name=default]/protocols/protocol[identifier=BGP][name=BGP]/bgp/global/config/as"); got == nil {
		t.Fatal("BGP not configured")
	}
	if value(leaf1, descPath) != "leaf2:Ethernet1" {
		t.Fatal("description not applied")
	}
	if cli := leaf1.CLI(); len(cli) == 0 || !strings.Contains(cli[0], "virtual-router") {
		t.Fatalf("vendor op not sent: %q", cli)
	}

	handEdit(t, "LEAF1_ADDR", "hacked")
	eventually(t, "heal", func() bool {
		for _, d := range c.Status().Devices {
			if d.Name == "leaf1" && d.Heals > 0 && d.Applies >= 2 {
				return value(leaf1, descPath) == "leaf2:Ethernet1"
			}
		}
		return false
	})

	// Additions under a root are drift too.
	c2, _ := device.Dial(device.Target{Address: os.Getenv("LEAF1_ADDR"), Username: "admin", Password: "admin", Insecure: true})
	defer c2.Close()
	lo9 := "/interfaces/interface[name=Loopback9]"
	err := c2.Set(ctx, []device.Op{{Kind: "update", Path: tree.MustParse(lo9), Value: map[string]any{"name": "Loopback9", "config": map[string]any{"name": "Loopback9"}}}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "Loopback9 removed", func() bool { return value(leaf1, lo9+"/name") == nil })
}

// fakeGitHub fakes the pulls API.
type fakeGitHub struct {
	mu     sync.Mutex
	prs    map[int]*github.PR
	bodies map[int]string
	heads  map[int]string
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var in map[string]any
	json.NewDecoder(r.Body).Decode(&in) //nolint:errcheck
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
		var open []*github.PR
		for n, pr := range f.prs {
			if pr.State == "open" && strings.HasSuffix(r.URL.Query().Get("head"), ":"+f.heads[n]) {
				open = append(open, pr)
			}
		}
		json.NewEncoder(w).Encode(open) //nolint:errcheck
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
		n := len(f.prs) + 1
		f.prs[n] = &github.PR{Number: n, State: "open", URL: "https://github.example/pr/" + string(rune('0'+n))}
		f.bodies[n], f.heads[n] = in["body"].(string), in["head"].(string)
		json.NewEncoder(w).Encode(f.prs[n]) //nolint:errcheck
	case strings.Contains(r.URL.Path, "/comments"):
		w.WriteHeader(http.StatusCreated)
	default: // GET or PATCH /pulls/1
		pr := f.prs[1]
		if r.Method == http.MethodPatch {
			if s, ok := in["state"].(string); ok {
				pr.State = s
			}
			if b, ok := in["body"].(string); ok {
				f.bodies[1] = b
			}
		}
		json.NewEncoder(w).Encode(pr) //nolint:errcheck
	}
}

func (f *fakeGitHub) pr(n int) (github.PR, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr, ok := f.prs[n]
	if !ok {
		return github.PR{}, "", false
	}
	return *pr, f.bodies[n], true
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestPRModeProposesAndRevertsOnClose(t *testing.T) {
	sims := startSims(t)
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	git(t, root, "init", "--quiet", "--bare", "-b", "main", remote)
	git(t, root, "init", "--quiet", "-b", "main", seed)
	copyExample(t, filepath.Join(seed, "lab"))
	git(t, seed, "add", ".")
	git(t, seed, "commit", "--quiet", "-m", "seed")
	git(t, seed, "push", "--quiet", remote, "main")

	gh := &fakeGitHub{prs: map[int]*github.PR{}, bodies: map[int]string{}, heads: map[int]string{}}
	api := httptest.NewServer(gh)
	defer api.Close()

	opt := fastOptions(&source.Git{URL: remote, Branch: "main", Subdir: "lab", Workdir: filepath.Join(root, "checkout")})
	opt.GitHub = &github.Client{Repo: "og124/lab", API: api.URL}
	c := controller.New(opt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx) //nolint:errcheck

	leaf2 := sims["leaf2"]
	eventually(t, "initial apply", func() bool { return applied(c, "leaf2") })
	if v := value(leaf2, descPath); v != "leaf1:Ethernet1" {
		t.Fatalf("description after apply: %v", v)
	}

	handEdit(t, "LEAF2_ADDR", "patched by hand")
	eventually(t, "drift PR", func() bool { _, _, ok := gh.pr(1); return ok })
	_, body, _ := gh.pr(1)
	if !strings.Contains(body, descPath) || !strings.Contains(body, "patched by hand") {
		t.Errorf("PR body doesn't show the change:\n%s", body)
	}
	// pr mode leaves the device alone.
	time.Sleep(500 * time.Millisecond)
	if v := value(leaf2, descPath); v != "patched by hand" {
		t.Fatalf("pr mode touched the device: %v", v)
	}
	// The branch carries drift.yaml.
	drift := git(t, root, "--git-dir", remote, "show", "netflux/drift/leaf2:lab/devices/leaf2/drift.yaml")
	if !strings.Contains(drift, "description: patched by hand") {
		t.Errorf("drift.yaml:\n%s", drift)
	}

	// Closing unmerged reverts.
	gh.mu.Lock()
	gh.prs[1].State = "closed"
	gh.mu.Unlock()
	eventually(t, "revert on close", func() bool { return value(leaf2, descPath) == "leaf1:Ethernet1" })
}

func TestPRMergeAdoptsChange(t *testing.T) {
	sims := startSims(t)
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	git(t, root, "init", "--quiet", "--bare", "-b", "main", remote)
	git(t, root, "init", "--quiet", "-b", "main", seed)
	copyExample(t, seed)
	git(t, seed, "add", ".")
	git(t, seed, "commit", "--quiet", "-m", "seed")
	git(t, seed, "push", "--quiet", remote, "main")

	opt := fastOptions(&source.Git{URL: remote, Branch: "main", Workdir: filepath.Join(root, "checkout")})
	c := controller.New(opt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx) //nolint:errcheck

	leaf2 := sims["leaf2"]
	eventually(t, "initial apply", func() bool { return applied(c, "leaf2") })
	if v := value(leaf2, descPath); v != "leaf1:Ethernet1" {
		t.Fatalf("description after apply: %v", v)
	}
	handEdit(t, "LEAF2_ADDR", "keep me")

	// Without GitHub only the branch is pushed; merge it by hand.
	eventually(t, "drift branch", func() bool {
		return exec.Command("git", "--git-dir", remote, "rev-parse", "--verify", "--quiet", "netflux/drift/leaf2").Run() == nil
	})
	git(t, seed, "pull", "--quiet", remote, "netflux/drift/leaf2")
	git(t, seed, "push", "--quiet", remote, "main")

	// Merged drift is desired state now.
	eventually(t, "adopted", func() bool {
		for _, d := range c.Status().Devices {
			if d.Name == "leaf2" && len(d.Drift) == 0 && d.Revision != "" && d.Applies >= 2 {
				return value(leaf2, descPath) == "keep me"
			}
		}
		return false
	})
}

func testLog() io.Writer {
	if os.Getenv("NETFLUX_TEST_LOG") != "" {
		return os.Stderr
	}
	return io.Discard
}
