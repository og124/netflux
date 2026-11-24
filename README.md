# netflux

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

leaf2 is in `pr` mode. With `-dir` there is no git remote to propose to, so it
reports the drift and leaves the device alone. Run it against a git remote
(below) to get pull requests.

## Against the cEOS lab

`examples/lab` targets the two leaves from
[ceos-gnmi-lab](https://github.com/og124/ceos-gnmi-lab) (172.20.20.11/.12,
gNMI on 6030). That lab only puts addresses on Ethernet1. netflux brings up
the rest: loopbacks, eBGP between the leaves, loopback advertisement through
routing policy, VLANs, NTP, DNS, LLDP and the banner.

```console
$ export NETFLUX_PASSWORD=admin
$ netflux diff -dir examples/lab leaf1             # what an apply would change
$ netflux import -dir examples/lab -o - leaf1      # the device's own view, as desired-state YAML
$ netflux apply -dir examples/lab leaf1            # once, and report anything that didn't take
$ netflux run -config netflux.yaml                 # then hand it over to the controller
```

`netflux import` is the safe way to bring an existing box under management.
It writes what the device already has under the managed roots, in the
device's own rendering, so the first apply is a no-op.

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

## Running the controller

`netflux run -config netflux.yaml` (see
[`netflux.example.yaml`](netflux.example.yaml)) keeps its own checkout of the
desired-state repo, fetches it every `poll`, and reconciles every device in
parallel. 

| endpoint | |
|---|---|
| `/` | status page: per device, sync state, drift, open PR, recent events |
| `/status` | the same, as JSON |
| `/metrics` | Prometheus: `netflux_drift_leaves`, `netflux_heals_total`, `netflux_streaming`, `netflux_unconverged_leaves`, … |
| `POST /sync` | fetch git now (point a push webhook here) |

The image (`make image`) runs `netflux run -config /etc/netflux/netflux.yaml`.

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
