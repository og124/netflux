package desired

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/og124/netflux/internal/tree"
)

func TestExampleLabRenders(t *testing.T) {
	devs, err := LoadAll(filepath.Join("..", "..", "examples", "lab"))
	if err != nil {
		t.Fatal(err)
	}
	l1 := devs["leaf1"]
	if l1.Spec.Mode != ModeStrict || devs["leaf2"].Spec.Mode != ModePR {
		t.Errorf("modes: %s %s", l1.Spec.Mode, devs["leaf2"].Spec.Mode)
	}
	// Group then device layering: domain from groups/all, hostname per device.
	for path, want := range map[string]string{
		"/system/config/domain-name": "lab.example.net",
		"/system/config/hostname":    "leaf1",
	} {
		if got := l1.Config[path].Value; got != want {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}
	// The network-instance's own key sits above every root and is dropped.
	if _, ok := l1.Config["/network-instances/network-instance[name=default]/name"]; ok {
		t.Error("ancestor key leaf kept")
	}
	if devs["leaf1"].Hash == devs["leaf2"].Hash {
		t.Error("different devices, same hash")
	}
}

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const inv = `
defaults: {roots: [/system/config], groups: [base]}
devices:
  sw1: {address: 127.0.0.1:1}
`

func TestValidation(t *testing.T) {
	cases := map[string]struct {
		files map[string]string
		err   string
	}{
		"outside roots": {map[string]string{
			"devices/sw1/x.yaml": "lldp: {config: {enabled: true}}",
		}, "outside every managed root"},
		"vendor without reason": {map[string]string{
			"devices/sw1/vendor.yaml": "- {origin: cli, value: 'ip routing'}",
		}, "no reason"},
		"vendor in openconfig": {map[string]string{
			"devices/sw1/vendor.yaml": "- {reason: x, origin: openconfig, value: {}}",
		}, "vendor origin"},
		"unknown device dir": {map[string]string{
			"devices/sw9/x.yaml": "system: {config: {hostname: x}}",
		}, "devices/sw9: not in inventory.yaml"},
		"keyless list entry": {map[string]string{
			"devices/sw1/x.yaml": "system: {config: {hostname: x, servers: [{config: {}}]}}",
		}, "no key leaves"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, InventoryFile, inv)
			write(t, dir, "groups/base/.keep", "")
			for rel, body := range c.files {
				write(t, dir, rel, body)
			}
			_, err := LoadAll(dir)
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("err = %v, want %q", err, c.err)
			}
		})
	}
}

func TestDriftFileLoadsLastAndAdopts(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, InventoryFile, `
defaults: {roots: [/interfaces]}
devices: {sw1: {address: 127.0.0.1:1}}
`)
	write(t, dir, "devices/sw1/interfaces.yaml", `
interfaces:
  interface:
    - name: Ethernet1
      config: {name: Ethernet1, description: from git, mtu: 9214}
    - name: Loopback1
      config: {name: Loopback1}
`)
	before, err := LoadAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	live := before["sw1"].Config.Clone()
	set := func(p string, v any) {
		pp := tree.MustParse(p)
		live[pp.Canon()] = tree.Leaf{Path: pp, Value: v}
	}
	set("/interfaces/interface[name=Ethernet1]/config/description", "by hand")
	set("/interfaces/interface[name=Loopback7]/name", "Loopback7")
	set("/interfaces/interface[name=Loopback7]/config/name", "Loopback7")
	delete(live, "/interfaces/interface[name=Ethernet1]/config/mtu")
	live.DeleteSubtree(tree.MustParse("/interfaces/interface[name=Loopback1]"))

	changes := tree.Diff(before["sw1"].Config, live)
	b, err := MergeDrift("sw1", nil, changes)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte("_delete: true")) {
		t.Errorf("entry removal not written as _delete:\n%s", b)
	}
	// "a-drift.yaml" would sort before interfaces.yaml; drift.yaml must
	// still load last.
	write(t, dir, "devices/sw1/drift.yaml", string(b))
	after, err := LoadAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	if d := tree.Diff(after["sw1"].Config, live); len(d) != 0 {
		t.Errorf("git + drift.yaml != live:\n%v\n%s", d, b)
	}

	// More drift later merges into the same file.
	live2 := live.Clone()
	set2 := tree.MustParse("/interfaces/interface[name=Ethernet1]/config/enabled")
	live2[set2.Canon()] = tree.Leaf{Path: set2, Value: false}
	b2, err := MergeDrift("sw1", b, tree.Diff(live, live2))
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, "devices/sw1/drift.yaml", string(b2))
	after2, _ := LoadAll(dir)
	if d := tree.Diff(after2["sw1"].Config, live2); len(d) != 0 {
		t.Errorf("second drift lost something: %v\n%s", d, b2)
	}
}

func TestEnvExpansion(t *testing.T) {
	t.Setenv("SW1_ADDR", "10.1.1.1:6030")
	if got := expandEnv("a: ${SW1_ADDR:-x}, b: ${UNSET_VAR_NETFLUX:-127.0.0.1:6030}"); got != "a: 10.1.1.1:6030, b: 127.0.0.1:6030" {
		t.Error(got)
	}
}
