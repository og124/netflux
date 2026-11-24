package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/og124/netflux/internal/desired"
	"github.com/og124/netflux/internal/device"
	"github.com/og124/netflux/internal/source"
	"github.com/og124/netflux/internal/tree"
)

// DeviceStatus is served on /status.
type DeviceStatus struct {
	Name        string    `json:"name"`
	Address     string    `json:"address"`
	Mode        string    `json:"mode"`
	Streaming   bool      `json:"streaming"`
	Revision    string    `json:"revision"`
	DesiredHash string    `json:"desired_hash"`
	LastApply   time.Time `json:"last_apply"`
	LastCheck   time.Time `json:"last_check"`
	// Drift is what changed since the last apply.
	Drift   []string `json:"drift"`
	DriftPR string   `json:"drift_pr,omitempty"`
	// Pending is what an apply would change (observe).
	Pending []string `json:"pending,omitempty"`
	// Unconverged is git config the device didn't take.
	Unconverged []string `json:"unconverged,omitempty"`
	LastError   string   `json:"last_error,omitempty"`
	Applies     int      `json:"applies"`
	Failures    int      `json:"failures"`
	Heals       int      `json:"heals"`
	Events      []Event  `json:"events"`
}

// Event is a recent history entry.
type Event struct {
	Time time.Time `json:"time"`
	Msg  string    `json:"msg"`
}

// persisted survives restarts.
type persisted struct {
	DesiredHash string     `json:"desired_hash"`
	Revision    string     `json:"revision"`
	AppliedAt   time.Time  `json:"applied_at"`
	Snapshot    []snapLeaf `json:"snapshot"`
	DriftHash   string     `json:"drift_hash,omitempty"`
	DriftPR     int        `json:"drift_pr,omitempty"`
	DriftPRURL  string     `json:"drift_pr_url,omitempty"`
}

type snapLeaf struct {
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

type pendingUpdate struct {
	dev *desired.Device
	rev string
}

type runner struct {
	name string
	c    *Controller
	log  *slog.Logger

	wake   chan struct{}
	pokes  chan struct{}
	cancel context.CancelFunc

	pmu     sync.Mutex
	pending *pendingUpdate

	// Owned by the run goroutine.
	dev       *desired.Device
	rev       string
	st        persisted
	snapshot  tree.Tree
	client    *device.Client
	subCancel context.CancelFunc
	subKey    string

	smu    sync.Mutex
	status DeviceStatus
}

func newRunner(c *Controller, d *desired.Device, rev string) *runner {
	return &runner{
		name:    d.Name,
		c:       c,
		log:     c.opt.Log.With("device", d.Name),
		wake:    make(chan struct{}, 1),
		pokes:   make(chan struct{}, 1),
		pending: &pendingUpdate{d, rev},
		status:  DeviceStatus{Name: d.Name},
	}
}

func (r *runner) update(d *desired.Device, rev string) {
	r.pmu.Lock()
	r.pending = &pendingUpdate{d, rev}
	r.pmu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *runner) stop() { r.cancel() }

func (r *runner) takePending() *pendingUpdate {
	r.pmu.Lock()
	defer r.pmu.Unlock()
	p := r.pending
	r.pending = nil
	return p
}

func (r *runner) run(ctx context.Context) {
	defer func() {
		if r.subCancel != nil {
			r.subCancel()
		}
		if r.client != nil {
			r.client.Close()
		}
	}()

	r.loadState()
	resync := time.NewTicker(r.c.opt.Resync)
	defer resync.Stop()
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()

	r.takeUpdate(ctx)
	r.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
			r.takeUpdate(ctx)
			r.reconcile(ctx)
		case <-r.pokes:
			debounce.Reset(r.c.opt.Debounce)
		case <-debounce.C:
			r.reconcile(ctx)
		case <-resync.C:
			if !r.pollPR(ctx) {
				r.reconcile(ctx)
			}
		}
	}
}

// takeUpdate adopts the latest desired state.
func (r *runner) takeUpdate(ctx context.Context) {
	p := r.takePending()
	if p == nil {
		return
	}
	reconnect := r.dev == nil || device.TargetFor(r.dev) != device.TargetFor(p.dev)
	r.dev, r.rev = p.dev, p.rev
	r.setStatus(func(s *DeviceStatus) {
		s.Address, s.Mode = r.dev.Spec.Address, string(r.dev.Spec.Mode)
	})
	if reconnect && r.client != nil {
		r.client.Close()
		r.client = nil
	}
	r.connect(ctx)
}

// connect dials and (re)subscribes as needed.
func (r *runner) connect(ctx context.Context) {
	if r.client == nil {
		client, err := device.Dial(device.TargetFor(r.dev))
		if err != nil {
			r.fail("connect", err)
			return
		}
		r.client = client
		r.subKey = ""
	}
	if key := fmt.Sprint(r.dev.Roots); key != r.subKey {
		if r.subCancel != nil {
			r.subCancel()
		}
		var sctx context.Context
		sctx, r.subCancel = context.WithCancel(ctx)
		r.subKey = key
		go r.subscribe(sctx, r.client, r.dev.Roots)
	}
}

// subscribe keeps an ON_CHANGE stream open; notifications poke the loop.
func (r *runner) subscribe(ctx context.Context, client *device.Client, roots []tree.Path) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := client.Subscribe(ctx, roots,
			func() {
				backoff = time.Second
				r.setStatus(func(s *DeviceStatus) { s.Streaming = true })
				r.log.Debug("subscribed")
			},
			func([]tree.Path) {
				select {
				case r.pokes <- struct{}{}:
				default:
				}
			})
		r.setStatus(func(s *DeviceStatus) { s.Streaming = false })
		if ctx.Err() != nil {
			return
		}
		r.log.Warn("subscription ended; retrying", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// reconcile applies git if it changed, else checks drift.
func (r *runner) reconcile(ctx context.Context) {
	if r.dev == nil {
		return
	}
	if r.connect(ctx); r.client == nil {
		return
	}
	stale := r.dev.Spec.Mode == desired.ModeStrict && time.Since(r.st.AppliedAt) > r.c.opt.Reapply
	if r.st.DesiredHash != r.dev.Hash || r.snapshot == nil || stale {
		// Settle any drift PR first.
		if r.pollPR(ctx) {
			return
		}
		reason := "git changed"
		switch {
		case r.snapshot == nil:
			reason = "first contact"
		case r.st.DesiredHash == r.dev.Hash:
			reason = "periodic re-apply"
		}
		r.apply(ctx, reason)
		return
	}
	r.checkDrift(ctx)
}

// apply replaces every root and snapshots the result as the drift baseline.
func (r *runner) apply(ctx context.Context, reason string) {
	d := r.dev
	live, err := r.client.GetConfig(ctx, d.Roots)
	if err != nil {
		r.fail("read before apply", err)
		return
	}
	plan, err := BuildPlan(d, live)
	if err != nil {
		r.fail("plan", err)
		return
	}
	changes := plan.Changes(live, d.Ignore)

	if d.Spec.Mode == desired.ModeObserve {
		r.snapshot = live
		r.commitState(d)
		r.setStatus(func(s *DeviceStatus) {
			s.Pending = formatChanges(changes)
			s.Drift, s.LastError = nil, ""
			s.LastCheck = time.Now()
		})
		r.event(fmt.Sprintf("observe: %d %s differ from git (%s); not applying", len(changes), plural(len(changes), "leaf", "leaves"), reason))
		return
	}

	// A mismatch is normalisation or a concurrent edit; a second apply tells
	// them apart, so an edit can't become the baseline.
	var after tree.Tree
	var unconverged []tree.Change
	for attempt := 1; ; attempt++ {
		if err := r.client.Set(ctx, plan.Ops); err != nil {
			r.fail("apply", err)
			return
		}
		if len(plan.Vendor) > 0 {
			if err := r.client.Set(ctx, plan.Vendor); err != nil {
				r.fail("apply vendor ops", err)
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.c.opt.Settle):
		}
		if after, err = r.client.GetConfig(ctx, d.Roots); err != nil {
			r.fail("read after apply", err)
			return
		}
		unconverged = Unconverged(plan.Effective, after, d.Ignore)
		if len(unconverged) == 0 || attempt == 2 {
			break
		}
		r.log.Info("device doesn't match git after apply; applying once more", "leaves", len(unconverged))
	}
	// Ignore our own Set's notifications.
	select {
	case <-r.pokes:
	default:
	}

	hadPR := r.st.DriftPR != 0 && r.st.DriftHash != ""
	r.snapshot = after
	r.commitState(d)
	r.setStatus(func(s *DeviceStatus) {
		s.Applies++
		s.LastApply = time.Now()
		s.LastCheck = s.LastApply
		s.Drift, s.Pending, s.LastError = nil, nil, ""
		s.Unconverged = formatChanges(unconverged)
	})
	changed := tree.Diff(live.Without(d.Ignore), after.Without(d.Ignore))
	msg := fmt.Sprintf("applied %s (%s): %d %s changed on the device", r.rev, reason, len(changed), plural(len(changed), "leaf", "leaves"))
	if len(unconverged) > 0 {
		msg += fmt.Sprintf("; %d %s didn't take, see unconverged", len(unconverged), plural(len(unconverged), "leaf", "leaves"))
		for _, c := range unconverged {
			r.log.Warn("unconverged", "change", c.String())
		}
	}
	r.event(msg)
	if hadPR && reason == "git changed" {
		r.commentPR(ctx, fmt.Sprintf("netflux applied %s to %s, so the device is back to what git says. "+
			"This PR still holds the change that was made by hand: merge it to put that back, or close it to discard it.", r.rev, r.name))
	}
}

// checkDrift compares live config with the snapshot.
func (r *runner) checkDrift(ctx context.Context) {
	d := r.dev
	changes, err := r.drift(ctx)
	if err != nil {
		r.fail("read", err)
		return
	}
	// Re-check new drift after a pause, in case the device was mid-commit.
	if len(changes) > 0 && r.st.DriftHash != hashChanges(changes) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.c.opt.Settle):
		}
		if changes, err = r.drift(ctx); err != nil {
			r.fail("read", err)
			return
		}
	}
	r.setStatus(func(s *DeviceStatus) {
		s.LastCheck = time.Now()
		s.Drift = formatChanges(changes)
		s.LastError = ""
	})

	if len(changes) == 0 {
		if r.st.DriftHash != "" {
			r.event("drift went away: the device matches git again")
			r.closePR(ctx, "The device matches git again (the change was undone on the device), so there is nothing to adopt.")
			r.st.DriftHash = ""
			r.saveState()
		}
		return
	}
	h := hashChanges(changes)
	for _, c := range changes {
		r.log.Info("drift", "change", c.String())
	}

	switch d.Spec.Mode {
	case desired.ModeStrict:
		r.event(fmt.Sprintf("drift: %d %s changed on the device; strict mode, reverting", len(changes), plural(len(changes), "leaf", "leaves")))
		r.setStatus(func(s *DeviceStatus) { s.Heals++ })
		r.apply(ctx, "heal")
	case desired.ModeObserve:
		if h != r.st.DriftHash {
			r.event(fmt.Sprintf("drift: %d %s changed on the device; observe mode, leaving it", len(changes), plural(len(changes), "leaf", "leaves")))
			r.st.DriftHash = h
			r.saveState()
		}
	case desired.ModePR:
		if h == r.st.DriftHash {
			return // already proposed exactly this
		}
		r.event(fmt.Sprintf("drift: %d %s changed on the device; proposing them back to git", len(changes), plural(len(changes), "leaf", "leaves")))
		if err := r.propose(ctx, changes); err != nil {
			r.fail("propose drift", err)
			return
		}
		r.st.DriftHash = h
		r.saveState()
	}
}

func (r *runner) drift(ctx context.Context) ([]tree.Change, error) {
	live, err := r.client.GetConfig(ctx, r.dev.Roots)
	if err != nil {
		return nil, err
	}
	return tree.Diff(r.snapshot.Without(r.dev.Ignore), live.Without(r.dev.Ignore)), nil
}

func (r *runner) branch() string { return "netflux/drift/" + r.name }

// propose pushes drift.yaml on a branch and opens or updates a PR.
func (r *runner) propose(ctx context.Context, changes []tree.Change) error {
	p, ok := r.c.opt.Source.(source.Proposer)
	if !ok {
		r.event("desired state isn't a git remote, so there is nowhere to propose this; run with a git source for PRs")
		return nil
	}
	rel := filepath.Join("devices", r.name, desired.DriftFile)
	var existing []byte
	if err := p.Read(func(dir string) error {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		existing = b
		return err
	}); err != nil {
		return err
	}
	content, err := desired.MergeDrift(r.name, existing, changes)
	if err != nil {
		return err
	}
	title := fmt.Sprintf("%s: adopt %d %s made on the device", r.name, len(changes), plural(len(changes), "change", "changes"))
	if _, err := p.Propose(ctx, r.branch(), title, map[string][]byte{rel: content}); err != nil {
		return err
	}
	gh := r.c.opt.GitHub
	if gh == nil {
		r.event(fmt.Sprintf("pushed branch %s; no GitHub configured, so open the pull request yourself", r.branch()))
		return nil
	}
	pr, err := gh.EnsurePR(ctx, r.branch(), p.Base(), title, prBody(r.name, r.rev, rel, changes))
	if err != nil {
		return err
	}
	if pr.Number != r.st.DriftPR {
		r.event(fmt.Sprintf("opened %s", pr.URL))
	} else {
		r.event(fmt.Sprintf("updated %s", pr.URL))
	}
	r.st.DriftPR, r.st.DriftPRURL = pr.Number, pr.URL
	r.setStatus(func(s *DeviceStatus) { s.DriftPR = pr.URL })
	return nil
}

// pollPR handles a closed PR: merged waits for git, unmerged reverts. It
// reports whether it applied.
func (r *runner) pollPR(ctx context.Context) bool {
	gh := r.c.opt.GitHub
	if gh == nil || r.st.DriftPR == 0 || r.client == nil {
		return false
	}
	pr, err := gh.Get(ctx, r.st.DriftPR)
	if err != nil {
		r.log.Warn("check drift PR", "err", err)
		return false
	}
	if pr.State != "closed" {
		return false
	}
	r.st.DriftPR, r.st.DriftPRURL = 0, ""
	r.setStatus(func(s *DeviceStatus) { s.DriftPR = "" })
	r.deleteBranch(ctx)
	if pr.Merged {
		r.event(fmt.Sprintf("%s merged; the change will arrive with the next git sync", pr.URL))
		r.saveState()
		r.c.SyncNow()
		return false
	}
	r.event(fmt.Sprintf("%s closed without merging: change rejected, reverting the device", pr.URL))
	r.st.DriftHash = ""
	r.saveState()
	r.apply(ctx, "drift rejected")
	return true
}

func (r *runner) commentPR(ctx context.Context, body string) {
	if gh := r.c.opt.GitHub; gh != nil && r.st.DriftPR != 0 {
		if err := gh.Comment(ctx, r.st.DriftPR, body); err != nil {
			r.log.Warn("comment on drift PR", "err", err)
		}
	}
}

func (r *runner) closePR(ctx context.Context, comment string) {
	gh := r.c.opt.GitHub
	if gh == nil || r.st.DriftPR == 0 {
		return
	}
	if err := gh.Close(ctx, r.st.DriftPR, comment); err != nil {
		r.log.Warn("close drift PR", "err", err)
		return
	}
	r.event(fmt.Sprintf("closed %s", r.st.DriftPRURL))
	r.st.DriftPR, r.st.DriftPRURL = 0, ""
	r.setStatus(func(s *DeviceStatus) { s.DriftPR = "" })
	r.deleteBranch(ctx)
}

func (r *runner) deleteBranch(ctx context.Context) {
	if p, ok := r.c.opt.Source.(source.Proposer); ok {
		if err := p.DeleteBranch(ctx, r.branch()); err != nil {
			r.log.Debug("delete drift branch", "err", err)
		}
	}
}

func prBody(device, rev, file string, changes []tree.Change) string {
	var b strings.Builder
	fmt.Fprintf(&b, "netflux saw these changes on **%s** that git doesn't have (git at `%s`):\n\n", device, rev)
	b.WriteString("| | path | git | device |\n|---|---|---|---|\n")
	const maxRows = 200
	for i, c := range changes {
		if i == maxRows {
			fmt.Fprintf(&b, "| | … and %d more | | |\n", len(changes)-maxRows)
			break
		}
		old, cur := "", ""
		mark := map[tree.ChangeKind]string{tree.Added: "+", tree.Removed: "−", tree.Changed: "~"}[c.Kind]
		if c.Kind != tree.Added {
			old = "`" + tree.FormatValue(c.Old) + "`"
		}
		if c.Kind != tree.Removed {
			cur = "`" + tree.FormatValue(c.New) + "`"
		}
		fmt.Fprintf(&b, "| %s | `%s` | %s | %s |\n", mark, c.Path, old, cur)
	}
	fmt.Fprintf(&b, "\n**Merge** to adopt them: they're written to `%s`, which layers over the device's other files.\n", file)
	b.WriteString("**Close** to reject them: netflux will put the device back the way git says.\n\n")
	fmt.Fprintf(&b, "%s is in `pr` mode. In `strict` mode netflux would have reverted this on sight.\n", device)
	return b.String()
}

func formatChanges(cs []tree.Change) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.String()
	}
	return out
}

func hashChanges(cs []tree.Change) string {
	if len(cs) == 0 {
		return ""
	}
	h := sha256.New()
	for _, c := range cs {
		fmt.Fprintln(h, c.String())
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func (r *runner) event(msg string) {
	r.log.Info(msg)
	r.setStatus(func(s *DeviceStatus) {
		s.Events = append(s.Events, Event{Time: time.Now(), Msg: msg})
		if len(s.Events) > 30 {
			s.Events = s.Events[len(s.Events)-30:]
		}
	})
}

func (r *runner) fail(what string, err error) {
	r.log.Error(what, "err", err)
	r.setStatus(func(s *DeviceStatus) {
		s.Failures++
		s.LastError = fmt.Sprintf("%s: %v", what, err)
	})
}

func (r *runner) setStatus(fn func(*DeviceStatus)) {
	r.smu.Lock()
	defer r.smu.Unlock()
	fn(&r.status)
}

func (r *runner) statusCopy() DeviceStatus {
	r.smu.Lock()
	defer r.smu.Unlock()
	s := r.status
	s.Drift = append([]string(nil), s.Drift...)
	s.Pending = append([]string(nil), s.Pending...)
	s.Unconverged = append([]string(nil), s.Unconverged...)
	s.Events = append([]Event(nil), s.Events...)
	return s
}

// commitState records an apply.
func (r *runner) commitState(d *desired.Device) {
	r.st.DesiredHash, r.st.Revision, r.st.AppliedAt = d.Hash, r.rev, time.Now()
	r.st.DriftHash = ""
	r.setStatus(func(s *DeviceStatus) { s.Revision, s.DesiredHash = r.rev, d.Hash })
	r.saveState()
}

func (r *runner) statePath() string {
	if r.c.opt.StateDir == "" {
		return ""
	}
	return filepath.Join(r.c.opt.StateDir, r.name+".json")
}

func (r *runner) saveState() {
	p := r.statePath()
	if p == "" {
		return
	}
	r.st.Snapshot = r.st.Snapshot[:0]
	for _, k := range r.snapshot.Keys() {
		l := r.snapshot[k]
		v, _ := json.Marshal(l.Value)
		r.st.Snapshot = append(r.st.Snapshot, snapLeaf{Path: l.Path.String(), Value: v})
	}
	b, err := json.MarshalIndent(r.st, "", "  ")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(p), 0o755)
	}
	if err == nil {
		err = os.WriteFile(p+".tmp", b, 0o600)
	}
	if err == nil {
		err = os.Rename(p+".tmp", p)
	}
	if err != nil {
		r.log.Error("save state", "err", err)
	}
}

func (r *runner) loadState() {
	p := r.statePath()
	if p == "" {
		return
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var st persisted
	if err == nil {
		err = json.Unmarshal(b, &st)
	}
	snap := tree.Tree{}
	for _, l := range st.Snapshot {
		if err != nil {
			break
		}
		var path tree.Path
		var v any
		if path, err = tree.ParsePath(l.Path); err == nil {
			v, err = tree.DecodeJSON(l.Value)
		}
		snap[path.Canon()] = tree.Leaf{Path: path, Value: v}
	}
	if err != nil {
		r.log.Warn("ignoring unreadable state; will re-apply git", "err", err)
		return
	}
	r.st, r.snapshot = st, snap
	r.setStatus(func(s *DeviceStatus) {
		s.Revision, s.DesiredHash, s.LastApply, s.DriftPR = st.Revision, st.DesiredHash, st.AppliedAt, st.DriftPRURL
	})
	r.log.Info("resumed from saved state", "revision", st.Revision, "leaves", len(snap))
}
