// Package device is a gNMI client for one device.
package device

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	gnmipb "github.com/openconfig/gnmi/proto/gnmi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/og124/netflux/internal/desired"
	"github.com/og124/netflux/internal/tree"
)

// Target is how to reach a device.
type Target struct {
	Address    string
	Username   string
	Password   string
	Insecure   bool // plaintext gRPC
	SkipVerify bool
	CAFile     string
	Timeout    time.Duration
}

// TargetFor builds a Target from a device's inventory entry.
func TargetFor(d *desired.Device) Target {
	t := Target{
		Address:  d.Spec.Address,
		Username: d.Spec.Username,
		Password: d.Spec.PasswordValue(),
		CAFile:   d.Spec.CAFile,
		Timeout:  30 * time.Second,
	}
	if d.Spec.Insecure != nil {
		t.Insecure = *d.Spec.Insecure
	}
	if d.Spec.SkipVerify != nil {
		t.SkipVerify = *d.Spec.SkipVerify
	}
	return t
}

// Client is a gNMI client for one device.
type Client struct {
	conn    *grpc.ClientConn
	gnmi    gnmipb.GNMIClient
	timeout time.Duration
}

// Dial connects lazily; errors surface on the first RPC.
func Dial(t Target) (*Client, error) {
	var creds credentials.TransportCredentials
	switch {
	case t.Insecure:
		creds = insecure.NewCredentials()
	default:
		cfg := &tls.Config{InsecureSkipVerify: t.SkipVerify} //nolint:gosec // opt-in, for lab self-signed certs
		if t.CAFile != "" {
			pem, err := os.ReadFile(t.CAFile)
			if err != nil {
				return nil, err
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("%s: no certificates", t.CAFile)
			}
			cfg.RootCAs = pool
		}
		creds = credentials.NewTLS(cfg)
	}
	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}
	if t.Username != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(basicAuth{t.Username, t.Password, !t.Insecure}))
	}
	conn, err := grpc.NewClient(t.Address, opts...)
	if err != nil {
		return nil, err
	}
	timeout := t.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Client{conn: conn, gnmi: gnmipb.NewGNMIClient(conn), timeout: timeout}, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

// basicAuth sends username/password metadata.
type basicAuth struct {
	user, pass string
	secure     bool
}

func (a basicAuth) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"username": a.user, "password": a.pass}, nil
}

func (a basicAuth) RequireTransportSecurity() bool { return a.secure }

// GetConfig fetches and flattens config under roots. Missing roots are empty.
func (c *Client) GetConfig(ctx context.Context, roots []tree.Path) (tree.Tree, error) {
	out := tree.Tree{}
	for _, r := range roots {
		ctx, cancel := context.WithTimeout(ctx, c.timeout)
		resp, err := c.gnmi.Get(ctx, &gnmipb.GetRequest{
			Path:     []*gnmipb.Path{r.ToGNMI()},
			Type:     gnmipb.GetRequest_CONFIG,
			Encoding: gnmipb.Encoding_JSON_IETF,
		})
		cancel()
		if status.Code(err) == codes.NotFound {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("get %s: %w", r, err)
		}
		for _, n := range resp.Notification {
			for _, u := range n.Update {
				p := tree.Join(n.Prefix, u.Path)
				v, err := decode(u.Val)
				if err != nil {
					return nil, fmt.Errorf("get %s: %s: %w", r, p, err)
				}
				l, err := tree.Flatten(p, v)
				if err != nil {
					return nil, fmt.Errorf("get %s: %w", r, err)
				}
				out.Apply(&tree.Layer{Set: l.Set})
			}
		}
	}
	return out, nil
}

// Op is one operation in a Set.
type Op struct {
	Kind   string // "delete", "replace" or "update"
	Origin string
	Path   tree.Path
	Value  any // JSON, or a string for origin cli
}

func (o Op) String() string {
	origin := ""
	if o.Origin != "" {
		origin = o.Origin + ":"
	}
	return fmt.Sprintf("%s %s%s", o.Kind, origin, o.Path)
}

// Set sends ops as one atomic SetRequest.
func (c *Client) Set(ctx context.Context, ops []Op) error {
	if len(ops) == 0 {
		return nil
	}
	req := &gnmipb.SetRequest{}
	for _, o := range ops {
		p := o.Path.ToGNMI()
		p.Origin = o.Origin
		if o.Kind == "delete" {
			req.Delete = append(req.Delete, p)
			continue
		}
		var val *gnmipb.TypedValue
		if s, ok := o.Value.(string); ok && o.Origin == "cli" {
			val = &gnmipb.TypedValue{Value: &gnmipb.TypedValue_AsciiVal{AsciiVal: s}}
		} else {
			b, err := json.Marshal(o.Value)
			if err != nil {
				return fmt.Errorf("%s: %w", o, err)
			}
			val = &gnmipb.TypedValue{Value: &gnmipb.TypedValue_JsonIetfVal{JsonIetfVal: b}}
		}
		u := &gnmipb.Update{Path: p, Val: val}
		if o.Kind == "replace" {
			req.Replace = append(req.Replace, u)
		} else {
			req.Update = append(req.Update, u)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	_, err := c.gnmi.Set(ctx, req)
	return err
}

// Subscribe streams ON_CHANGE updates under roots, calling onChange for
// config changes and onSync once subscribed. It always returns an error.
func (c *Client) Subscribe(ctx context.Context, roots []tree.Path, onSync func(), onChange func([]tree.Path)) error {
	sl := &gnmipb.SubscriptionList{
		Mode:        gnmipb.SubscriptionList_STREAM,
		Encoding:    gnmipb.Encoding_JSON_IETF,
		UpdatesOnly: true,
	}
	for _, r := range roots {
		sl.Subscription = append(sl.Subscription, &gnmipb.Subscription{
			Path: r.ToGNMI(),
			Mode: gnmipb.SubscriptionMode_ON_CHANGE,
		})
	}
	stream, err := c.gnmi.Subscribe(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&gnmipb.SubscribeRequest{Request: &gnmipb.SubscribeRequest_Subscribe{Subscribe: sl}}); err != nil {
		return err
	}
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return errors.New("subscription closed by device")
		}
		if err != nil {
			return err
		}
		if resp.GetSyncResponse() {
			onSync()
			continue
		}
		n := resp.GetUpdate()
		if n == nil {
			continue
		}
		var touched []tree.Path
		for _, u := range n.Update {
			if p := tree.Join(n.Prefix, u.Path); isConfig(p, u.Val) {
				touched = append(touched, p)
			}
		}
		for _, d := range n.Delete {
			touched = append(touched, tree.Join(n.Prefix, d))
		}
		if len(touched) > 0 {
			onChange(touched)
		}
	}
}

// isConfig reports whether an update touches a config container.
func isConfig(p tree.Path, v *gnmipb.TypedValue) bool {
	for _, e := range p {
		if e.Name == "config" || strings.HasSuffix(e.Name, ":config") {
			return true
		}
		if e.Name == "state" || strings.HasSuffix(e.Name, ":state") {
			return false
		}
	}
	val, err := decode(v)
	if err != nil {
		return false
	}
	l, err := tree.Flatten(p, val)
	if err != nil {
		return false
	}
	for _, leaf := range l.Set {
		for _, e := range leaf.Path[len(p):] {
			if e.Name == "config" {
				return true
			}
		}
	}
	return false
}

func decode(v *gnmipb.TypedValue) (any, error) {
	switch x := v.GetValue().(type) {
	case *gnmipb.TypedValue_JsonIetfVal:
		return tree.DecodeJSON(x.JsonIetfVal)
	case *gnmipb.TypedValue_JsonVal:
		return tree.DecodeJSON(x.JsonVal)
	case *gnmipb.TypedValue_StringVal:
		return x.StringVal, nil
	case *gnmipb.TypedValue_AsciiVal:
		return x.AsciiVal, nil
	case *gnmipb.TypedValue_IntVal:
		return x.IntVal, nil
	case *gnmipb.TypedValue_UintVal:
		return x.UintVal, nil
	case *gnmipb.TypedValue_BoolVal:
		return x.BoolVal, nil
	case *gnmipb.TypedValue_DoubleVal:
		return x.DoubleVal, nil
	case *gnmipb.TypedValue_FloatVal: //nolint:staticcheck // still sent by some targets
		return float64(x.FloatVal), nil //nolint:staticcheck
	case *gnmipb.TypedValue_LeaflistVal:
		var out []any
		for _, e := range x.LeaflistVal.Element {
			d, err := decode(e)
			if err != nil {
				return nil, err
			}
			out = append(out, d)
		}
		return out, nil
	case nil:
		return nil, nil
	}
	return nil, fmt.Errorf("unsupported value type %T", v.GetValue())
}
