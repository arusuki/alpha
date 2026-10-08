# project alpha

English · [简体中文](docs/README_cn.md)

A self-hosted workspace for managing containers, storage, and users across Linux servers.

Open a node from the control panel to explore disk usage, manage containers, and follow running processes. When people share servers, you can invite users, provision their workspaces, and see which containers belong to whom.

[Quick start](#quick-start) · [Documentation](#documentation) · [Development](docs/development.md)

![project alpha cluster control panel](docs/images/cluster.png)

*The control panel with two compute nodes in a local test environment.*

## Features

- **Manage multiple hosts** — Check node connectivity and open each host's workspace from one place.
- **Understand disk usage** — Explore storage by directory, container, or user, distinguish exclusive and shared space, and keep scan results for later comparison.
- **Prepare workspaces** — Create, start, stop, and manage containers, or import existing ones.
- **Manage users** — Collect registration details through invitations and provision containers, Tailscale shares, and `alpha-jump` public keys. Users can view their own resources and request containers on new nodes.
- **Investigate issues** — Explore Tetragon process trees and use Agent to analyze host or container storage and generate reports. Administrators choose what to clean up.

The control service provides the web interface and central management. Each compute node runs a worker. Both use the same binary with separate data directories. An optional registry service provides a public registration entry point.

## Quick start

You need a Linux host with **Go 1.26+** and **GCC**. Compute nodes using container features also need the Docker CLI and a service account with access to the Docker daemon.

### 1. Start the control service

Build and run from the repository directory:

```bash
go build -o bin/project-alpha ./cmd/project-alpha
./bin/project-alpha --control --data-dir ./control-data
```

Open <http://127.0.0.1:8765> and follow the prompts to create an administrator account.

If the service runs on a remote server, you can access it through an SSH tunnel:

```bash
ssh -L 8765:127.0.0.1:8765 user@your-server
```

### 2. Connect a compute node

Copy the built binary to the compute node and run:

```bash
./bin/project-alpha --worker --data-dir ./node-data --host 0.0.0.0 --port 8766
```

The startup log prints the node's connection token. Add a node in the control panel with a name, an address reachable from the control service (such as `http://10.0.0.11:8766`), and that token.

The control service and a worker can run on the same machine with different ports and data directories. The worker saves its token in its data directory and reuses it after a restart.

### 3. Explore your workspace

Select a connected node to open its workspace:

- Configure the scan scope, run a scan, and explore disk usage.
- Set the container image, data directory, and SSH ports before creating workspaces.
- To open registration to external users, configure invitations and user resources in the control panel, then deploy a registry using the [public registration guide](docs/operations.md#公网-registry).

For a persistent deployment, see the included [systemd and nginx configuration](docs/operations.md#部署). The [cluster guide](docs/cluster.md) covers node connections, permissions, and APIs.

## Linux releases and CI

The [Linux CI and Release workflow](.github/workflows/release.yml) runs Go race tests, `go vet`, and frontend JavaScript tests on pushes to `master`, pull requests targeting `master`, and manual runs, then builds Linux amd64 and arm64 packages. These builds are available as Artifacts on the Actions page for 14 days.

To publish a release, create and push a new version tag on a commit containing the workflow. For example, when releasing version 0.5.3:

```bash
git tag v0.5.3
git push origin v0.5.3
```

After all checks and both architecture builds pass, a `v*` tag push creates a GitHub Release with:

- `project-alpha_<tag>_linux_amd64.tar.gz`
- `project-alpha_<tag>_linux_arm64.tar.gz`
- `SHA256SUMS`

Tags containing a hyphen, such as `v0.5.3-rc.1`, are marked as prereleases. Failed runs can be rerun from Actions; assets with matching names are replaced if the Release already exists. Branch, pull request, and manual runs only produce Artifacts. The workflow uses the repository's built-in `GITHUB_TOKEN` and needs no additional Secret.

Each archive contains `bin/project-alpha`, `bin/rootless-docker`, `bin/alpha-updater`, `README.md`, `docs/`, `deploy/`, and `BUILD_INFO` recording the version, commit, architecture, and Go version. Web assets are embedded in the main binary.

Run `project-alpha --version` to check the binary's version and build information. Use `project-alpha --help` for a short command overview, and `<subcommand> --help` (such as `serve --help` or `share-node --help`) for detailed options and examples.

The binaries are compiled natively for each architecture inside an Ubuntu 20.04 container, with CGO enabled for SQLite. They require glibc 2.31 or newer, such as Ubuntu 20.04, and do not directly support Alpine/musl. Release packages do not require Go or GCC to run; Docker and other features still need their runtime dependencies. For older Linux distributions, build from source using the [quick start](#quick-start).

The workflow uses Ubuntu 24.04 runners with the compiler, headers, libraries, and packaged binary checks inside `ubuntu:20.04`. CI checks the container's glibc version and rejects binaries requiring GLIBC symbols newer than 2.31. Release builds do not reuse the host's Go/CGO caches.

After downloading the archive for your architecture and `SHA256SUMS`, for example for version 0.5.3:

```bash
# Check only downloaded architectures; omit --ignore-missing if you downloaded both.
sha256sum --check --ignore-missing SHA256SUMS
tar -xzf project-alpha_v0.5.3_linux_amd64.tar.gz
cd project-alpha_v0.5.3_linux_amd64
./bin/project-alpha --control --data-dir ./control-data
```

## Documentation

The detailed guides are currently available in Chinese.

| Topic | Guide |
| --- | --- |
| Node connections, service roles, and permissions | [Cluster management](docs/cluster.md) |
| GPU processes, utilization, and rolling 72-hour usage | [GPU management](docs/gpu.md) |
| Creating, importing, and managing containers | [Container management](docs/containers.md) |
| Invitations, registration forms, and user registration | [User registration](docs/members.md) |
| Share node installation, service SSH identity, and troubleshooting | [Share node configuration](docs/share-node.md) |
| Tailscale shares, jump account keys, and resource reclamation | [Access and user resources](docs/bastion.md) |
| How storage usage is calculated | [Storage accounting](docs/accounting.md) |
| Scan results, directory exploration, and incremental updates | [Records and exploration](docs/records.md) |
| Agent reports and cleanup | [Agent API](docs/agent.md) |
| Public registration, backups, and deployment | [Running and configuration](docs/operations.md) |
| The standalone rootless Docker tool | [Rootless Docker](docs/rootless-docker.md) |
| Tests, benchmarks, and code structure | [Development and verification](docs/development.md) |

## Development

The backend uses Go and SQLite. Web assets are embedded in the binary. Common checks:

```bash
go test -race ./...
go vet ./...
for test in tests/test_*.js; do node "$test" || exit; done
```

Frontend tests require Node.js 20+ and have no npm dependencies. See [development and verification](docs/development.md#验证) for browser regression tests and Docker integration tests.

The current version is **0.5.3**, and the project is under active development. Before 1.0, database, configuration, API, and snapshot formats may change. [alpha-updater](docs/updater.md) updates local release binaries and upgrades databases in place, starting with v0.3.1. Other incompatible formats require a new data directory; existing data is preserved. See [data storage and backups](docs/operations.md#配置与数据).
