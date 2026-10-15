# Runtime upstream maintenance

The authoritative fork branches (`main` for Incus, LXC, LXCFS and pylxd,
`criu-dev` for CRIU) are the source integration baselines. Production images
use immutable component commits and image digests; a repository merge alone
does not upgrade a deployed runtime.

For each update:

1. Fetch the relevant upstream branch and review changes since both the fork
   baseline and the component actually pinned in `Dockerfile`.
2. Preserve necessary fork fixes, including the separately pinned LXC restored
   init pidfd patch, until an equivalent upstream implementation is verified.
3. Build both architectures and verify component identities and patch markers
   in the image. Use the exact tested platform manifest for deployment.
4. Validate three dedicated test nodes: create/start/exec, volume persistence,
   cold/live migration, failed restore continuity and retry, and cleanup.
5. Render the production change against a fresh Helm and live DaemonSet backup.
   Deploy one node at a time with compute scheduling disabled. Check original
   guest PID/start time, ID maps, LXCFS identities, and compute readiness.
6. Run production migration acceptance, remove temporary resources, and publish
   the artifact digest and validation evidence. Keep unreviewed development
   branch changes out of a validated release.

The 2026-10-01 LXC update integrates upstream `35c69e4d` and retains patch
`8eed33fe` (SHA256 `6118975e098c7df94c162a498c453f41c3cbabf21297a257989fa55fff2ee50d`).
LXCFS and pylxd were current against their upstream branches at the audit.
The Incus OVN interconnect fix and CRIU development backlog were left for
separate review in that LXC runtime release.

The 2026-10-15 LXC update moves to upstream `61d8ba0a9`, which adds three
fixes on top of `35c69e4d`: `exec_criu` initializes its `controllers` pointer
before the scope-exit free (`5707d8dd6`), fstab and mount entries share one
idmapped mount sequence counter (`ddb556c3a`), and a macro spelling fix. Patch
`8eed33fe` still applies unchanged and is retained; upstream review is
lxc/lxc#4734. The pinned LXC already provides `environment_runtime_hooks`, so
the Incus upstream merge of the same date activates split runtime and hook
environments; validate both together.

The subsequent CRIU release on 2026-10-01 integrates official `criu-dev`
`4485a86da` and retains the three container restoration fixes plus a buffer-size
argument adaptation at `df1e64a6b`. Incus image revision `3a2c173e99` also includes
the reviewed OVN fix. The exact deployed image, platform manifest and acceptance
results are recorded in `validation-criu-upstream-20261001.json`.

`criu-dev` is now the only local and origin CRIU branch. The old repair branch,
temporary review branches and unchanged stable mirror were retired after
production acceptance. Their historical commits were preserved in the local
release evidence bundle `criu-before-consolidation.bundle`. Future updates start
from `criu-dev`; images continue to pin verified commits rather than floating
branch heads.
