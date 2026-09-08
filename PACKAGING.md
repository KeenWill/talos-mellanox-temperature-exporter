# talos-mellanox-temperature-exporter packaging

The Dockerfile builds two independent distribution formats from the same static
binaries. The ordinary OCI image starts the exporter; the Talos image contains an
extension manifest and an isolated extension-service root filesystem. Neither
format installs drivers, changes NIC configuration, or upgrades firmware.

## Build

```sh
docker buildx build --platform linux/amd64 --target exporter \
  --provenance=false --load -t talos-mellanox-temperature-exporter:dev .
docker buildx build --platform linux/amd64 --target talos-extension \
  --provenance=false --load -t talos-mellanox-temperature-exporter-extension:dev .
```

Run the Go verification stage separately:

```sh
docker buildx build --platform linux/amd64 --target test .
```

It runs race-enabled tests and `go vet`; final image builds do not implicitly run
this stage.

Use `--target` explicitly: the default final stage is the Talos extension. The
initial supported build platform is Linux amd64; other architectures need their
own build and hardware validation. Production image publication is a separate
step. Pin the resulting image digest wherever it is deployed.

The source/toolchain pins are:

| Component | Pin |
|---|---|
| Dockerfile frontend | `docker/dockerfile:1@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32` |
| Go and C build environment | `golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36` |
| mstflint release | `v4.37.0-1`, commit `a2b9858b59196c0d9a2224ac8ebcd86292f9abef` |
| Release source archive SHA256 | `89299633cc78c17032547aa78b9df1d9bf08c5bd6279cde7750bd4d4367afb81` |

The source archive is checked by Docker before extraction. All compilation tools
come from the pinned build image; the build does not install packages from a
mutable distribution repository. Both runtime binaries are static, so the final
images contain no shell, package manager, compiler, or C runtime libraries.
Upstream mstflint notices are retained under `/usr/share/licenses/mstflint`.

Build checks execute `mstmget_temp --version` and `--help`, then reject a binary
that depends on an ELF interpreter. These checks do not open a NIC. Go tests and
hardware collection checks are separate validation steps.

## PCI access and runtime restrictions

Temperature queries use the firmware GET API. The underlying PCI transport still
needs to write request/semaphore registers through PCI configuration space. It
requires root, the relevant capabilities/security-policy allowances, and writable
access to the selected PCI device's `config` file. A read-only sysfs mount may
allow discovery while every actual temperature query fails.

The sysfs discovery tree contains symlinks from `/sys/bus/pci/devices` into
`/sys/devices`. A writable mount of only the former does not provide writable
access to the symlink targets. With an explicit device allowlist, a host-specific
service can mount individual canonical PCI config files writable over an
otherwise read-only sysfs tree. The generic Talos extension uses writable sysfs
because it discovers devices dynamically; this is a significant privilege, even
though this exporter invokes only temperature reads.

The ordinary OCI image runs as UID 0. It does not grant capabilities or mount host
files by itself. Its administrator must configure the required host access. Use
a read-only container root filesystem and a bounded tmpfs at `/tmp` for the
mstflint lock files. Do not mount an entire writable host root. Restrict metrics
port 9835 to monitoring clients; the endpoint has no authentication or TLS.
The exact minimum capability set and SELinux policy require validation against
the intended runtime and NIC. A successful `--help` check does not establish that
those permissions are sufficient.

The ASIC and the pluggable optical module are separate temperature sources. The
packaged reader is invoked in ASIC-only mode (`--no-modules`). Some passive cables
have no temperature sensor. This package does not interpret missing module data
as a hardware fault or a zero-degree reading.

## Talos extension

`extension/nic-temperature.yaml` defines service `ext-nic-temperature`. It runs
continuously, waits 60 seconds between completed polling cycles, and exposes
port 9835 on the node network.
The service root remains read-only. A 16 MiB noexec/nosuid/nodev tmpfs provides
`/tmp`; sysfs is writable for PCI command transport. Default Talos masked/read-only
paths are retained, and mount propagation stays private. Talos extension services
are privileged containers; this package does not claim process-level isolation
from all host hardware.

The service leaves listen address, interval, timeout, and PCI allowlist at the
exporter defaults. Override them with an `ExtensionServiceConfig` environment:
`MELLANOX_TEMPERATURE_LISTEN_ADDRESS`, `MELLANOX_TEMPERATURE_POLL_INTERVAL`,
`MELLANOX_TEMPERATURE_QUERY_TIMEOUT`, and comma-separated
`MELLANOX_TEMPERATURE_PCI_DEVICES`. CLI arguments take precedence over environment
values.

The initial manifest compatibility range is Talos 1.12.x. It uses the documented
[extension service format](https://docs.siderolabs.com/talos/v1.12/build-and-extend-talos/custom-images-and-development/extension-services).
That range describes the intended platform; validate the packaged service on a
canary node before a fleet rollout. Hardware/SELinux behavior is not established
by building the image.

Include the digest-pinned `talos-extension` image with **all** other required
extensions when building an installer using the Talos imager. Use an imager
version matching the target Talos release. Install/upgrade a canary from that
installer, verify the service and real metrics, and then roll through the other
nodes using the operator's normal maintenance procedure. System extensions are
applied at install or upgrade time, not by a live machine-config apply. Do not
replace a node's existing extension set accidentally.

Example read-only verification after installation:

```sh
talosctl --nodes NODE get extensions
talosctl --nodes NODE service ext-nic-temperature
talosctl --nodes NODE logs ext-nic-temperature
curl --fail http://NODE:9835/metrics
```

A running process proves only that the HTTP/polling service started. Verify that
supported devices are discovered, individual queries succeed, sample timestamps
advance, and temperatures agree with a direct query. Collection failures must
remain visible and must not leave a stale temperature looking current.

## Supported transport, not a kernel workaround

The temperature reader comes from NVIDIA's open-source
[mstflint project](https://github.com/Mellanox/mstflint). NVIDIA documents
[PCI-address access without MST kernel modules](https://docs.nvidia.com/networking/display/mftv4221526lts/running-mst-in-an-environment-without-a-kernel),
including ConnectX-4. This permits a userspace workaround for cards whose firmware
capability reporting prevents Linux hwmon from registering their sensors. It does
not guarantee that every adapter, firmware, or security policy supports that
access. This package does not load MST modules or replace the mlx5 driver.
