# Handling the ZFS userspace/kernel version coupling

## The problem

The `zpool-discovery` agent shells out to `zpool`/`zfs`. Those CLIs talk to the
host's ZFS **kernel module** via ioctls on `/dev/zfs`, and OpenZFS only
guarantees ioctl compatibility **within a matching version line**. If the
userspace tools the agent runs are a different version from the host kernel
module, `zpool list` can fail or return garbage — producing wrong `ZfsPool`
status.

On Talos the kernel module is pinned by the `siderolabs/zfs` system extension
(e.g. `2.4.3-v1.13.6` = OpenZFS 2.4.3), and that extension also ships the
matching `zpool`/`zfs` **on the host**.

## Options

### 1. Bundle utils in the image (simple, drift-prone)
Install `zfsutils-linux` in the discovery image and run it directly.
- **Pro:** self-contained, portable, easy to test.
- **Con:** the image's ZFS version floats with the base image and can drift from
  the host kernel module. You must keep them aligned by hand.

### 2. Host-exec: run the host's own version-matched binaries (implemented, default)
Run the host's `zpool`/`zfs` from the privileged pod, so the CLI version always
matches the kernel module. Two mechanisms, selected by `discovery.hostExec.*`:
- **`chroot /host`** (default) — bind-mount the host root at `/host` and
  `chroot` into it. This is the method the `siderolabs/zfs` extension documents.
- **`nsenter`** — enter the host mount namespace of PID 1 (requires `hostPID`).
  Works too, but is not the Talos-documented path.

Wiring: the controller takes `--host-command-prefix` (e.g. `chroot /host`), and
`zpool`/`zfs` resolve on the host PATH. Set `discovery.hostExec.enabled=false` to
fall back to option 1.
- **Pro:** zero version drift; no extra images.
- **Con:** couples to host layout / Talos; needs a privileged pod.

### 3. Inject the (static) controller into a user-chosen utils image
The controller binary is static (`CGO_ENABLED=0`), so it runs on any base image.
Ship it as a tiny image and run the pod's main container **from a user-specified
`zfs-utils` image** that carries a matching `zpool`/`zfs`, injecting the binary:
- **initContainer copy** → shared `emptyDir` → main (utils image) runs it.
  Portable across all Kubernetes versions.
- **Image Volume** (`volumes[].image`, KEP-4639) → mount the controller image
  read-only into the utils container. Native, no initContainer. This cluster runs
  **Kubernetes 1.36**, where image volumes are available, so this is viable here.

A user with a different ZFS version just points `discovery.utilsImage` at a
matching image; the controller is unchanged.
- **Pro:** decouples controller release cadence from ZFS version; portable to any
  distro (not just Talos); user-swappable.
- **Con:** requires a matching `zfs-utils` image to exist or be built; two-image
  supply chain.

### 4. Make the bundled utils version configurable (weakest)
Thread a `discovery.zfsUtilsVersion` build ARG into the Dockerfile
(`apt-get install zfsutils-linux=${VERSION}`).
- **Con:** `apt` only offers versions in the Debian repo snapshot, so arbitrary
  upstream OpenZFS versions may need backports, a different base image, or a
  build from source. Largely superseded by options 2 and 3.

## Current default

Option 2 (`chroot /host`) is the default because the Talos target ships
version-matched host binaries and documents this exact access pattern. Option 3
is the most portable long-term direction for non-Talos clusters.

## Prior art: how democratic-csi handles it

democratic-csi hits the same coupling and solves it by **running ZFS where the
kernel module lives**, which is the same principle as option 2. Confirmed from
its source:

- **Generic/remote drivers (`zfs-generic-*`, `freenas-*`)** execute ZFS commands
  on the storage host **over SSH**, so the userspace version always matches that
  host.
- **Local drivers (`zfs-local-{dataset,zvol}`)** exec `zfs`/`zpool` directly
  (`LocalCliExecClient`), but the image ships **wrapper scripts** at
  `/usr/local/bin/{zfs,zpool}` that shadow the real binaries and redirect to the
  host via **`chroot /host`** (`docker/zfs`, `docker/zpool`). The wrapper probes a
  list of candidate host paths and falls back to a clean-env `chroot`:

  ```bash
  P="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/run/current-system/sw/bin"
  for p in $P; do
    if [[ -f "/host${p}/zpool" ]]; then chroot /host "${p}/zpool" "$@"; exit $?; fi
  done
  chroot /host /usr/bin/env -i PATH="${P}" zpool "$@"
  ```

- **`nsenter` is only used for `iscsiadm`**, and only as a non-default strategy
  (`ISCSIADM_HOST_STRATEGY` defaults to `chroot`). That branch enters the running
  `iscsid` daemon's mount/net namespaces — a special case because `iscsiadm` must
  talk to a live host daemon, not just run a host binary. It is **not** how
  democratic-csi runs ZFS.

Takeaways that apply to us:

- **`chroot /host` is the right default** — it is precisely what democratic-csi
  uses for ZFS. Our default matches.
- The wrapper's **path probe is now adopted** (`internal/zpool/hostexec.go`): the
  discovery agent probes `/usr/local/sbin` (Talos extension),
  `/run/current-system/sw/bin` (NixOS) and the standard dirs for `zpool`/`zfs`,
  runs the resolved absolute path under `chroot`/`nsenter`, and falls back to
  `env -i PATH=...` so resolution never depends on the container's inherited
  `PATH`.
- Talos has no SSH daemon, so the SSH route democratic-csi uses for remote hosts
  is unavailable; `chroot` (or, for daemon-coupled tools, `nsenter`) is the
  local-node equivalent.



