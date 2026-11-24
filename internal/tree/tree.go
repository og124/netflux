package tree

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Leaf is one value: a scalar, or []any for a leaf-list.
type Leaf struct {
	Path  Path
	Value any
}

// Tree is flattened config keyed by canonical path.
type Tree map[string]Leaf

// Layer is one config source: leaves set and subtrees deleted.
type Layer struct {
	Set    Tree
	Delete []Path
}

// DeleteMember in a list entry deletes the entry when layering.
const DeleteMember = "_delete"

// Flatten turns an RFC 7951 value at base into a Layer. A list is an array of
// objects whose scalar members are its keys, as OpenConfig guarantees. null
// deletes a subtree.
func Flatten(base Path, v any) (*Layer, error) {
	l := &Layer{Set: Tree{}}
	var err error
	if arr, ok := v.([]any); ok && len(base) > 0 && len(base[len(base)-1].Keys) == 0 {
		n := len(base) - 1
		err = l.member(base[:n], base[n].Name, arr)
	} else {
		err = l.node(base, v)
	}
	if err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Layer) node(p Path, v any) error {
	switch x := v.(type) {
	case nil:
		l.Delete = append(l.Delete, p)
	case map[string]any:
		for _, k := range sortedKeys(x) {
			if k == DeleteMember {
				return fmt.Errorf("%s: %q is only valid inside a list entry", p, DeleteMember)
			}
			if err := l.member(p, k, x[k]); err != nil {
				return err
			}
		}
	default:
		s, err := scalar(v)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		l.Set[p.Canon()] = Leaf{Path: p, Value: s}
	}
	return nil
}

func (l *Layer) member(parent Path, name string, v any) error {
	arr, ok := v.([]any)
	if !ok {
		return l.node(parent.Child(Elem{Name: name}), v)
	}
	p := parent.Child(Elem{Name: name})
	maps := 0
	for _, e := range arr {
		if _, ok := e.(map[string]any); ok {
			maps++
		}
	}
	switch {
	case maps == 0:
		vals := make([]any, len(arr))
		for i, e := range arr {
			s, err := scalar(e)
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			vals[i] = s
		}
		l.Set[p.Canon()] = Leaf{Path: p, Value: vals}
		return nil
	case maps != len(arr):
		return fmt.Errorf("%s: list mixes entries and scalars", p)
	}
	for _, e := range arr {
		if err := l.entry(parent, name, e.(map[string]any)); err != nil {
			return err
		}
	}
	return nil
}

func (l *Layer) entry(parent Path, name string, m map[string]any) error {
	keys := map[string]string{}
	for k, v := range m {
		if k == DeleteMember || isComposite(v) {
			continue
		}
		s, err := scalar(v)
		if err != nil {
			return fmt.Errorf("%s/%s: key %s: %w", parent, name, k, err)
		}
		keys[k] = scalarString(s)
	}
	if len(keys) == 0 {
		return fmt.Errorf("%s/%s: list entry has no key leaves (every OpenConfig list entry repeats its keys beside config)", parent, name)
	}
	p := parent.Child(Elem{Name: name, Keys: keys})
	if del, _ := m[DeleteMember].(bool); del {
		l.Delete = append(l.Delete, p)
		return nil
	}
	for _, k := range sortedKeys(m) {
		if k == DeleteMember {
			continue
		}
		if err := l.member(p, k, m[k]); err != nil {
			return err
		}
	}
	return nil
}

func isComposite(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return true
	}
	return false
}

// scalar normalises a decoded scalar.
func scalar(v any) (any, error) {
	switch x := v.(type) {
	case nil, string, bool, int64, uint64, float64, json.Number:
		return v, nil
	case int:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case uint32:
		return uint64(x), nil
	case uint:
		return uint64(x), nil
	case float32:
		return float64(x), nil
	case time.Time:
		return x.Format(time.RFC3339Nano), nil
	}
	return nil, fmt.Errorf("unsupported value %v (%T)", v, v)
}

func scalarString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case json.Number:
		return x.String()
	}
	return fmt.Sprint(v)
}

// ValuesEqual compares leaf values loosely: numbers by value, identities with
// or without module prefix, leaf-lists as sets.
func ValuesEqual(a, b any) bool {
	al, aok := a.([]any)
	bl, bok := b.([]any)
	if aok != bok {
		return false
	}
	if aok {
		if len(al) != len(bl) {
			return false
		}
		as, bs := sortedStrings(al), sortedStrings(bl)
		for i := range as {
			if !stringsEqual(as[i], bs[i]) {
				return false
			}
		}
		return true
	}
	return stringsEqual(scalarString(a), scalarString(b))
}

func stringsEqual(a, b string) bool {
	return a == b || stripIdentity(a) == b || a == stripIdentity(b)
}

func sortedStrings(vs []any) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = scalarString(v)
	}
	sort.Slice(out, func(i, j int) bool { return stripIdentity(out[i]) < stripIdentity(out[j]) })
	return out
}

// FormatValue renders a value for humans.
func FormatValue(v any) string {
	if l, ok := v.([]any); ok {
		parts := make([]string, len(l))
		for i, e := range l {
			parts[i] = scalarString(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	return scalarString(v)
}

// Apply merges l into t: deletes, then sets.
func (t Tree) Apply(l *Layer) {
	for _, d := range l.Delete {
		t.DeleteSubtree(d)
	}
	for k, v := range l.Set {
		t[k] = v
	}
}

// DeleteSubtree removes every leaf at or under p.
func (t Tree) DeleteSubtree(p Path) {
	for k, l := range t {
		if l.Path.HasPrefix(p) {
			delete(t, k)
		}
	}
}

// Subtree returns the leaves at or under root.
func (t Tree) Subtree(root Path) Tree {
	out := Tree{}
	for k, l := range t {
		if l.Path.HasPrefix(root) {
			out[k] = l
		}
	}
	return out
}

// Matching returns the leaves matched by any of patterns.
func (t Tree) Matching(patterns []Path) Tree {
	out := Tree{}
	for k, l := range t {
		if l.Path.MatchAny(patterns) {
			out[k] = l
		}
	}
	return out
}

// Without returns the leaves matched by none of patterns.
func (t Tree) Without(patterns []Path) Tree {
	out := Tree{}
	for k, l := range t {
		if !l.Path.MatchAny(patterns) {
			out[k] = l
		}
	}
	return out
}

// Clone returns a shallow copy.
func (t Tree) Clone() Tree {
	out := make(Tree, len(t))
	for k, v := range t {
		out[k] = v
	}
	return out
}

// Keys returns the paths, sorted.
func (t Tree) Keys() []string {
	ks := make([]string, 0, len(t))
	for k := range t {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// ChangeKind says how a leaf differs.
type ChangeKind string

const (
	Added   ChangeKind = "added"
	Removed ChangeKind = "removed"
	Changed ChangeKind = "changed"
)

// Change is one leaf that differs between two trees.
type Change struct {
	Kind ChangeKind
	Path Path
	Old  any
	New  any
}

func (c Change) String() string {
	switch c.Kind {
	case Added:
		return fmt.Sprintf("+ %s = %s", c.Path, FormatValue(c.New))
	case Removed:
		return fmt.Sprintf("- %s (was %s)", c.Path, FormatValue(c.Old))
	}
	return fmt.Sprintf("~ %s: %s -> %s", c.Path, FormatValue(c.Old), FormatValue(c.New))
}

// Diff lists changes from a to b, sorted by path.
func Diff(a, b Tree) []Change {
	var out []Change
	for k, al := range a {
		bl, ok := b[k]
		switch {
		case !ok:
			out = append(out, Change{Kind: Removed, Path: al.Path, Old: al.Value})
		case !ValuesEqual(al.Value, bl.Value):
			out = append(out, Change{Kind: Changed, Path: bl.Path, Old: al.Value, New: bl.Value})
		}
	}
	for k, bl := range b {
		if _, ok := a[k]; !ok {
			out = append(out, Change{Kind: Added, Path: bl.Path, New: bl.Value})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path.Canon() < out[j].Path.Canon() })
	return out
}

// Unflatten rebuilds the RFC 7951 value under root, or nil if empty. List
// keys come first, then config, then the rest.
func Unflatten(t Tree, root Path) any {
	top := &bnode{}
	found := false
	for _, k := range t.Keys() {
		l := t[k]
		if !l.Path.HasPrefix(root) {
			continue
		}
		found = true
		n := top
		for _, e := range l.Path[len(root):] {
			n = n.child(e)
		}
		n.leaf, n.val = true, l.Value
	}
	if !found {
		return nil
	}
	if top.leaf {
		return top.val
	}
	var rootKeys map[string]string
	if len(root) > 0 {
		rootKeys = root[len(root)-1].Keys
	}
	return top.render(rootKeys)
}

type bnode struct {
	elem  Elem
	leaf  bool
	val   any
	kids  map[string]*bnode
	order []string
}

func (n *bnode) child(e Elem) *bnode {
	k := e.str(true)
	if c, ok := n.kids[k]; ok {
		return c
	}
	if n.kids == nil {
		n.kids = map[string]*bnode{}
	}
	c := &bnode{elem: e}
	n.kids[k] = c
	n.order = append(n.order, k)
	return c
}

// render builds a container or list entry object.
func (n *bnode) render(keys map[string]string) Object {
	type group struct {
		name    string
		entries []*bnode
		single  *bnode
	}
	groups := map[string]*group{}
	var names []string
	for _, k := range n.order {
		c := n.kids[k]
		gn := stripModule(c.elem.Name)
		g, ok := groups[gn]
		if !ok {
			g = &group{name: c.elem.Name}
			groups[gn] = g
			names = append(names, gn)
		}
		if len(c.elem.Keys) > 0 {
			g.entries = append(g.entries, c)
		} else {
			g.single = c
		}
	}
	keyNames := map[string]bool{}
	for k := range keys {
		keyNames[stripModule(k)] = true
	}
	sort.Slice(names, func(i, j int) bool {
		ri, rj := memberRank(names[i], keyNames), memberRank(names[j], keyNames)
		if ri != rj {
			return ri < rj
		}
		return names[i] < names[j]
	})
	var obj Object
	seen := map[string]bool{}
	for _, gn := range names {
		g := groups[gn]
		seen[gn] = true
		if len(g.entries) > 0 {
			sort.Slice(g.entries, func(i, j int) bool {
				return g.entries[i].elem.str(true) < g.entries[j].elem.str(true)
			})
			arr := make([]any, len(g.entries))
			for i, e := range g.entries {
				arr[i] = e.render(e.elem.Keys)
			}
			obj = append(obj, Member{Key: g.name, Value: arr})
			continue
		}
		c := g.single
		if c.leaf {
			obj = append(obj, Member{Key: g.name, Value: c.val})
		} else {
			obj = append(obj, Member{Key: g.name, Value: c.render(nil)})
		}
	}
	// Keys known only from the path.
	var missing Object
	for _, k := range sortedKeys(keys) {
		if !seen[stripModule(k)] {
			missing = append(missing, Member{Key: k, Value: keys[k]})
		}
	}
	return append(missing, obj...)
}

func memberRank(name string, keys map[string]bool) int {
	switch {
	case keys[name]:
		return 0
	case name == "config":
		return 1
	}
	return 2
}

// Object is a JSON object that keeps its member order.
type Object []Member

// Member is one member of an Object.
type Member struct {
	Key   string
	Value any
}

// MarshalJSON writes the members in order.
func (o Object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(m.Key)
		b.Write(k)
		b.WriteByte(':')
		v, err := json.Marshal(m.Value)
		if err != nil {
			return nil, err
		}
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// MarshalYAML writes the members in order.
func (o Object) MarshalYAML() (any, error) {
	n := &yaml.Node{Kind: yaml.MappingNode}
	for _, m := range o {
		var v yaml.Node
		if err := v.Encode(yamlValue(m.Value)); err != nil {
			return nil, err
		}
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: m.Key}, &v)
	}
	return n, nil
}

// yamlValue renders json.Number as a YAML number.
func yamlValue(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		if f, err := x.Float64(); err == nil {
			return f
		}
		return x.String()
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = yamlValue(e)
		}
		return out
	}
	return v
}

// DecodeJSON decodes JSON, keeping numbers exact.
func DecodeJSON(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// Normalize converts decoded YAML to map[string]any.
func Normalize(v any) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, c := range x {
			n, err := Normalize(c)
			if err != nil {
				return nil, err
			}
			out[k] = n
		}
		return out, nil
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, c := range x {
			n, err := Normalize(c)
			if err != nil {
				return nil, err
			}
			out[fmt.Sprint(k)] = n
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, c := range x {
			n, err := Normalize(c)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	}
	return v, nil
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
