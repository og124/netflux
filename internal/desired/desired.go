// Package desired loads each device's desired state from a directory:
//
//	inventory.yaml          devices and what netflux owns on them
//	groups/<group>/*.yaml   shared config
//	devices/<name>/*.yaml   per-device config
//
// Files are OpenConfig trees layered in order: groups, then device files
// alphabetically, then drift.yaml. vendor.yaml holds non-OpenConfig ops.
package desired

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/og124/netflux/internal/tree"
)

const (
	InventoryFile = "inventory.yaml"
	VendorFile    = "vendor.yaml"
	DriftFile     = "drift.yaml"
)

// Mode is how a device handles drift.
type Mode string

const (
	// ModePR opens a PR: merge adopts, close reverts.
	ModePR Mode = "pr"
	// ModeStrict reverts drift at once.
	ModeStrict Mode = "strict"
	// ModeObserve only reports.
	ModeObserve Mode = "observe"
)

// Spec is a device's inventory entry. Unset fields take the defaults; lists
// replace rather than extend.
type Spec struct {
	Address     string   `yaml:"address"`
	Username    string   `yaml:"username"`
	Password    string   `yaml:"password"`
	PasswordEnv string   `yaml:"password_env"`
	Insecure    *bool    `yaml:"insecure"`
	SkipVerify  *bool    `yaml:"skip_verify"`
	CAFile      string   `yaml:"ca_file"`
	Mode        Mode     `yaml:"mode"`
	Groups      []string `yaml:"groups"`
	Roots       []string `yaml:"roots"`
	Preserve    []string `yaml:"preserve"`
	Ignore      []string `yaml:"ignore"`
}

// Inventory is inventory.yaml.
type Inventory struct {
	Defaults Spec            `yaml:"defaults"`
	Devices  map[string]Spec `yaml:"devices"`
}

// VendorOp is one vendor.yaml entry.
type VendorOp struct {
	Reason string `yaml:"reason" json:"reason"`
	Origin string `yaml:"origin" json:"origin"`
	Path   string `yaml:"path,omitempty" json:"path,omitempty"`
	Op     string `yaml:"op" json:"op"`
	Value  any    `yaml:"value,omitempty" json:"value,omitempty"`
	Source string `yaml:"-" json:"-"`
}

// Device is one device's rendered desired state.
type Device struct {
	Name string
	Spec Spec
	// Roots are replaced wholesale on apply.
	Roots []tree.Path
	// Preserve is copied from the live device into every replace.
	Preserve []tree.Path
	// Ignore is excluded from drift detection.
	Ignore []tree.Path
	Config tree.Tree
	Vendor []VendorOp
	// Files are the layers applied, in order.
	Files []string
	// Hash changes only when what netflux would push changes.
	Hash string
}

// LoadInventory reads inventory.yaml.
func LoadInventory(dir string) (*Inventory, error) {
	b, err := os.ReadFile(filepath.Join(dir, InventoryFile))
	if err != nil {
		return nil, err
	}
	var inv Inventory
	dec := yaml.NewDecoder(strings.NewReader(expandEnv(string(b))))
	dec.KnownFields(true)
	if err := dec.Decode(&inv); err != nil {
		return nil, fmt.Errorf("%s: %w", InventoryFile, err)
	}
	if len(inv.Devices) == 0 {
		return nil, fmt.Errorf("%s: no devices", InventoryFile)
	}
	return &inv, nil
}

// expandEnv expands ${VAR} and ${VAR:-default}.
func expandEnv(s string) string {
	return os.Expand(s, func(v string) string {
		name, def, hasDef := strings.Cut(v, ":-")
		if val, ok := os.LookupEnv(name); ok && val != "" {
			return val
		}
		if hasDef {
			return def
		}
		return ""
	})
}

// LoadAll renders every device, collecting all errors.
func LoadAll(dir string) (map[string]*Device, error) {
	inv, err := LoadInventory(dir)
	if err != nil {
		return nil, err
	}
	var errs []error
	out := map[string]*Device{}
	for _, name := range sortedNames(inv.Devices) {
		d, err := inv.Render(dir, name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out[name] = d
	}
	if entries, err := os.ReadDir(filepath.Join(dir, "devices")); err == nil {
		for _, e := range entries {
			if _, ok := inv.Devices[e.Name()]; e.IsDir() && !ok {
				errs = append(errs, fmt.Errorf("devices/%s: not in %s", e.Name(), InventoryFile))
			}
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

// Render builds one device's desired state.
func (inv *Inventory) Render(dir, name string) (*Device, error) {
	spec, ok := inv.Devices[name]
	if !ok {
		return nil, fmt.Errorf("%s: not in %s", name, InventoryFile)
	}
	spec = resolve(inv.Defaults, spec)
	d := &Device{Name: name, Spec: spec, Config: tree.Tree{}}
	fail := func(format string, a ...any) error {
		return fmt.Errorf("%s: "+format, append([]any{name}, a...)...)
	}

	if spec.Address == "" {
		return nil, fail("no address")
	}
	switch spec.Mode {
	case ModePR, ModeStrict, ModeObserve:
	default:
		return nil, fail("mode %q: want pr, strict or observe", spec.Mode)
	}
	var err error
	if d.Roots, err = parsePaths(spec.Roots); err != nil {
		return nil, fail("roots: %v", err)
	}
	if len(d.Roots) == 0 {
		return nil, fail("no roots: netflux has to be told which subtrees it owns")
	}
	for i, a := range d.Roots {
		for j, b := range d.Roots {
			if i != j && a.HasPrefix(b) {
				return nil, fail("root %s is inside root %s", a, b)
			}
		}
	}
	if d.Preserve, err = parsePaths(spec.Preserve); err != nil {
		return nil, fail("preserve: %v", err)
	}
	if d.Ignore, err = parsePaths(spec.Ignore); err != nil {
		return nil, fail("ignore: %v", err)
	}

	var layerDirs []string
	for _, g := range spec.Groups {
		gd := filepath.Join("groups", g)
		if st, err := os.Stat(filepath.Join(dir, gd)); err != nil || !st.IsDir() {
			return nil, fail("group %q: no directory %s", g, gd)
		}
		layerDirs = append(layerDirs, gd)
	}
	layerDirs = append(layerDirs, filepath.Join("devices", name))

	for _, ld := range layerDirs {
		files, err := layerFiles(filepath.Join(dir, ld))
		if err != nil {
			return nil, fail("%v", err)
		}
		for _, f := range files {
			rel := filepath.Join(ld, f)
			if f == VendorFile {
				ops, err := loadVendor(filepath.Join(dir, rel))
				if err != nil {
					return nil, fail("%s: %v", rel, err)
				}
				for i := range ops {
					ops[i].Source = rel
				}
				d.Vendor = append(d.Vendor, ops...)
				d.Files = append(d.Files, rel)
				continue
			}
			layers, err := LoadLayers(filepath.Join(dir, rel))
			if err != nil {
				return nil, fail("%s: %v", rel, err)
			}
			for _, l := range layers {
				d.Config.Apply(l)
			}
			d.Files = append(d.Files, rel)
		}
	}

	var stray []string
	for _, k := range d.Config.Keys() {
		l := d.Config[k]
		if !underAny(l.Path, d.Roots) && isKeyLeaf(l.Path) {
			// A key above a root; the root path carries it.
			delete(d.Config, k)
			continue
		}
		if !underAny(l.Path, d.Roots) {
			stray = append(stray, l.Path.String())
		} else if l.Path.MatchAny(d.Preserve) {
			return nil, fail("%s is preserved (device-owned) but git sets it", l.Path)
		}
	}
	if len(stray) > 0 {
		if len(stray) > 5 {
			stray = append(stray[:5], fmt.Sprintf("... %d more", len(stray)-5))
		}
		return nil, fail("config outside every managed root (add a root, or remove it):\n  %s", strings.Join(stray, "\n  "))
	}
	d.Hash = d.hash()
	return d, nil
}

// LoadLayers reads a YAML file; each document is a layer.
func LoadLayers(path string) ([]*tree.Layer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return DecodeLayers(f)
}

// DecodeLayers is LoadLayers for a reader.
func DecodeLayers(r io.Reader) ([]*tree.Layer, error) {
	dec := yaml.NewDecoder(r)
	var out []*tree.Layer
	for {
		var v any
		err := dec.Decode(&v)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if v == nil {
			continue
		}
		n, err := tree.Normalize(v)
		if err != nil {
			return nil, err
		}
		if _, ok := n.(map[string]any); !ok {
			return nil, fmt.Errorf("top level must be a mapping (an OpenConfig tree rooted at /)")
		}
		l, err := tree.Flatten(tree.Path{}, n)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

func loadVendor(path string) ([]VendorOp, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ops []VendorOp
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&ops); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	for i, op := range ops {
		where := fmt.Sprintf("entry %d", i+1)
		if strings.TrimSpace(op.Reason) == "" {
			return nil, fmt.Errorf("%s: no reason. Vendor config is the exception; say why OpenConfig can't do this", where)
		}
		if op.Origin == "" || op.Origin == "openconfig" {
			return nil, fmt.Errorf("%s: origin %q: vendor ops need a vendor origin (e.g. cli); OpenConfig goes in the tree", where, op.Origin)
		}
		switch op.Op {
		case "":
			ops[i].Op = "update"
		case "update", "replace", "delete":
		default:
			return nil, fmt.Errorf("%s: op %q: want update, replace or delete", where, op.Op)
		}
		if op.Origin == "cli" {
			if _, ok := op.Value.(string); !ok {
				return nil, fmt.Errorf("%s: origin cli takes a string of CLI", where)
			}
		}
		if ops[i].Op != "delete" && op.Value == nil {
			return nil, fmt.Errorf("%s: no value", where)
		}
		if n, err := tree.Normalize(op.Value); err == nil {
			ops[i].Value = n
		}
	}
	return ops, nil
}

func layerFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var files []string
	hasDrift := false
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !isYAML(n) {
			continue
		}
		if n == DriftFile {
			hasDrift = true
			continue
		}
		files = append(files, n)
	}
	sort.Strings(files)
	if hasDrift {
		files = append(files, DriftFile)
	}
	return files, nil
}

func (d *Device) hash() string {
	h := sha256.New()
	for _, k := range d.Config.Keys() {
		l := d.Config[k]
		v, _ := json.Marshal(l.Value)
		fmt.Fprintf(h, "%s=%s\n", l.Path, v)
	}
	v, _ := json.Marshal(d.Vendor)
	h.Write(v)
	for _, set := range [][]tree.Path{d.Roots, d.Preserve} {
		h.Write([]byte{0})
		for _, p := range set {
			fmt.Fprintln(h, p)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// PasswordValue returns the password, from the environment if set.
func (s Spec) PasswordValue() string {
	if s.PasswordEnv != "" {
		return os.Getenv(s.PasswordEnv)
	}
	return s.Password
}

func resolve(def, s Spec) Spec {
	str := func(v, d string) string {
		if v == "" {
			return d
		}
		return v
	}
	bl := func(v, d *bool) *bool {
		if v == nil {
			return d
		}
		return v
	}
	lst := func(v, d []string) []string {
		if v == nil {
			return d
		}
		return v
	}
	out := Spec{
		Address:     s.Address,
		Username:    str(s.Username, def.Username),
		Password:    str(s.Password, def.Password),
		PasswordEnv: str(s.PasswordEnv, def.PasswordEnv),
		Insecure:    bl(s.Insecure, def.Insecure),
		SkipVerify:  bl(s.SkipVerify, def.SkipVerify),
		CAFile:      str(s.CAFile, def.CAFile),
		Mode:        Mode(str(string(s.Mode), string(def.Mode))),
		Groups:      lst(s.Groups, def.Groups),
		Roots:       lst(s.Roots, def.Roots),
		Preserve:    lst(s.Preserve, def.Preserve),
		Ignore:      lst(s.Ignore, def.Ignore),
	}
	if out.Mode == "" {
		out.Mode = ModePR
	}
	return out
}

func parsePaths(ss []string) ([]tree.Path, error) {
	out := make([]tree.Path, 0, len(ss))
	for _, s := range ss {
		p, err := tree.ParsePath(s)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// isKeyLeaf reports whether p is a list entry's key leaf.
func isKeyLeaf(p tree.Path) bool {
	n := len(p)
	if n < 2 {
		return false
	}
	_, ok := p[n-2].Keys[p[n-1].Name]
	return ok
}

func underAny(p tree.Path, roots []tree.Path) bool {
	for _, r := range roots {
		if p.HasPrefix(r) {
			return true
		}
	}
	return false
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}
