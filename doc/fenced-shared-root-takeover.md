# Fenced shared-root takeover implementation record

This is an implementation record, not an enabled recovery API. Shared Ceph
evacuation must continue to fail closed until the Nova coordinator, durable
Incus transaction and complete system acceptance are implemented.

## Storage ownership primitive (2026-10-03)

`internal/server/storage/cephownership` implements an atomic replacement of
`incus.openstack.materialization_ownership` on a format-2 RBD header. The binding
includes the Ceph cluster FSID, pool ID and RBD image ID. It never resolves an
image name for the write, creates a header, formats a filesystem or copies data.
The caller must already hold the externally coordinated fenced takeover grant.
This primitive does not itself prove fencing or grant Nova ownership.

The adapter loads the librados C ABI at runtime. Unsupported systems or a missing
library fail closed. An existing header, the exact previous marker and the update
are checked in one OSD write operation. A lost-response replay also accepts the
same target marker; a different owner remains a conflict. The Ceph FSID is checked
on the connected cluster, and the pool is opened by ID.

The implementation relies on format-2 RBD user metadata being stored as raw OMAP
values under `metadata_`, as defined by Ceph's `cls_rbd` metadata protocol. This
is a deliberately narrow compatibility dependency, not a general RBD header
editor. Only Incus's ownership key can be written. Before enabling the recovery
API on a new Ceph release, run the native compatibility test against that release.

Protocol references:

- https://docs.ceph.com/en/latest/rados/api/librados/
- https://github.com/ceph/ceph/blob/squid/src/cls/rbd/cls_rbd.cc

The storage driver wrapper takes the existing background-flatten lock before the
volume mount lock, matching identity-bound deletion. It checks the name-to-ID
binding before and after the transfer. If a name changes during the operation,
the original image can have the target marker while the replacement remains
untouched; the caller must query/retry the durable transaction, not roll back by
overwriting the marker or deleting either image.

## Native evidence

On 2026-10-03, `TestNativeOwnershipCAS` passed on all dedicated compute testbeds:

| Compute | Temporary original image ID | Result |
| --- | --- | --- |
| 10.32.32.131 | `808e86d81a5dd4` | PASS |
| 10.32.32.132 | `80a6e5100bacf1` | PASS |
| 10.32.32.136 | `673c9566d12ec5` | PASS |

Each test created uniquely named 8 MiB images in the explicitly selected
`incus-rootfs-rbd-pool`. It exercised competing marker transfers (exactly one
winner), lost-response replay, a wrong FSID, a missing image, and replacement of
the original image name. The replacement had a different marker and remained
unchanged. Cleanup removed only the test-created names, including the renamed
original. These tests did not change any existing instance or restart a daemon.
They validate the storage primitive; they do not constitute evacuation acceptance.

The native hosts used Ceph 19.2.3. The current Incus daemon image contains Ceph
19.2.4 and `librados.so.2`, but this new adapter has not yet been built and run
inside that musl image. Image compatibility remains a packaging acceptance item.

The complete `go test -v ./...` run passed on node03 after correcting Windows
archive line endings in the isolated validation checkout. The changed storage
packages passed `golangci-lint` with zero issues and race-enabled tests. A driver
test additionally verifies that takeover waits for the existing flatten lock and
rejects a same-name replacement before writing ownership. The full
`make static-analysis` target is not green: it stops on existing codespell
findings in unmodified files such as `instance_post.go`, OVN/firewall sources and
existing test scripts. These findings are not counted as passed checks.

The test is skipped unless `INCUS_CEPH_OWNERSHIP_TEST_POOL`,
`INCUS_CEPH_OWNERSHIP_TEST_CLUSTER` and `INCUS_CEPH_OWNERSHIP_TEST_USER` identify
the intended test backend. Run with `go test -v -race` on Linux with cgo and Ceph
credentials. Never configure these variables to an unreviewed production backend.

## Remaining integration and acceptance

The following are required before exposing or enabling shared-root evacuation:

1. Persist the source manifest before failure, including configuration, ID mapping,
   immutable storage identity, source materialization marker and owner generation.
2. Atomically bind an etcd takeover grant to one instance, source generation,
   independently verified fence retirement, original image and one target token.
   Two targets must not receive valid grants; a watcher check is auxiliary only.
3. Persist Incus's prepare/claim/import/commit/start/completion journal before
   each side effect. Retrying a token must require the identical binding.
4. Import stopped local instance and volume records using the existing original
   storage, with deletion protection installed first. Rebuild current network and
   Cinder device configuration through Nova; do not import stale attachments.
5. Commit durable target ownership before mounting or starting. Returned sources
   must discard only stale local records, mounts and networking, retaining the RBD.
6. Validate poweroff evacuation, cross-node target competition, SIGKILL and lost
   responses at every phase, source return, concurrent flatten, Cinder data volumes
   and final deletion on all three testbeds. Assert unchanged image ID, preserved
   data and exactly one running instance/writer. Failed target replacement remains
   within the previously accepted manual recovery boundary.

The planned Nova policy switch must remain separate from BFV evacuation and default to false.
Local root reconstruction retains separate semantics: original-image rebuild,
not original-root takeover. CRIU is outside this protocol.
