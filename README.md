# netflux

GitOps reconciliation for network switches.

The opinion: run a network the way you run a Kubernetes cluster. Git holds
the desired state. A controller keeps making the live network match it. A
change made by hand on a box is drift, and it gets corrected.

- **Replace, not merge.** netflux applies each device's config with gNMI
  `Set` in replace mode. The device ends up exactly as declared, rather than
  as git layered on top of whatever was already there.
- **Drift is watched, not polled for.** A gNMI `ON_CHANGE` subscription
  reports hand changes as they happen. A device in `strict` mode is healed on
  the spot. One in `pr` mode gets a pull request that writes the change back
  into git: merge it to adopt the change, close it to revert the device.
- **OpenConfig first.** Desired state is vendor-neutral OpenConfig. Vendor
  models are a fallback, kept in a separate file, and each entry has to say
  why OpenConfig couldn't do the job.
- **The whole switch, not just a feature.** Interfaces, BGP, VLANs, routing
  policy, system, NTP, DNS, LLDP and ACLs all work the same way. You declare
  which subtrees netflux owns and it manages any OpenConfig under them. No
  per-feature code, and no compiled YANG.

```
                 git (OpenConfig YAML)
                          │ poll / POST /sync
                          ▼
   inventory + groups/ + devices/  ──render──▶  desired tree per device
                                                       │
                       ┌───────────── plan ────────────┘
                       ▼
     gNMI Set: replace every managed root, one transaction
                       │
                       ▼
     read back ──▶ baseline snapshot
                       ▲
                       │ compare
     gNMI Subscribe ON_CHANGE ──▶ live config ──▶ drift?
                                                    ├─ strict:  re-apply now
                                                    ├─ pr:      branch + pull request
                                                    └─ observe: report only
```

## Try it on a laptop

No switch needed. `netflux sim` runs in-memory gNMI devices that behave
enough like switches to drive the whole loop. That includes a switch's habit
of filling in config you never set.

```console
$ make demo        # two simulated leaves + the controller, reconciling examples/lab
```

Open http://127.0.0.1:8080, then play the engineer who "just fixes it on the
box". leaf1 is `strict`:

```console
$ gnmic -a 127.0.0.1:57401 --insecure -u admin -p admin -e json_ietf \
    set --update-path '/interfaces/interface[name=Ethernet1]/config/description' \
        --update-value '"someone was here"'
```

Within a couple of seconds netflux logs the drift and puts it back:

```
level=INFO msg=drift device=leaf1 change="~ /interfaces/interface[name=Ethernet1]/config/description: \"leaf2:Ethernet1\" -> \"someone was here\""
level=INFO msg="drift: 1 leaf changed on the device; strict mode, reverting" device=leaf1
level=INFO msg="applied dir:96ddce6bb0c0 (heal): 1 leaf changed on the device" device=leaf1
```

The same happens for *additions* under a managed root, like a rogue
`Loopback66`, and for deletions, like a BGP neighbour removed by hand.
Replace means "exactly this", so all three are drift.

leaf2 is in `pr` mode. With `-dir` there is no git remote to propose to, so it
reports the drift and leaves the device alone. Run it against a git remote
(below) to get pull requests.

## Against the cEOS lab

`examples/lab` targets the two leaves from
[ceos-gnmi-lab](https://github.com/og124/ceos-gnmi-lab) (172.20.20.11/.12,
gNMI on 6030). That lab only puts addresses on Ethernet1. netflux brings up
the rest: loopbacks, eBGP between the leaves, loopback advertisement through
routing policy, VLANs, NTP, DNS, LLDP and the banner.

Go in carefully. The first apply replaces whole subtrees:

```console
$ export NETFLUX_PASSWORD=admin
$ netflux diff -dir examples/lab leaf1             # what an apply would change
$ netflux import -dir examples/lab -o - leaf1      # the device's own view, as desired-state YAML
$ netflux apply -dir examples/lab leaf1            # once, and report anything that didn't take
$ netflux run -config netflux.yaml                 # then hand it over to the controller
```

`netflux import` is the safe way to bring an existing box under management.
It writes what the device already has under the managed roots, in the
device's own rendering, so the first apply is a no-op. After that you edit
from a known-good base.

`mode: observe` is the other safe on-ramp. netflux reads and reports what it
would change, and never writes.

## Desired state

```
examples/lab/
  inventory.yaml            devices, how to reach them, what netflux owns on them
  groups/all/*.yaml         layered onto every device that lists the group
  groups/leaves/*.yaml
  groups/leaves/vendor.yaml non-OpenConfig fallbacks, each with its reason
  devices/leaf1/*.yaml      per-device config, layered last
  devices/leaf1/drift.yaml  written by netflux when drift is proposed; loads last
```

Every YAML file is an OpenConfig tree rooted at `/`, in RFC 7951 (JSON_IETF)
shape. That's the same shape `gnmic get -e json_ietf` prints, so you can paste
from a device into git and back. A device's layers apply in this order: its
groups, in the order it lists them, then its own files alphabetically, with
`drift.yaml` always last. A later layer overrides an earlier one leaf by leaf.
`null` deletes a subtree, and `_delete: true` deletes a list entry:

```yaml
interfaces:
  interface:
    - name: Loopback9
      _delete: true          # inherited from a group, not wanted here
    - name: Ethernet1
      config:
        mtu: null            # back to the device default
```

### What netflux owns: roots, preserve, ignore

```yaml
roots:                       # replaced wholesale on every apply
  - /interfaces
  - /network-instances/network-instance[name=default]/protocols/protocol[identifier=BGP][name=BGP]
preserve:                    # under a root, but the device's: copied from live into every replace
  - /interfaces/interface[name=Management*]
ignore: []                   # left out of drift detection
```

- **Roots** are the contract. Anything under a root that isn't in git is
  removed from the device. Anything outside every root is never touched. A
  root git says nothing about is cleared, or deleted if it's a list entry.
  Config in git outside every root fails validation, so a typo can't
  silently do nothing.
- **Preserve** is how netflux avoids cutting itself off. Replacing
  `/interfaces` without the management interface would remove its address.
  So netflux reads the preserved subtrees from the live device and puts them
  back into every replace. Patterns take `*` for an element and globs in key
  values.
- **Ignore** is for leaves a device changes by itself.

### Modes

| mode | on drift | while a PR is open |
|---|---|---|
| `strict` | re-applies git at once | — |
| `pr` (default) | writes the change to `devices/<name>/drift.yaml` on branch `netflux/drift/<name>` and opens a PR | device left as is. **Merge** adopts the change. **Close** reverts the device. If the change is undone on the device, netflux closes the PR itself |
| `observe` | reports only, and never writes | — |

### Vendor fallback

```yaml
# groups/leaves/vendor.yaml
- reason: >
    EOS's anycast gateway (VARP, `ip virtual-router mac-address`) has no
    OpenConfig model. OpenConfig only covers VRRP, which is a different protocol.
  origin: cli
  value: ip virtual-router mac-address 00:1c:73:00:00:99
```

Each entry is a raw gNMI operation in a non-OpenConfig origin, sent in a
second `Set` after the OpenConfig one lands. A missing `reason` fails
validation. These entries are applied but not drift-checked, because netflux
only reads OpenConfig back. That gap is one more reason to keep the list
short.

## Running the controller

`netflux run -config netflux.yaml` (see
[`netflux.example.yaml`](netflux.example.yaml)) keeps its own checkout of the
desired-state repo, fetches it every `poll`, and reconciles every device in
parallel. A commit that fails validation is never applied: netflux logs it,
flags it on `/metrics`, and keeps enforcing the last good revision.

| endpoint | |
|---|---|
| `/` | status page: per device, sync state, drift, open PR, recent events |
| `/status` | the same, as JSON |
| `/metrics` | Prometheus: `netflux_drift_leaves`, `netflux_heals_total`, `netflux_streaming`, `netflux_unconverged_leaves`, … |
| `POST /sync` | fetch git now (point a push webhook here) |

The image (`make image`) runs `netflux run -config /etc/netflux/netflux.yaml`.

## How it decides what drift is

The obvious design is to compare the device with git. It doesn't work,
because no device echoes back exactly what it was sent. It fills in defaults
(`mtu: 1500`, `enabled: true`), qualifies identities
(`BGP` → `openconfig-policy-types:BGP`), and turns numbers into strings. A
device that matches git perfectly would look drifted forever.

So netflux reads the device back right after every apply, and that snapshot
is the baseline. Drift is any change against the snapshot, which means only
changes made since netflux last applied count. Separately, any leaf git sets
that the device doesn't hold after an apply is reported as **unconverged**.
That's config the device silently rejected, or normalised past recognition.

Two consequences, handled on purpose:

- If the read-back doesn't match git, netflux applies once more before
  taking the snapshot. Normalisation survives a second apply, and a hand
  edit racing the first apply doesn't. Without this, an edit made in that
  moment would become the baseline.
- A hand *addition* in that same moment is indistinguishable from a default
  the device filled in. A strict device is therefore re-applied every 10
  minutes even when there's no drift, which bounds how long such an addition
  can survive. Flux and Argo reconcile on an interval for the same reason.

netflux reads OpenConfig without compiling YANG. A list entry is an array of
objects whose scalar members are its keys. OpenConfig guarantees that shape,
because every list entry repeats its keys beside its `config` container.
Comparisons ignore YANG module prefixes. They also treat `"1500"` and `1500`
as equal, since JSON_IETF carries 64-bit integers as strings.

## Status

Verified: `go test -race ./...` exercises the full loop over real gRPC
against simulated devices, a real git remote and a fake GitHub API:

- apply with the management interface preserved
- strict heal of changed, added and deleted config
- a pull request opened for drift; closing it reverts the device
- merging the drift branch adopts the change

The `make demo` walkthrough above was run as written.

**Not yet verified against real cEOS.** cEOS is x86_64-only, and the lab was
parked while this was written. Before trusting the example on real
switches, check these EOS behaviours with `netflux diff`, `import` and
`apply`:

- whether replacing `/interfaces` leaves Ethernet1 a routed port
- whether EOS accepts an empty replace of a root
- which leaves EOS reports as unconverged

## Code

| | |
|---|---|
| `internal/tree` | OpenConfig as a flat map of leaf paths: parse, flatten, unflatten, merge, diff |
| `internal/desired` | inventory, layering and validation; `drift.yaml` writing |
| `internal/device` | gNMI client: Get, Set, Subscribe |
| `internal/controller` | per-device reconcile loop, drift handling, saved state |
| `internal/source` | git checkout and drift branches; plain directory for development |
| `internal/github` | pull requests for drift |
| `internal/sim` | the simulated device |
| `internal/server` | status page, JSON, metrics |
