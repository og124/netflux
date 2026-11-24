// Package tree models config as a flat map of leaf paths.
//
// Paths keep their raw form (sent on the wire) and a canonical form, with
// YANG module prefixes stripped, used for comparison.
package tree

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	gnmipb "github.com/openconfig/gnmi/proto/gnmi"
)

// Elem is one step of a path: a node name and, for a list entry, its keys.
type Elem struct {
	Name string
	Keys map[string]string
}

// Path is a schema path from the root.
type Path []Elem

// ParsePath parses a gNMI path string; "\]" escapes a "]" in a key.
func ParsePath(s string) (Path, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "/" {
		return Path{}, nil
	}
	if s[0] == '/' {
		s = s[1:]
	}
	var p Path
	var cur Elem
	var name strings.Builder
	inName := true
	flush := func() error {
		cur.Name = name.String()
		if cur.Name == "" {
			return fmt.Errorf("empty element in path %q", s)
		}
		p = append(p, cur)
		cur, inName = Elem{}, true
		name.Reset()
		return nil
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '/' && inName:
			if err := flush(); err != nil {
				return nil, err
			}
		case c == '[':
			inName = false
			j := i + 1
			var k, v strings.Builder
			for j < len(s) && s[j] != '=' {
				k.WriteByte(s[j])
				j++
			}
			if j == len(s) {
				return nil, fmt.Errorf("unterminated key in path %q", s)
			}
			j++
			for j < len(s) && s[j] != ']' {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				v.WriteByte(s[j])
				j++
			}
			if j == len(s) {
				return nil, fmt.Errorf("unterminated key in path %q", s)
			}
			if cur.Keys == nil {
				cur.Keys = map[string]string{}
			}
			cur.Keys[k.String()] = v.String()
			i = j
		case c == '/':
			if err := flush(); err != nil {
				return nil, err
			}
		default:
			if !inName {
				return nil, fmt.Errorf("unexpected %q after keys in path %q", c, s)
			}
			name.WriteByte(c)
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return p, nil
}

// MustParse is ParsePath that panics.
func MustParse(s string) Path {
	p, err := ParsePath(s)
	if err != nil {
		panic(err)
	}
	return p
}

var keyEscaper = strings.NewReplacer(`\`, `\\`, `]`, `\]`)

func (e Elem) str(canon bool) string {
	name := e.Name
	if canon {
		name = stripModule(name)
	}
	if len(e.Keys) == 0 {
		return name
	}
	type kv struct{ k, v string }
	kvs := make([]kv, 0, len(e.Keys))
	for k, v := range e.Keys {
		if canon {
			k, v = stripModule(k), stripIdentity(v)
		}
		kvs = append(kvs, kv{k, v})
	}
	sort.Slice(kvs, func(i, j int) bool { return kvs[i].k < kvs[j].k })
	var b strings.Builder
	b.WriteString(name)
	for _, x := range kvs {
		fmt.Fprintf(&b, "[%s=%s]", x.k, keyEscaper.Replace(x.v))
	}
	return b.String()
}

// String is the raw form of the path.
func (p Path) String() string { return p.join(false) }

// Canon is the canonical form, a leaf's identity.
func (p Path) Canon() string { return p.join(true) }

func (p Path) join(canon bool) string {
	if len(p) == 0 {
		return "/"
	}
	var b strings.Builder
	for _, e := range p {
		b.WriteByte('/')
		b.WriteString(e.str(canon))
	}
	return b.String()
}

// Child returns p plus e, without aliasing p.
func (p Path) Child(e Elem) Path {
	q := make(Path, len(p), len(p)+1)
	copy(q, p)
	return append(q, e)
}

// HasPrefix reports whether q is p or an ancestor of p.
func (p Path) HasPrefix(q Path) bool {
	if len(q) > len(p) {
		return false
	}
	for i := range q {
		if p[i].str(true) != q[i].str(true) {
			return false
		}
	}
	return true
}

// Match reports whether pattern matches p or an ancestor. "*" matches any
// element, key values are globs, and unlisted keys match anything.
func (p Path) Match(pattern Path) bool {
	if len(pattern) > len(p) {
		return false
	}
	for i, pe := range pattern {
		e := p[i]
		if pe.Name != "*" && stripModule(pe.Name) != stripModule(e.Name) {
			return false
		}
		for pk, pv := range pe.Keys {
			v, ok := lookupKey(e.Keys, pk)
			if !ok {
				return false
			}
			if ok, _ := path.Match(stripIdentity(pv), stripIdentity(v)); !ok {
				return false
			}
		}
	}
	return true
}

func lookupKey(keys map[string]string, k string) (string, bool) {
	k = stripModule(k)
	for kk, v := range keys {
		if stripModule(kk) == k {
			return v, true
		}
	}
	return "", false
}

// MatchAny reports whether any of patterns matches p.
func (p Path) MatchAny(patterns []Path) bool {
	for _, pat := range patterns {
		if p.Match(pat) {
			return true
		}
	}
	return false
}

// ToGNMI converts p to a gNMI path.
func (p Path) ToGNMI() *gnmipb.Path {
	g := &gnmipb.Path{}
	for _, e := range p {
		pe := &gnmipb.PathElem{Name: e.Name}
		if len(e.Keys) > 0 {
			pe.Key = make(map[string]string, len(e.Keys))
			for k, v := range e.Keys {
				pe.Key[k] = v
			}
		}
		g.Elem = append(g.Elem, pe)
	}
	return g
}

// FromGNMI converts a gNMI path, including the legacy string form.
func FromGNMI(g *gnmipb.Path) Path {
	if g == nil {
		return Path{}
	}
	if len(g.Elem) == 0 && len(g.Element) > 0 { //nolint:staticcheck // legacy devices
		p, err := ParsePath(strings.Join(g.Element, "/")) //nolint:staticcheck
		if err == nil {
			return p
		}
	}
	p := make(Path, 0, len(g.Elem))
	for _, pe := range g.Elem {
		e := Elem{Name: pe.Name}
		if len(pe.Key) > 0 {
			e.Keys = make(map[string]string, len(pe.Key))
			for k, v := range pe.Key {
				e.Keys[k] = v
			}
		}
		p = append(p, e)
	}
	return p
}

// Join joins a notification's prefix and path.
func Join(prefix, p *gnmipb.Path) Path {
	return append(FromGNMI(prefix), FromGNMI(p)...)
}

// stripModule drops a YANG module prefix from a name.
func stripModule(s string) string {
	if i := strings.IndexByte(s, ':'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// identityRE matches a module-qualified identity. Narrow on purpose: IPv6
// and most free text don't match.
var identityRE = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)+:([a-zA-Z_][a-zA-Z0-9_.-]*)$`)

func stripIdentity(s string) string {
	if m := identityRE.FindStringSubmatch(s); m != nil {
		return m[len(m)-1]
	}
	return s
}
