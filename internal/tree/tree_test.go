package tree

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParsePathRoundTrip(t *testing.T) {
	for _, s := range []string{
		"/",
		"/interfaces/interface[name=Ethernet1]/config/description",
		"/network-instances/network-instance[name=default]/protocols/protocol[identifier=BGP][name=BGP]",
		`/a/b[k=x\]y]/c`,
		"/acl/acl-sets/acl-set[name=IN][type=ACL_IPV4]",
	} {
		p, err := ParsePath(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if got := p.String(); got != s {
			t.Errorf("round trip %q -> %q", s, got)
		}
	}
	if _, err := ParsePath("/a/b[k=v"); err == nil {
		t.Error("unterminated key parsed")
	}
}

func TestCanonStripsPrefixes(t *testing.T) {
	a := MustParse("/openconfig-network-instance:network-instances/network-instance[name=default]/protocols/protocol[identifier=openconfig-policy-types:BGP][name=BGP]")
	b := MustParse("/network-instances/network-instance[name=default]/protocols/protocol[name=BGP][identifier=BGP]")
	if a.Canon() != b.Canon() {
		t.Errorf("%s != %s", a.Canon(), b.Canon())
	}
	// IPv6 and free text keep their colons.
	if got := stripIdentity("2001:db8::1"); got != "2001:db8::1" {
		t.Errorf("ipv6 mangled: %s", got)
	}
	if got := stripIdentity("leaf2:Ethernet1"); got != "leaf2:Ethernet1" {
		t.Errorf("free text mangled: %s", got)
	}
}

func TestValuesEqual(t *testing.T) {
	cases := []struct {
		a, b any
		eq   bool
	}{
		{int64(1500), json.Number("1500"), true},
		{"1500", int64(1500), true},
		{"IPV4_UNICAST", "openconfig-bgp-types:IPV4_UNICAST", true},
		{"spine-1:Ethernet3", "spine-2:Ethernet3", false},
		{true, "true", true},
		{[]any{"b", "a"}, []any{"a", "b"}, true},
		{[]any{"a"}, "a", false},
	}
	for _, c := range cases {
		if got := ValuesEqual(c.a, c.b); got != c.eq {
			t.Errorf("ValuesEqual(%v, %v) = %v", c.a, c.b, got)
		}
	}
}

const ifJSON = `{
  "interface": [
    {"name": "Ethernet1",
     "config": {"name": "Ethernet1", "description": "to leaf2", "mtu": 9214},
     "subinterfaces": {"subinterface": [
       {"index": 0, "ipv4": {"addresses": {"address": [
         {"ip": "10.10.10.1", "config": {"ip": "10.10.10.1", "prefix-length": 30}}]}}}]}},
    {"name": "Loopback0", "config": {"name": "Loopback0"}}
  ]
}`

func TestFlattenUnflattenRoundTrip(t *testing.T) {
	v, err := DecodeJSON([]byte(ifJSON))
	if err != nil {
		t.Fatal(err)
	}
	root := MustParse("/interfaces")
	l, err := Flatten(root, v)
	if err != nil {
		t.Fatal(err)
	}
	want := "/interfaces/interface[name=Ethernet1]/subinterfaces/subinterface[index=0]/ipv4/addresses/address[ip=10.10.10.1]/config/prefix-length"
	if _, ok := l.Set[want]; !ok {
		t.Fatalf("missing %s in %v", want, l.Set.Keys())
	}
	if _, ok := l.Set["/interfaces/interface[name=Loopback0]/name"]; !ok {
		t.Error("key leaf not kept")
	}

	back := Unflatten(l.Set, root)
	b, _ := json.Marshal(back)
	l2, err := Flatten(root, mustDecode(t, b))
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(l.Set, l2.Set); len(d) != 0 {
		t.Errorf("round trip changed: %v\n%s", d, b)
	}
	// Keys come first, then config.
	if !strings.HasPrefix(string(b), `{"interface":[{"name":"Ethernet1","config":`) {
		t.Errorf("member order: %s", b)
	}
}

func TestUnflattenListEntryRoot(t *testing.T) {
	root := MustParse("/network-instances/network-instance[name=default]/protocols/protocol[identifier=BGP][name=BGP]")
	tr := Tree{}
	p := root.Child(Elem{Name: "bgp"}).Child(Elem{Name: "global"}).Child(Elem{Name: "config"}).Child(Elem{Name: "as"})
	tr[p.Canon()] = Leaf{Path: p, Value: int64(65001)}
	b, _ := json.Marshal(Unflatten(tr, root))
	want := `{"identifier":"BGP","name":"BGP","bgp":{"global":{"config":{"as":65001}}}}`
	if string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}
}

func TestLayerDeletes(t *testing.T) {
	base, _ := Flatten(Path{}, mustDecode(t, []byte(`{"interfaces": {"interface": [
		{"name": "Loopback9", "config": {"name": "Loopback9", "description": "x"}},
		{"name": "Ethernet1", "config": {"name": "Ethernet1", "description": "keep", "mtu": 9214}}]}}`)))
	over, err := Flatten(Path{}, mustDecode(t, []byte(`{"interfaces": {"interface": [
		{"name": "Loopback9", "_delete": true},
		{"name": "Ethernet1", "config": {"mtu": null}}]}}`)))
	if err != nil {
		t.Fatal(err)
	}
	tr := Tree{}
	tr.Apply(base)
	tr.Apply(over)
	for _, k := range tr.Keys() {
		if strings.Contains(k, "Loopback9") || strings.HasSuffix(k, "/mtu") {
			t.Errorf("not deleted: %s", k)
		}
	}
	if _, ok := tr["/interfaces/interface[name=Ethernet1]/config/description"]; !ok {
		t.Error("sibling deleted")
	}
}

func TestMatch(t *testing.T) {
	p := MustParse("/interfaces/interface[name=Management0]/config/description")
	for pat, want := range map[string]bool{
		"/interfaces/interface[name=Management*]": true,
		"/interfaces/interface":                   true,
		"/interfaces/interface[name=Ethernet*]":   false,
		"/interfaces/*/config/description":        true,
		"/system":                                 false,
	} {
		if got := p.Match(MustParse(pat)); got != want {
			t.Errorf("%s matches %s = %v", pat, p, got)
		}
	}
}

func TestDiff(t *testing.T) {
	a, _ := Flatten(Path{}, mustDecode(t, []byte(`{"system":{"config":{"hostname":"leaf1","domain-name":"lab"}}}`)))
	b, _ := Flatten(Path{}, mustDecode(t, []byte(`{"system":{"config":{"hostname":"hacked","motd-banner":"hi"}}}`)))
	d := Diff(a.Set, b.Set)
	var kinds []string
	for _, c := range d {
		kinds = append(kinds, string(c.Kind)+" "+c.Path.String())
	}
	want := []string{
		"removed /system/config/domain-name",
		"changed /system/config/hostname",
		"added /system/config/motd-banner",
	}
	if strings.Join(kinds, "|") != strings.Join(want, "|") {
		t.Errorf("got %v", kinds)
	}
}

func mustDecode(t *testing.T, b []byte) any {
	t.Helper()
	v, err := DecodeJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
