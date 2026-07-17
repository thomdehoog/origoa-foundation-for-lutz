# Installation

Origoa runs as one Go binary and stores its data in a local Git repository. Choose either the native installation or Docker; both serve the same embedded browser UI and API.

## Requirements

- Linux or macOS on a local filesystem that supports advisory file locking.
- Git available on `PATH`.
- One of:
  - Go 1.26 or newer for a native build.
  - Docker for the container installation.

Windows-native hosting, network filesystems without reliable locks, and multiple servers sharing one repository are outside the supported deployment model.

## Get the source

If the repository has been published, replace `<repository-url>` with its URL:

```sh
git clone <repository-url> origoa-foundation
cd origoa-foundation
```

For an existing local checkout, run the remaining commands from its root—the directory containing `go.mod` and `Dockerfile`.

## Option 1: native installation

Confirm the tools are available:

```sh
go version
git --version
```

Run the tests and build a compact binary:

```sh
go test ./...
mkdir -p bin
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o ./bin/origoa ./cmd/origoa
```

Start the server with an explicit data location:

```sh
ORIGOA_REPOSITORY="$(pwd)/.origoa-data" ./bin/origoa
```

Open <http://127.0.0.1:3000>. On first start, Origoa creates the data directory as a Git repository and commits its initial configuration.

Stop it with `Ctrl-C`. `SIGINT` and `SIGTERM` both trigger a graceful shutdown.

To install the binary system-wide:

```sh
sudo install -m 0755 ./bin/origoa /usr/local/bin/origoa
```

Run it under the operating system's service supervisor with a dedicated user, a writable persistent repository directory, and the environment variables below.

## Option 2: Docker

Build the image and create persistent storage:

```sh
docker build -t origoa:local .
docker volume create origoa-data
```

Start the container. Publishing only on `127.0.0.1` prevents direct network exposure:

```sh
docker run --detach \
  --name origoa \
  --restart unless-stopped \
  --publish 127.0.0.1:3000:3000 \
  --volume origoa-data:/data \
  origoa:local
```

Inspect or stop it with:

```sh
docker logs origoa
docker stop origoa
```

Removing the container does not remove the named volume. Do not delete `origoa-data` unless its artifacts and Git history are no longer needed.

## Verify the installation

With the server running:

```sh
curl --fail --silent --show-error http://127.0.0.1:3000/api/health
```

A healthy response has this shape:

```json
{"revision":"<git-commit>","status":"ok"}
```

The browser UI should load at <http://127.0.0.1:3000>. Logs are written to standard output.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `ORIGOA_HOST` | `127.0.0.1` | Listen address. The container sets this to `0.0.0.0` internally. |
| `ORIGOA_PORT` | `3000` | HTTP port, from 1 through 65535. |
| `ORIGOA_REPOSITORY` | `.origoa-data` | Persistent managed Git repository. Prefer an absolute path for services. |

Example:

```sh
ORIGOA_HOST=127.0.0.1 \
ORIGOA_PORT=8080 \
ORIGOA_REPOSITORY=/var/lib/origoa \
/usr/local/bin/origoa
```

Schemas and workflows are normal JSON files below `.origoa/` inside the managed repository. Stop the server before editing them directly, then commit the changes before restarting:

```sh
git -C /var/lib/origoa add .origoa
git -C /var/lib/origoa commit -m 'Update Origoa configuration'
```

API-created artifacts are committed automatically. Do not run concurrent Git writers against a live repository; they do not participate in Origoa's write lock.

## Production checklist

- Keep Origoa bound to loopback or a private container network.
- Put an authenticating TLS reverse proxy in front of it. Origoa does not provide TLS, users, or permissions.
- Persist and back up the complete managed repository, including its `.git` directory.
- Rehearse restoration and verify it with `git fsck --full` and `/api/health`.
- Run only one Origoa service against a repository and keep the data on a local filesystem.
- Monitor process restarts, logs, disk space, latency, and health checks.

## Upgrade

Back up the managed repository first. For a native installation, stop the service, build and install the new binary, then restart it with the same `ORIGOA_REPOSITORY`.

For Docker, build a new image tag, stop and remove only the container, and recreate it with the existing `origoa-data` volume. The repository format remains Git-readable independently of the executable.

## Troubleshooting

- `git: executable file not found`: install Git and ensure the service user's `PATH` includes it.
- `permission denied`: make the repository directory writable by the user running Origoa; do not run the service as root to hide ownership errors.
- `address already in use`: select another `ORIGOA_PORT` or stop the process already using it.
- Startup reports repository corruption: stop writes, preserve a copy, and inspect `git -C <repository> status` plus `git -C <repository> fsck --full` before changing data.
- Health requests fail while the process is running: inspect standard-output logs and confirm the host, port, proxy, and container port mapping.
