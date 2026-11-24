package controller

import (
	"github.com/og124/netflux/internal/desired"
	"github.com/og124/netflux/internal/device"
	"github.com/og124/netflux/internal/tree"
)

// Plan is one apply.
type Plan struct {
	// Effective is git plus preserved live config.
	Effective tree.Tree
	// Ops replace every root, atomically.
	Ops []device.Op
	// Vendor ops follow in a second Set.
	Vendor []device.Op
}

// BuildPlan replaces every root. An empty root is cleared, or deleted if it
// is a list entry.
func BuildPlan(d *desired.Device, live tree.Tree) (Plan, error) {
	eff := d.Config.Clone()
	for k, l := range live.Matching(d.Preserve) {
		eff[k] = l
	}
	p := Plan{Effective: eff}
	for _, r := range d.Roots {
		v := tree.Unflatten(eff, r)
		if v == nil {
			if len(r) > 0 && len(r[len(r)-1].Keys) > 0 {
				p.Ops = append(p.Ops, device.Op{Kind: "delete", Path: r})
				continue
			}
			v = map[string]any{}
		}
		p.Ops = append(p.Ops, device.Op{Kind: "replace", Path: r, Value: v})
	}
	for _, v := range d.Vendor {
		path, err := tree.ParsePath(v.Path)
		if err != nil {
			return Plan{}, err
		}
		p.Vendor = append(p.Vendor, device.Op{Kind: v.Op, Origin: v.Origin, Path: path, Value: v.Value})
	}
	return p, nil
}

// Changes is what the plan would change. Removals may include defaults the
// device restores.
func (p Plan) Changes(live tree.Tree, ignore []tree.Path) []tree.Change {
	return tree.Diff(live.Without(ignore), p.Effective.Without(ignore))
}

// Unconverged lists git leaves the device doesn't hold after an apply.
func Unconverged(eff, live tree.Tree, ignore []tree.Path) []tree.Change {
	var out []tree.Change
	for _, c := range tree.Diff(live.Without(ignore), eff.Without(ignore)) {
		if c.Kind != tree.Removed {
			out = append(out, c)
		}
	}
	return out
}
