package desired

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/og124/netflux/internal/tree"
)

const driftHeader = `# Written by netflux: hand changes on %s. Loads last; fold into other files
# when convenient. null removes a value, "_delete: true" a list entry.
`

// MergeDrift layers changes onto an existing drift.yaml.
func MergeDrift(device string, existing []byte, changes []tree.Change) ([]byte, error) {
	sets, dels := tree.Tree{}, map[string]tree.Path{}
	if len(bytes.TrimSpace(existing)) > 0 {
		layers, err := DecodeLayers(bytes.NewReader(existing))
		if err != nil {
			return nil, fmt.Errorf("existing %s: %w", DriftFile, err)
		}
		for _, l := range layers {
			for _, d := range l.Delete {
				sets.DeleteSubtree(d)
				dels[d.Canon()] = d
			}
			for k, v := range l.Set {
				sets[k] = v
			}
		}
	}

	for _, c := range changes {
		switch c.Kind {
		case tree.Added, tree.Changed:
			for k, d := range dels {
				if c.Path.HasPrefix(d) {
					delete(dels, k)
				}
			}
			sets[c.Path.Canon()] = tree.Leaf{Path: c.Path, Value: c.New}
		case tree.Removed:
			p := c.Path
			// A removed key means a removed entry.
			if n := len(p); n >= 2 {
				if _, isKey := p[n-2].Keys[p[n-1].Name]; isKey {
					p = p[:n-1]
				}
			}
			sets.DeleteSubtree(p)
			dels[p.Canon()] = p
		}
	}
	// Drop deletes inside deleted entries.
	for k, d := range dels {
		for k2, d2 := range dels {
			if k != k2 && d.HasPrefix(d2) {
				delete(dels, k)
				break
			}
		}
	}

	out := sets.Clone()
	for _, d := range dels {
		if last := d[len(d)-1]; len(last.Keys) > 0 {
			p := d.Child(tree.Elem{Name: tree.DeleteMember})
			out[p.Canon()] = tree.Leaf{Path: p, Value: true}
		} else {
			out[d.Canon()] = tree.Leaf{Path: d, Value: nil}
		}
	}

	return EncodeYAML(fmt.Sprintf(driftHeader, device), out)
}

// EncodeYAML renders t as a desired-state file.
func EncodeYAML(header string, t tree.Tree) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(header)
	v := tree.Unflatten(t, tree.Path{})
	if v == nil {
		return buf.Bytes(), nil
	}
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
