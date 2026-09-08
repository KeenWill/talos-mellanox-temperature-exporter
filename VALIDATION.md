# Validation

Validated on 2026-09-08 UTC. This records what was tested, not a compatibility
claim for every mlx5 NIC or Talos installation.

| Check | Result |
|---|---|
| `go test -race -count=1 ./...` | Pass |
| `go vet ./...` and `gofmt` | Pass |
| `docker build --target test .` with pinned Go/C image | Pass |
| Ordinary `exporter` Docker target | Pass; approximately 7.4 MB |
| `talos-extension` Docker target | Pass; approximately 7.4 MB |
| Exporter `--help`, vendor reader `--version` / `--help` in runtime image | Pass |
| Static vendor binary ELF interpreter check | Pass |
| Talos 1.12.6 `extensions.Load`, extension `Validate`, service `Spec.Validate` against extracted image | Pass |
| `promtool check metrics` against live exposition | Pass |
| ConnectX-4 MT4115 ASIC query through exporter | Pass, 68°C, collection success 1 |
| Successive polling cycles | Pass; last-success timestamp advances, with PCI/interface/firmware identity |
| Talos extension install, reboot, and supervised service lifecycle | Pass on a canary |
| Installed extension periodic collection and metrics exposition | Pass; repeated successful polls and healthy scrape |
| Coexistence with existing services | Pass; existing services remained running |
| Other NIC generations, other architectures, optical-module temperatures | Not validated |

Automated tests cover device discovery, exclusion of VFs and other PCI drivers,
allowlist validation, shared-interface identity, malformed/nonfinite/out-of-range
responses, command failure/timeout/output bounds, failed and stale temperatures,
discovery failure, label escaping and HTTP routes. The fake vendor command is the
test executable itself: tests require no hardware, root, network or vendor tools.

The live polling check ran this source as a static binary with the pinned
mstflint source version on an existing Talos 1.12.6 kernel, in a temporary
privileged test container. Only the selected PCI device path was writable.
This established collection behavior before the installed-extension check below;
it does not establish least-privilege portability across runtimes.

The Talos image intentionally grants writable sysfs for generic discovery and
PCI command transport. Its root filesystem is read-only; temporary lockfiles
use a bounded tmpfs. That is broader access than the selected-device hardware
test. Review [PACKAGING.md](PACKAGING.md) before installation.

A subsequent canary deployment verified the packaged extension's boot lifecycle:
the supervised service started after reboot, continued polling successfully, and
provided valid metrics to the monitoring system. Existing services remained
running. This is a pass/fail deployment check, not a guarantee of compatibility
across every supported operating-system release or adapter.

The validated generic extension was built from source commit
`54e54c6f3a3e613ba4d16a79125f72a2822019c4` and published as:

```text
ghcr.io/keenwill/talos-mellanox-temperature-exporter:v0.1.0@sha256:53428e6b509ec46aadefbb747f1c2fd39b539422e9c9c78738cfcf987cd265cc
```

CI builds/tests packaging; deployment and rolling upgrades remain
operator-controlled.
