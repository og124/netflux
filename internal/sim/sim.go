// Package sim is an in-memory gNMI switch for development and demos. It
// fills in defaults like a real device but doesn't validate YANG.
package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	gnmipb "github.com/openconfig/gnmi/proto/gnmi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/og124/netflux/internal/tree"
)

// Target is one simulated device.
type Target struct {
	gnmipb.UnimplementedGNMIServer

	name       string
	user, pass string
	log        *slog.Logger

	mu   sync.Mutex
	cfg  tree.Tree
	cli  []string
	subs map[chan *gnmipb.Notification]struct{}
}

// New returns a target seeded like a fresh lab switch.
func New(name, user, pass string, log *slog.Logger) *Target {
	t := &Target{name: name, user: user, pass: pass, log: log, cfg: tree.Tree{}, subs: map[chan *gnmipb.Notification]struct{}{}}
	seed := fmt.Sprintf(`{
	  "system": {
	    "config": {"hostname": %q},
	    "aaa": {"authentication": {"users": {"user": [
	      {"username": %q, "config": {"username": %q, "role": "network-admin"}}]}}},
	    "grpc-servers": {"grpc-server": [
	      {"name": "default", "config": {"name": "default", "enable": true, "port": 6030}}]}
	  },
	  "interfaces": {"interface": [
	    {"name": "Management0", "config": {"name": "Management0", "type": "iana-if-type:ethernetCsmacd"},
	     "subinterfaces": {"subinterface": [{"index": 0, "config": {"index": 0},
	       "ipv4": {"addresses": {"address": [{"ip": "172.20.20.2", "config": {"ip": "172.20.20.2", "prefix-length": 24}}]}}}]}},
	    {"name": "Ethernet1", "config": {"name": "Ethernet1", "type": "iana-if-type:ethernetCsmacd"}}
	  ]},
	  "network-instances": {"network-instance": [
	    {"name": "default", "config": {"name": "default", "type": "openconfig-network-instance-types:DEFAULT_INSTANCE"},
	     "vlans": {"vlan": [{"vlan-id": 1, "config": {"vlan-id": 1, "name": "default"}}]}}
	  ]}
	}`, name, user, user)
	v, err := tree.DecodeJSON([]byte(seed))
	if err != nil {
		panic(err)
	}
	l, err := tree.Flatten(tree.Path{}, v)
	if err != nil {
		panic(err)
	}
	t.cfg.Apply(l)
	fillDefaults(t.cfg)
	return t
}

// Serve serves gNMI on lis.
func (t *Target) Serve(lis net.Listener) error {
	s := grpc.NewServer()
	gnmipb.RegisterGNMIServer(s, t)
	return s.Serve(lis)
}

// Config returns a copy of the running config.
func (t *Target) Config() tree.Tree {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cfg.Clone()
}

// CLI returns the origin-cli values received.
func (t *Target) CLI() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.cli...)
}

func (t *Target) auth(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	if first(md["username"]) != t.user || first(md["password"]) != t.pass {
		return status.Error(codes.Unauthenticated, "bad username or password")
	}
	return nil
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

// Capabilities advertises JSON_IETF.
func (t *Target) Capabilities(ctx context.Context, _ *gnmipb.CapabilityRequest) (*gnmipb.CapabilityResponse, error) {
	if err := t.auth(ctx); err != nil {
		return nil, err
	}
	return &gnmipb.CapabilityResponse{
		SupportedEncodings: []gnmipb.Encoding{gnmipb.Encoding_JSON_IETF},
		GNMIVersion:        "0.10.0",
	}, nil
}

// Get returns each path as JSON_IETF, or NotFound.
func (t *Target) Get(ctx context.Context, req *gnmipb.GetRequest) (*gnmipb.GetResponse, error) {
	if err := t.auth(ctx); err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	resp := &gnmipb.GetResponse{}
	for _, gp := range req.Path {
		p := tree.Join(req.Prefix, gp)
		v := tree.Unflatten(t.cfg, p)
		if v == nil {
			return nil, status.Errorf(codes.NotFound, "%s: not found", p)
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		resp.Notification = append(resp.Notification, &gnmipb.Notification{
			Timestamp: time.Now().UnixNano(),
			Update:    []*gnmipb.Update{{Path: p.ToGNMI(), Val: &gnmipb.TypedValue{Value: &gnmipb.TypedValue_JsonIetfVal{JsonIetfVal: b}}}},
		})
	}
	return resp, nil
}

// Set applies deletes, replaces, then updates, atomically.
func (t *Target) Set(ctx context.Context, req *gnmipb.SetRequest) (*gnmipb.SetResponse, error) {
	if err := t.auth(ctx); err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	next := t.cfg.Clone()
	var cli []string
	resp := &gnmipb.SetResponse{Timestamp: time.Now().UnixNano()}
	result := func(p *gnmipb.Path, op gnmipb.UpdateResult_Operation) {
		resp.Response = append(resp.Response, &gnmipb.UpdateResult{Path: p, Op: op})
	}
	for _, gp := range req.Delete {
		next.DeleteSubtree(tree.Join(req.Prefix, gp))
		result(gp, gnmipb.UpdateResult_DELETE)
	}
	apply := func(u *gnmipb.Update, replace bool) error {
		if u.Path.GetOrigin() == "cli" {
			cli = append(cli, u.Val.GetAsciiVal())
			return nil
		}
		if o := u.Path.GetOrigin(); o != "" && o != "openconfig" {
			return status.Errorf(codes.Unimplemented, "origin %q not supported by the simulator", o)
		}
		p := tree.Join(req.Prefix, u.Path)
		var v any
		var err error
		switch x := u.Val.GetValue().(type) {
		case *gnmipb.TypedValue_JsonIetfVal:
			v, err = tree.DecodeJSON(x.JsonIetfVal)
		case *gnmipb.TypedValue_JsonVal:
			v, err = tree.DecodeJSON(x.JsonVal)
		case *gnmipb.TypedValue_StringVal:
			v = x.StringVal
		case *gnmipb.TypedValue_UintVal:
			v = x.UintVal
		case *gnmipb.TypedValue_IntVal:
			v = x.IntVal
		case *gnmipb.TypedValue_BoolVal:
			v = x.BoolVal
		default:
			return status.Errorf(codes.InvalidArgument, "%s: unsupported value %T", p, x)
		}
		if err != nil {
			return status.Errorf(codes.InvalidArgument, "%s: %v", p, err)
		}
		l, err := tree.Flatten(p, v)
		if err != nil {
			return status.Errorf(codes.InvalidArgument, "%v", err)
		}
		if replace {
			next.DeleteSubtree(p)
		}
		next.Apply(l)
		return nil
	}
	for _, u := range req.Replace {
		if err := apply(u, true); err != nil {
			return nil, err
		}
		result(u.Path, gnmipb.UpdateResult_REPLACE)
	}
	for _, u := range req.Update {
		if err := apply(u, false); err != nil {
			return nil, err
		}
		result(u.Path, gnmipb.UpdateResult_UPDATE)
	}
	fillDefaults(next)
	if _, ok := next["/interfaces/interface[name=Management0]/name"]; !ok {
		t.log.Warn("management interface removed: on a real switch this is where you lose the box", "device", t.name)
	}
	changes := tree.Diff(t.cfg, next)
	t.cfg = next
	t.cli = append(t.cli, cli...)
	t.notify(changes)
	t.log.Info("set", "device", t.name, "delete", len(req.Delete), "replace", len(req.Replace), "update", len(req.Update), "leaves_changed", len(changes))
	return resp, nil
}

// fillDefaults adds interface defaults, as devices do.
func fillDefaults(cfg tree.Tree) {
	for _, l := range cfg {
		p := l.Path
		if len(p) != 3 || p[0].Name != "interfaces" || p[1].Name != "interface" || p[2].Name != "name" {
			continue
		}
		cfgPath := p[:2].Child(tree.Elem{Name: "config"})
		for leaf, val := range map[string]any{"enabled": true, "mtu": int64(1500), "name": l.Value} {
			lp := cfgPath.Child(tree.Elem{Name: leaf})
			if _, ok := cfg[lp.Canon()]; !ok {
				cfg[lp.Canon()] = tree.Leaf{Path: lp, Value: val}
			}
		}
	}
}

func (t *Target) notify(changes []tree.Change) {
	if len(changes) == 0 {
		return
	}
	n := &gnmipb.Notification{Timestamp: time.Now().UnixNano()}
	for _, c := range changes {
		if c.Kind == tree.Removed {
			n.Delete = append(n.Delete, c.Path.ToGNMI())
			continue
		}
		b, _ := json.Marshal(c.New)
		n.Update = append(n.Update, &gnmipb.Update{Path: c.Path.ToGNMI(), Val: &gnmipb.TypedValue{Value: &gnmipb.TypedValue_JsonIetfVal{JsonIetfVal: b}}})
	}
	for ch := range t.subs {
		select {
		case ch <- n:
		default: // drop rather than block Set
		}
	}
}

// Subscribe supports STREAM ON_CHANGE.
func (t *Target) Subscribe(stream gnmipb.GNMI_SubscribeServer) error {
	if err := t.auth(stream.Context()); err != nil {
		return err
	}
	req, err := stream.Recv()
	if err != nil {
		return err
	}
	sl := req.GetSubscribe()
	if sl == nil || sl.Mode != gnmipb.SubscriptionList_STREAM {
		return status.Error(codes.Unimplemented, "only STREAM subscriptions are simulated")
	}
	var paths []tree.Path
	for _, s := range sl.Subscription {
		paths = append(paths, tree.Join(sl.Prefix, s.Path))
	}
	under := func(p tree.Path) bool {
		for _, r := range paths {
			if p.HasPrefix(r) {
				return true
			}
		}
		return false
	}

	ch := make(chan *gnmipb.Notification, 64)
	t.mu.Lock()
	t.subs[ch] = struct{}{}
	var initial *gnmipb.Notification
	if !sl.UpdatesOnly {
		initial = &gnmipb.Notification{Timestamp: time.Now().UnixNano()}
		for _, k := range t.cfg.Keys() {
			if l := t.cfg[k]; under(l.Path) {
				b, _ := json.Marshal(l.Value)
				initial.Update = append(initial.Update, &gnmipb.Update{Path: l.Path.ToGNMI(), Val: &gnmipb.TypedValue{Value: &gnmipb.TypedValue_JsonIetfVal{JsonIetfVal: b}}})
			}
		}
	}
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.subs, ch)
		t.mu.Unlock()
	}()

	if initial != nil && len(initial.Update) > 0 {
		if err := stream.Send(&gnmipb.SubscribeResponse{Response: &gnmipb.SubscribeResponse_Update{Update: initial}}); err != nil {
			return err
		}
	}
	if err := stream.Send(&gnmipb.SubscribeResponse{Response: &gnmipb.SubscribeResponse_SyncResponse{SyncResponse: true}}); err != nil {
		return err
	}
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case n := <-ch:
			out := &gnmipb.Notification{Timestamp: n.Timestamp}
			for _, u := range n.Update {
				if under(tree.FromGNMI(u.Path)) {
					out.Update = append(out.Update, u)
				}
			}
			for _, d := range n.Delete {
				if under(tree.FromGNMI(d)) {
					out.Delete = append(out.Delete, d)
				}
			}
			if len(out.Update)+len(out.Delete) == 0 {
				continue
			}
			if err := stream.Send(&gnmipb.SubscribeResponse{Response: &gnmipb.SubscribeResponse_Update{Update: out}}); err != nil {
				return err
			}
		}
	}
}
