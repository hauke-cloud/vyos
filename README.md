<!-- llm-readme-management spec=1 commit=a501ed805ca8b35d17f938b2a2773db758fc09cc template=golang model=qwen3.8-27b-q4 digest=b3d4b07f2e19 generated=2026-10-12T14:16:30Z -->
<a href="https://hauke.cloud" target="_blank"><img src="https://img.shields.io/badge/home-hauke.cloud-brightgreen" alt="hauke.cloud" style="display: block;" /></a>
<a href="https://github.com/hauke-cloud" target="_blank"><img src="https://img.shields.io/badge/github-hauke.cloud-blue" alt="hauke.cloud Github Organisation" style="display: block;" /></a>
<a href="https://github.com/hauke-cloud/llm-readme-management" target="_blank"><img src="https://img.shields.io/badge/template-golang-orange" alt="Repository type - golang" style="display: block;" /></a>


# vyos


<img src="https://raw.githubusercontent.com/hauke-cloud/.github/main/resources/img/organisation-logo-small.png" alt="hauke.cloud logo" width="109" height="123" align="right">


<llm header hint="Name the Go module path and say whether this is a service, a CLI or a library.">

This repository builds a container image (`ghcr.io/hauke-cloud/vyos`) from the VyOS rolling-release nightly ISO and ships a single-purpose Go CLI, `hcloud-vrrp-failover`, that reassigns Hetzner Cloud floating IPs, alias IPs, and network routes to whichever router becomes VRRP master. It is for operators running a two-router VyOS failover pair on Hetzner Cloud.

</llm>


## :book: Description

<llm description>

This repository packages the VyOS rolling-release nightly ISO into a container image (`ghcr.io/hauke-cloud/vyos`) and bundles a Go binary, `hcloud-vrrp-failover`, that moves Hetzner Cloud addresses during VRRP failover. It targets an operator running a two-router VyOS pair on Hetzner Cloud who needs a bootable VyOS userland in a container and a helper that reassigns Floating IPs, alias IPs, and network routes to the new VRRP master.

The build downloads the pinned nightly ISO, verifies its minisign signature, converts it to an OCI rootfs, and assembles a `FROM scratch` image with systemd as PID 1 and the statically compiled helper. A weekly workflow opens a PR bumping the ISO pin to the newest nightly tag.

- Builds a `linux/amd64` image with systemd as init and a `systemctl is-system-running` healthcheck.
- `hcloud-vrrp-failover` runs as a VRRP transition script; on master election it assigns Floating IPs, migrates alias IPs, and repoints network routes via the Hetzner Cloud API.
- The takeover is idempotent, polls actions to completion, and retries every 2 s until success or a configurable timeout.

The image is what `hauke-cloud/router-api` runs on its router instances.

</llm>


## :clipboard: Requirements

<llm requirements hint="Give the Go version from the go directive in go.mod. Mention Docker only if the repository actually builds an image.">

- Go 1.27.1 (pinned in `go.mod`).
- A container engine — Docker or Podman — running rootful. Rootless boots but cannot commit firewall or WireGuard configuration.
- For `make rootfs`: `xorriso`, `squashfs-tools`, `jq`, `xz-utils`, and either a `minisign` binary (v0.3.0) or Go (which fetches it via `go run`). Must run as root or with `fakeroot` installed.
- For `make smoke`: `curl`, `jq`, `openssl`.
- For contributing: `pre-commit`.
- A Hetzner Cloud API token with read/write scope, stored in a file referenced by `tokenFile` in `/config/hetzner/failover.json`.
- The router instance must run on Hetzner Cloud so the helper can query the metadata service for its server ID.
- A configured DNS name server on the VyOS instance; without it the helper cannot reach `api.hetzner.cloud`.
- amd64 architecture only (VyOS nightlies are published for amd64 only).

</llm>


## 🚀 Getting started

<llm getting_started hint="Cover go build, go run and go test with the real package paths. If a Makefile or Taskfile exists, prefer its targets over raw go commands.">

1. Clone the repository and enter the working directory.

```bash
git clone https://github.com/hauke-cloud/vyos.git
cd vyos
```

2. Build the `hcloud-vrrp-failover` helper binary for your host.

```bash
make build
```

3. Run the unit test suite with the race detector.

```bash
make test
```

4. Build the container image. This step downloads and verifies the pinned VyOS nightly ISO, so it requires `xorriso`, `squashfs-tools`, `jq`, `xz`, a `minisign` binary (or Go), and root or `fakeroot`.

```bash
make rootfs
make image
```

The resulting image is tagged `ghcr.io/hauke-cloud/vyos:dev` by default. You can override the tag with `IMAGE_TAG=your-tag make image`.

</llm>


## :airplane: Usage

<llm usage hint="For a library, show a small import-and-call example using real exported identifiers. For a service or CLI, show how it is started and the flags or subcommands it accepts.">

**Run the image**

The image is published at `ghcr.io/hauke-cloud/vyos`. Start it with a rootful container engine:

```bash
podman run -d --name vyos --privileged --network host \
  -v /lib/modules:/lib/modules:ro \
  -v vyos-config:/opt/vyatta/etc/config \
  ghcr.io/hauke-cloud/vyos:latest
```

The named volume `vyos-config` holds your VyOS configuration and the failover helper's config file.

**Wire up VRRP failover**

Inside VyOS, register the helper as the master transition script for your VRRP group:

```
set high-availability vrrp group wan transition-script master '/usr/local/bin/hcloud-vrrp-failover wan'
```

The helper reads `/config/hetzner/failover.json` (override the path with `-config`). The JSON maps each VRRP group name to its `floatingIPs`, `aliasIPs`, and `routes` (each route carries a `network` index and a `destination` CIDR). The top-level `tokenFile` field points to a file containing your Hetzner Cloud API token. The helper also needs a working name server (`set system name-server …`) so it can reach `api.hetzner.cloud`.

**Build from source**

```bash
make ci-image   # rootfs + image + smoke test
make test       # go test -race -count=1 ./...
make check      # fmt + vet + lint + test
```

`make rootfs` downloads and verifies the nightly ISO pinned in `VYOS_VERSION`; `make image` builds the container; `make smoke` boots it and round-trips configuration through the REST API.

</llm>


## :wrench: Configuration

<llm configuration hint="Environment variables and CLI flags, taken from the flag definitions or the config struct.">

The runtime helper `hcloud-vrrp-failover` takes four CLI flags and reads a JSON config file.

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `-config` | string | `/config/hetzner/failover.json` | Path to the JSON config file |
| `-timeout` | duration | `2m` | Maximum retry window for the takeover |
| `-server-id` | int64 | `0` | Hetzner server ID; `0` queries the metadata service |
| `-version` | bool | — | Print version and exit |

The config file (`/config/hetzner/failover.json`) rejects unknown fields:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `tokenFile` | string | yes | Path to a file holding the Hetzner Cloud API token (read/write scope) |
| `groups` | map[string]Group | yes | One entry per VRRP group name |

Each `Group` value holds `floatingIPs` (`[]string`), `aliasIPs` (`[]string`, must fall inside a local subnet), and `routes` (`[]Route` with `network` int64 non-zero and `destination` CIDR).

Build-time environment variables consumed by `scripts/build-rootfs.sh`:

| Variable | Default | Description |
|----------|---------|-------------|
| `VYOS_VERSION` | value of the `VYOS_VERSION` file | Nightly tag to download |
| `VYOS_ARCH` | `amd64` | ISO architecture |
| `CACHE_DIR` | `.cache` | ISO cache directory |
| `BUILD_DIR` | `build` | Output directory for `rootfs.tar.xz` |

The Makefile additionally exposes `IMAGE` (default `ghcr.io/hauke-cloud/vyos`), `IMAGE_TAG` (default `dev`), `CONTAINER_ENGINE`, `GOLANGCI_LINT_VERSION`, and `ISO_TO_OCI_COMMIT`. The Dockerfile accepts `VERSION` and `COMMIT` ARGs, populated by CI.

</llm>


## :hammer: Development

<llm development hint="Include go test, go vet and gofmt only where the CI workflows actually run them.">

Before pushing, run the full local check:

```bash
make check
```

This runs `make fmt`, `make ci-lint` (gofmt check, `go vet ./...`, golangci-lint v2.14.0), and `make test` (`go test -race -count=1 ./...`). CI runs the same `make ci-lint` and `make test` targets, then `make ci-image` (rootfs build, image build, smoke test) before the inpacken action pushes to `ghcr.io`.

Pre-commit hooks are mandatory. Install them once:

```bash
pre-commit install
```

Run them across the tree before committing:

```bash
pre-commit run --all-files
```

PR titles are validated by a workflow. The title must begin with a conventional-commit type (`fix`, `feat`, `docs`, `ci`, `chore`), a colon, and a space; the subject must start with an uppercase letter. A `[WIP]` prefix is permitted.

`scripts/iso-to-oci` is a pinned copy of an upstream script. Refresh it with:

```bash
make update-iso-to-oci
```

and commit the result. Generated artefacts under `build/`, `bin/`, `.cache/`, and `coverage.out` are gitignored; do not commit them.

</llm>


## 📄 License

This Project is licensed under the GNU General Public License v3.0

- see the [LICENSE](LICENSE) file for details.


## :coffee: Contributing

To become a contributor, please check out the [CONTRIBUTING](CONTRIBUTING.md) file.


## :email: Contact

For any inquiries or support requests, please open an issue in this
repository or contact us at [contact@hauke.cloud](mailto:contact@hauke.cloud).
