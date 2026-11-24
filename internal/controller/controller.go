// Package controller keeps each device equal to git.
//
// Drift is measured against the config read back after the last apply, not
// against git: devices add defaults and normalise values.
package controller

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/og124/netflux/internal/desired"
	"github.com/og124/netflux/internal/github"
	"github.com/og124/netflux/internal/source"
)

// Options configures a Controller.
type Options struct {
	Source source.Source
	// GitHub, if set, opens PRs for drift branches.
	GitHub *github.Client
	// StateDir persists snapshots across restarts.
	StateDir string
	// Poll is the git fetch interval.
	Poll time.Duration
	// Resync is the full re-read and PR check interval.
	Resync time.Duration
	// Reapply re-applies strict devices periodically, catching hand additions
	// made between an apply and its read-back.
	Reapply time.Duration
	// Settle is the wait between Set and read-back.
	Settle time.Duration
	// Debounce batches change notifications.
	Debounce time.Duration
	Log      *slog.Logger
}

// Controller runs a loop per device.
type Controller struct {
	opt     Options
	syncNow chan struct{}

	mu       sync.Mutex
	runners  map[string]*runner
	revision string
	loadErr  string
	synced   time.Time
}

// New returns a Controller with defaults.
func New(opt Options) *Controller {
	if opt.Poll == 0 {
		opt.Poll = 15 * time.Second
	}
	if opt.Resync == 0 {
		opt.Resync = 60 * time.Second
	}
	if opt.Reapply == 0 {
		opt.Reapply = 10 * time.Minute
	}
	if opt.Settle == 0 {
		opt.Settle = 2 * time.Second
	}
	if opt.Debounce == 0 {
		opt.Debounce = 500 * time.Millisecond
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	return &Controller{opt: opt, syncNow: make(chan struct{}, 1), runners: map[string]*runner{}}
}

// Run syncs git and runs device loops until ctx ends.
func (c *Controller) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	tick := time.NewTicker(c.opt.Poll)
	defer tick.Stop()
	for {
		c.sync(ctx, &wg)
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		case <-tick.C:
		case <-c.syncNow:
		}
	}
}

// SyncNow triggers a git sync.
func (c *Controller) SyncNow() {
	select {
	case c.syncNow <- struct{}{}:
	default:
	}
}

func (c *Controller) sync(ctx context.Context, wg *sync.WaitGroup) {
	rev, err := c.opt.Source.Sync(ctx)
	if err != nil {
		c.opt.Log.Error("sync desired state", "err", err)
		c.setLoad(c.revision, "sync: "+err.Error())
		return
	}
	c.mu.Lock()
	same := rev == c.revision && c.loadErr == ""
	c.synced = time.Now()
	c.mu.Unlock()
	if same {
		return
	}

	var devs map[string]*desired.Device
	err = c.opt.Source.Read(func(dir string) error {
		var err error
		devs, err = desired.LoadAll(dir)
		return err
	})
	if err != nil {
		// Keep enforcing the last good revision.
		c.opt.Log.Error("desired state is invalid; still enforcing the last good revision", "revision", rev, "err", err)
		c.setLoad(rev, err.Error())
		return
	}
	c.opt.Log.Info("desired state loaded", "revision", rev, "devices", len(devs))
	c.setLoad(rev, "")

	c.mu.Lock()
	defer c.mu.Unlock()
	for name, d := range devs {
		if r, ok := c.runners[name]; ok {
			r.update(d, rev)
			continue
		}
		r := newRunner(c, d, rev)
		rctx, cancel := context.WithCancel(ctx)
		r.cancel = cancel
		c.runners[name] = r
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.run(rctx)
		}()
	}
	for name, r := range c.runners {
		if _, ok := devs[name]; !ok {
			c.opt.Log.Warn("device left the inventory; no longer managing it, and leaving its config as it is", "device", name)
			r.stop()
			delete(c.runners, name)
		}
	}
}

func (c *Controller) setLoad(rev, errMsg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.revision, c.loadErr = rev, errMsg
}

// Status is served on /status.
type Status struct {
	Revision  string         `json:"revision"`
	LoadError string         `json:"load_error,omitempty"`
	Synced    time.Time      `json:"synced"`
	Devices   []DeviceStatus `json:"devices"`
}

// Status snapshots every device.
func (c *Controller) Status() Status {
	c.mu.Lock()
	s := Status{Revision: c.revision, LoadError: c.loadErr, Synced: c.synced}
	runners := make([]*runner, 0, len(c.runners))
	for _, r := range c.runners {
		runners = append(runners, r)
	}
	c.mu.Unlock()
	for _, r := range runners {
		s.Devices = append(s.Devices, r.statusCopy())
	}
	sort.Slice(s.Devices, func(i, j int) bool { return s.Devices[i].Name < s.Devices[j].Name })
	return s
}
