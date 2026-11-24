// Command netflux reconciles network devices to OpenConfig in git.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/og124/netflux/internal/controller"
	"github.com/og124/netflux/internal/desired"
	"github.com/og124/netflux/internal/device"
	"github.com/og124/netflux/internal/github"
	"github.com/og124/netflux/internal/server"
	"github.com/og124/netflux/internal/sim"
	"github.com/og124/netflux/internal/source"
	"github.com/og124/netflux/internal/tree"
)

const usage = `netflux: GitOps for network devices.

Git holds each device's desired config as OpenConfig; netflux makes the live
device match it with gNMI Set in replace mode, and treats any change made on
the device itself as drift.

Usage:
  netflux run      [-config netflux.yaml | -dir DIR]   run the controller
  netflux validate [-dir DIR]                          check desired state
  netflux render   [-dir DIR] DEVICE                   print what would be sent
  netflux diff     [-dir DIR] [-exit-code] DEVICE      what an apply would change
  netflux apply    [-dir DIR] DEVICE                   apply once and exit
  netflux import   [-dir DIR] [-o FILE] DEVICE         seed git from a live device
  netflux sim      NAME=ADDR...                        run simulated gNMI devices

DIR defaults to the current directory and holds inventory.yaml.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmds := map[string]func(context.Context, []string) error{
		"run":      cmdRun,
		"validate": cmdValidate,
		"render":   cmdRender,
		"diff":     cmdDiff,
		"apply":    cmdApply,
		"import":   cmdImport,
		"sim":      cmdSim,
	}
	cmd, ok := cmds[os.Args[1]]
	if !ok {
		if os.Args[1] != "help" && os.Args[1] != "-h" && os.Args[1] != "--help" {
			fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		}
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	err := cmd(ctx, os.Args[2:])
	var ec exitCode
	switch {
	case errors.As(err, &ec):
		os.Exit(int(ec))
	case err != nil:
		fmt.Fprintln(os.Stderr, "netflux:", err)
		os.Exit(1)
	}
}

type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit %d", int(e)) }

// parse allows flags after positional arguments.
func parse(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		fs.Parse(args) //nolint:errcheck // ExitOnError
		args = fs.Args()
		if len(args) == 0 {
			return pos
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func logger(verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// Config is netflux.yaml.
type Config struct {
	Dir string `yaml:"dir"`
	Git *struct {
		URL      string `yaml:"url"`
		Branch   string `yaml:"branch"`
		Path     string `yaml:"path"`
		TokenEnv string `yaml:"token_env"`
		Author   string `yaml:"author"`
	} `yaml:"git"`
	GitHub *struct {
		Repo     string `yaml:"repo"`
		TokenEnv string `yaml:"token_env"`
		API      string `yaml:"api"`
	} `yaml:"github"`
	StateDir string        `yaml:"state_dir"`
	Workdir  string        `yaml:"workdir"`
	Listen   string        `yaml:"listen"`
	Poll     time.Duration `yaml:"poll"`
	Resync   time.Duration `yaml:"resync"`
	Settle   time.Duration `yaml:"settle"`
}

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", "", "controller config (netflux.yaml)")
	dir := fs.String("dir", "", "use this directory as desired state instead of git")
	listen := fs.String("listen", "", "HTTP listen address (overrides config)")
	stateDir := fs.String("state", "", "state directory (overrides config; -state '' keeps no state)")
	verbose := fs.Bool("v", false, "debug logging")
	parse(fs, args)
	log := logger(*verbose)

	cfg := Config{StateDir: ".netflux/state", Workdir: ".netflux/checkout", Listen: "127.0.0.1:8080"}
	if *cfgPath != "" {
		b, err := os.ReadFile(*cfgPath)
		if err != nil {
			return err
		}
		dec := yaml.NewDecoder(strings.NewReader(string(b)))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return fmt.Errorf("%s: %w", *cfgPath, err)
		}
	}
	if *dir != "" {
		cfg.Dir, cfg.Git = *dir, nil
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "state" {
			cfg.StateDir = *stateDir
		}
	})

	var src source.Source
	switch {
	case cfg.Git != nil:
		g := &source.Git{URL: cfg.Git.URL, Branch: cfg.Git.Branch, Subdir: cfg.Git.Path, Workdir: cfg.Workdir, Author: cfg.Git.Author}
		if g.Branch == "" {
			g.Branch = "main"
		}
		if cfg.Git.TokenEnv != "" {
			g.Token = os.Getenv(cfg.Git.TokenEnv)
		}
		src = g
	case cfg.Dir != "":
		src = source.Dir{Path: cfg.Dir}
	default:
		src = source.Dir{Path: "."}
	}
	var gh *github.Client
	if cfg.GitHub != nil {
		gh = &github.Client{Repo: cfg.GitHub.Repo, API: cfg.GitHub.API}
		if cfg.GitHub.TokenEnv != "" {
			gh.Token = os.Getenv(cfg.GitHub.TokenEnv)
		}
		if gh.Token == "" {
			return fmt.Errorf("github.repo is set but $%s is empty", cfg.GitHub.TokenEnv)
		}
	}

	c := controller.New(controller.Options{
		Source: src, GitHub: gh, StateDir: cfg.StateDir,
		Poll: cfg.Poll, Resync: cfg.Resync, Settle: cfg.Settle, Log: log,
	})
	if cfg.Listen != "" {
		lis, err := net.Listen("tcp", cfg.Listen)
		if err != nil {
			return err
		}
		srv := &http.Server{Handler: server.Handler(c), ReadHeaderTimeout: 10 * time.Second}
		go srv.Serve(lis) //nolint:errcheck
		defer srv.Close()
		log.Info("status page", "url", "http://"+lis.Addr().String())
	}
	return c.Run(ctx)
}

func cmdValidate(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	dir := fs.String("dir", ".", "desired-state directory")
	parse(fs, args)
	devs, err := desired.LoadAll(*dir)
	if err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(devs)) {
		d := devs[name]
		fmt.Printf("%-12s ok  %4d leaves  %d roots  %d vendor ops  mode %-7s  %s\n",
			name, len(d.Config), len(d.Roots), len(d.Vendor), d.Spec.Mode, strings.Join(d.Files, " "))
	}
	return nil
}

func loadOne(dir string, rest []string) (*desired.Device, error) {
	if len(rest) != 1 {
		return nil, errors.New("want exactly one DEVICE")
	}
	inv, err := desired.LoadInventory(dir)
	if err != nil {
		return nil, err
	}
	return inv.Render(dir, rest[0])
}

func cmdRender(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("render", flag.ExitOnError)
	dir := fs.String("dir", ".", "desired-state directory")
	d, err := loadOne(*dir, parse(fs, args))
	if err != nil {
		return err
	}
	// Preserved subtrees come from the device at apply time.
	plan, err := controller.BuildPlan(d, tree.Tree{})
	if err != nil {
		return err
	}
	for _, op := range append(plan.Ops, plan.Vendor...) {
		fmt.Println(op)
		if op.Value != nil {
			if s, ok := op.Value.(string); ok {
				fmt.Println(indent(s))
			} else {
				b, _ := json.MarshalIndent(op.Value, "", "  ")
				fmt.Println(indent(string(b)))
			}
		}
	}
	if len(d.Preserve) > 0 {
		fmt.Fprintf(os.Stderr, "\npreserved from the live device at apply time: %v\n", d.Preserve)
	}
	return nil
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n  ")
}

func dial(d *desired.Device) (*device.Client, error) { return device.Dial(device.TargetFor(d)) }

func cmdDiff(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	dir := fs.String("dir", ".", "desired-state directory")
	exit := fs.Bool("exit-code", false, "exit 2 if the device differs from git")
	d, err := loadOne(*dir, parse(fs, args))
	if err != nil {
		return err
	}
	c, err := dial(d)
	if err != nil {
		return err
	}
	defer c.Close()
	live, err := c.GetConfig(ctx, d.Roots)
	if err != nil {
		return err
	}
	plan, err := controller.BuildPlan(d, live)
	if err != nil {
		return err
	}
	changes := plan.Changes(live, d.Ignore)
	removed := 0
	for _, ch := range changes {
		fmt.Println(ch)
		if ch.Kind == tree.Removed {
			removed++
		}
	}
	fmt.Fprintf(os.Stderr, "\n%s: %d leaves differ from git", d.Name, len(changes))
	if removed > 0 {
		fmt.Fprintf(os.Stderr, " (%d removals; some may be defaults the device puts back by itself)", removed)
	}
	fmt.Fprintln(os.Stderr)
	if *exit && len(changes) > 0 {
		return exitCode(2)
	}
	return nil
}

func cmdApply(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	dir := fs.String("dir", ".", "desired-state directory")
	d, err := loadOne(*dir, parse(fs, args))
	if err != nil {
		return err
	}
	c, err := dial(d)
	if err != nil {
		return err
	}
	defer c.Close()
	live, err := c.GetConfig(ctx, d.Roots)
	if err != nil {
		return err
	}
	plan, err := controller.BuildPlan(d, live)
	if err != nil {
		return err
	}
	changes := plan.Changes(live, d.Ignore)
	if err := c.Set(ctx, plan.Ops); err != nil {
		return fmt.Errorf("set: %w", err)
	}
	if err := c.Set(ctx, plan.Vendor); err != nil {
		return fmt.Errorf("set vendor ops: %w", err)
	}
	time.Sleep(2 * time.Second)
	after, err := c.GetConfig(ctx, d.Roots)
	if err != nil {
		return err
	}
	fmt.Printf("%s: applied, %d leaves changed\n", d.Name, len(changes))
	if un := controller.Unconverged(plan.Effective, after, d.Ignore); len(un) > 0 {
		fmt.Printf("%d leaves git sets didn't take:\n", len(un))
		for _, ch := range un {
			fmt.Println(" ", ch)
		}
		return exitCode(3)
	}
	return nil
}

func cmdImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	dir := fs.String("dir", ".", "desired-state directory")
	out := fs.String("o", "", "output file (default devices/DEVICE/imported.yaml; - for stdout)")
	force := fs.Bool("force", false, "overwrite an existing file")
	rest := parse(fs, args)
	if len(rest) != 1 {
		return errors.New("want exactly one DEVICE")
	}
	inv, err := desired.LoadInventory(*dir)
	if err != nil {
		return err
	}
	// Skip validating the config import will replace.
	spec, ok := inv.Devices[rest[0]]
	if !ok {
		return fmt.Errorf("%s: not in %s", rest[0], desired.InventoryFile)
	}
	d, err := renderSpecOnly(*dir, inv, rest[0], spec)
	if err != nil {
		return err
	}
	c, err := dial(d)
	if err != nil {
		return err
	}
	defer c.Close()
	live, err := c.GetConfig(ctx, d.Roots)
	if err != nil {
		return err
	}
	cfg := live.Without(d.Preserve).Without(d.Ignore)
	header := fmt.Sprintf("# Imported from %s by `netflux import` on %s. Applying it is a no-op.\n",
		d.Name, time.Now().Format("2006-01-02"))
	b, err := desired.EncodeYAML(header, cfg)
	if err != nil {
		return err
	}
	if *out == "-" {
		_, err := os.Stdout.Write(b)
		return err
	}
	if *out == "" {
		*out = filepath.Join(*dir, "devices", d.Name, "imported.yaml")
	}
	if _, err := os.Stat(*out); err == nil && !*force {
		return fmt.Errorf("%s exists; -force to overwrite", *out)
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		return err
	}
	fmt.Printf("%s: wrote %d leaves to %s\n", d.Name, len(cfg), *out)
	return nil
}

// renderSpecOnly renders a device with no config.
func renderSpecOnly(dir string, inv *desired.Inventory, name string, spec desired.Spec) (*desired.Device, error) {
	tmp, err := os.MkdirTemp("", "netflux-import-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	for _, g := range append(append([]string(nil), inv.Defaults.Groups...), spec.Groups...) {
		os.MkdirAll(filepath.Join(tmp, "groups", g), 0o755) //nolint:errcheck
	}
	return inv.Render(tmp, name)
}

func cmdSim(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sim", flag.ExitOnError)
	user := fs.String("user", "admin", "gNMI username")
	pass := fs.String("pass", "admin", "gNMI password")
	targets := parse(fs, args)
	if len(targets) == 0 {
		return errors.New("usage: netflux sim leaf1=127.0.0.1:57401 leaf2=127.0.0.1:57402")
	}
	log := logger(false)
	errc := make(chan error, len(targets))
	for _, t := range targets {
		name, addr, ok := strings.Cut(t, "=")
		if !ok {
			return fmt.Errorf("%q: want NAME=ADDR", t)
		}
		lis, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		log.Info("simulated device", "name", name, "gnmi", lis.Addr().String())
		go func() { errc <- sim.New(name, *user, *pass, log).Serve(lis) }()
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}
