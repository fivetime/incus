(storage-cephext)=
# Ceph external RBD - `cephext`

<!-- Include start Ceph external intro -->
The `cephext` driver is a variant of the {ref}`storage-ceph` driver for RBD
images that are created and owned by an external system, for example OpenStack
Cinder volumes.

A `cephext` storage pool points at an existing OSD pool (through `source`) and
never creates, deletes or resizes any RBD image in it. Every Incus volume in
such a pool maps to a pre-existing RBD image through the `ceph.rbd.image_name`
volume configuration key:

- Creating a volume *claims* the matching image in place: Incus verifies that
  the image exists, then maps and mounts it as usual. No image is created, no
  file system is created and no content is written.
- Deleting a volume merely *releases* the image: it is unmapped and the local
  records are removed, the image itself is left untouched.

The most common use is adopting an externally provisioned volume as an
instance root disk, by setting `initial.ceph.rbd.image_name` on the root disk
device at instance creation time:

    incus create my-instance --empty --storage cinder \
      --device root,initial.ceph.rbd.image_name=volume-8231d2e8-e306-40e4-8f42-a9d2475f2e05

Because the image is owned by the external system, all operations that would
alter it or its snapshots behind the owner's back are refused: Incus-side
snapshots, copies, backups, refreshes, renames and size changes are not
supported. Size and snapshot management stay entirely with the external owner.

Shared storage migration handover is supported: when two standalone servers
see the same Ceph cluster and OSD pool, instances backed by claimed volumes
migrate between them without any data transfer.

`cephext` pools support `container` and `custom` volumes. Images, VMs and
buckets are not supported.
<!-- Include end Ceph external intro -->

## Requirements

The requirements are the same as for the {ref}`storage-ceph` driver. In
addition, the OSD pool named in `source` must already exist and the RBD images
referenced by `ceph.rbd.image_name` must contain a file system matching the
volume's `block.filesystem` (default `ext4`) with the expected volume layout
(for container root volumes: a `rootfs` directory at the top level).

## Configuration options

The pool level configuration options are the same as for the
{ref}`storage-ceph` driver, except that `source` is required and must name an
existing OSD pool.

In addition to the {ref}`storage-ceph` volume options, the following volume
configuration key is available:

Key                   | Type   | Default | Description
:--                   | :---   | :------ | :----------
`ceph.rbd.image_name` | string | -       | Name of the externally managed RBD image backing the volume
