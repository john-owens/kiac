# k0s as a third distro (`--distro k0s`)

Status: investigated, **not implemented**. This doc records why k0s was
considered, what was measured, what an implementation would take, and
the conditions under which it becomes worth building. Revisit when one
of the triggers at the end applies.

Question asked: could kiac offer `--distro k0s` alongside `kubeadm` and
`k3s` for "very stripped-down" clusters?

Short answer: feasible, roughly 1,800-2,000 lines including tests, but
**it would not make kiac clusters smaller**. Measured like-for-like, k0s
idles slightly *heavier* than kiac's already-trimmed k3s nodes, has no
Kubernetes 1.37 release yet, and brings one genuinely new problem
(worker re-join after node IP changes). Its real advantages are
elsewhere: unmodified upstream components, FIPS builds, and parity with
fleets that already run k0s.

## Findings (measured 2026-10-07)

Both distros ran single-node, privileged, in Docker on a Linux amd64
host, at the same Kubernetes version (1.36.4: `rancher/k3s:v1.36.4-k3s1`,
`k0sproject/k0s:v1.36.4-k0s.1`). Memory is the container cgroup's usage
reported by `docker stats`, sampled three times after every system pod
had been Running for 150s. Absolute numbers on an Apple silicon node VM
will differ; the *relative* comparison is the point.

| Configuration | Idle memory | Running system pods |
|---|---|---|
| k3s, stock defaults | ~698 MiB | coredns, traefik, svclb, local-path, metrics-server |
| k0s, stock defaults (`controller --enable-worker`) | ~646 MiB | coredns, konnectivity-agent, kube-proxy, kube-router, metrics-server |
| **k3s as kiac runs it** (`--disable=traefik --disable=servicelb --disable-network-policy`) | **~533 MiB** | coredns, local-path, metrics-server |
| **k0s at its leanest** (`controller --single`: kine/sqlite, no konnectivity) | **~556 MiB** | coredns, kube-proxy, kube-router, metrics-server |

Takeaways:

- k0s only looks lighter than *stock* k3s, which ships Traefik and its
  service load balancer. kiac already disables both, and its k3s nodes
  still include a storage provisioner that k0s does not ship.
- A third-party comparison reports k0s ~458 MB vs k3s ~627 MB idle
  (computingforgeeks, Oct 2026). That compares against full default
  k3s, so it does not reflect kiac's configuration.
- Single-node readiness was comparable once images were local (k0s
  `--single` Ready ~30s after start; k3s faster). Neither difference is
  material against VM boot time.
- Gotcha hit while measuring: the k0s image's entrypoint is already
  `tini -- /entrypoint.sh`, so passing `-- k0s controller` as the
  command makes it try to execute `--` and exit 127.

## k0s facts relevant to kiac

Sources: <https://docs.k0sproject.io/stable/k0s-in-docker/>,
<https://github.com/k0sproject/k0s/releases>,
<https://docs.k0sproject.io/stable/configuration/#specnetwork>,
<https://docs.k0sproject.io/stable/runtime/>.

- **Image**: `docker.io/k0sproject/k0s`, Alpine-based, multi-arch
  including arm64; entrypoint `tini -- /entrypoint.sh` (kind-derived DNS
  and nested cgroup v2 fix-ups), default command
  `k0s controller --enable-worker`, `KUBECONFIG` preset to
  `/var/lib/k0s/pki/admin.conf`. Tags use `-` for `+`
  (`v1.36.4-k0s.1`).
- **Release cadence**: maintained 1.33-1.36; **1.37 is alpha only** as
  of this writing. kiac pins 1.32-1.37 and defaults to 1.37, so k0s
  would need its own, narrower version table.
- **Topology**: `--single` (kine/sqlite, no Konnectivity, cannot add
  workers) vs `controller --enable-worker --no-taints` (expandable).
  Workers join with `k0s token create --role=worker` then
  `k0s worker <token>`.
- **Datastore**: etcd by default; kine/sqlite is opt-in
  (`spec.storage.type: kine`) and limited to one controller.
- **CNI**: kube-router by default; `provider: custom` ships none, so
  kiac's kindnet plus the upstream CNI plugin binaries (as on k3s) would
  slot in.
- **Bundled addons**: CoreDNS, kube-proxy, kube-router, metrics-server.
  **No default StorageClass**: kiac would have to install local-path
  itself, which k3s never needed.
- **containerd**: bundled, socket `/run/k0s/containerd.sock`, drop-ins
  under `/etc/k0s/containerd.d/`, registry mirrors via `hosts.toml`;
  `k0s ctr` passthrough. The `--registry-cache` and `--ca-cert` designs
  carry over: hosts.toml as on kubeadm, CA bundle as on k3s (Alpine has
  no `update-ca-certificates` in the image path kiac would use).

## What an implementation would take

The k3s path is the right template: single binary, no systemd in the
image, supervised by a boot preamble, token-based worker join. Estimate
from a repo-wide map (2026-10-07): ~1,350 lines of Go, ~1,800-2,000 with
test parity, ~2,400 if GPU workers were included.

New files, mirroring their k3s counterparts:

- `pkg/cluster/k0s.go`: controller and worker argv, boot preamble,
  token creation, readiness waits, `k0s kubectl` wrapper (~550).
- `pkg/cluster/k0s_resume.go`: resume and heal (~400).
- `pkg/cluster/k0s_supervisor.go`, `k0s_assets.go`: supervisor, kindnet
  and a local-path manifest (~80 plus YAML).

One-line-to-small branches in: `cmd/create.go` (dispatch, flags),
`versions.go` (pinned image table), `status.go` (`distroFromNodes`),
`configfile.go`, `persist.go`, `cluster.go` (kubeconfig path),
`verify.go`, `support.go`, `lb.go` and `edgeproxy.go` (kubeconfig path
parameterised), `chaos.go`, `regcache.go`, `trust.go`, `webui.go`, docs.

Already distro-agnostic, no work: kubeconfig merge, the kiac-lb script,
the edge-proxy binary, verify check bodies, `kiac load image` (same
`k8s.io` containerd namespace), the registry cache container, runtime
layer.

### The hard part: workers after an IP change

k3s workers find the server through the `K3S_URL` environment variable,
which `kiac resume` rewrites when the control plane's address changes.
A k0s join token *embeds* the server URL. Resume would have to mint a
fresh worker token on the controller, write it into each worker, and
restart the worker process, or find a supported server-address
override. This is the only part with no existing kiac pattern to copy.

### Gates before writing code

Boot the k0s image in one apple/container VM and confirm:

1. cgroup v2 delegation and `/dev/kmsg` work under vminitd (the image's
   entrypoint was written for Docker).
2. `k0s kubectl` and `k0s ctr` behave as multicall binaries under
   vminitd (k3s needed argv0 handling for `kubectl`).
3. metrics-server is present at the pinned version.
4. Token regeneration on resume is fast enough not to stretch
   `kiac resume`.

### Suggested MVP scope if built

- Support: one controller plus workers on apple/container nodes,
  `controller --enable-worker --no-taints` with kine/sqlite, `custom`
  CNI with kindnet, explicit local-path storage, kiac-lb, edge proxy,
  resume via token regeneration, `--ca-cert`, `--registry-cache`.
- Reject at validation: `--gpu-workers`, multiple controllers,
  Kubernetes 1.37 until k0s ships it GA, `--cni` other than the default.

## Recommendation

Do not build it for footprint. For the smallest cluster kiac makes
today:

```bash
kiac create cluster --distro k3s --no-metrics --no-storage --no-lb --no-edge-proxy --cp-memory 2G
```

Build `--distro k0s` when one of these triggers holds:

- Users need to test against the same distribution they run in
  production (k0s fleets, k0sctl, Mirantis).
- Unmodified upstream binaries or k0s FIPS builds are a requirement.
- k0s ships Kubernetes 1.37 (or kiac's default moves to a version k0s
  supports GA), removing the version-matrix mismatch.

Separately: kiac's default distro is kubeadm. Making k3s the default
would cut idle memory for every new cluster, but it changes behaviour
for existing users, so it belongs in a discussion with upstream rather
than a fork-only change.
