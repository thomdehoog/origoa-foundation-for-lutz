# Source Structure

Origoa is intentionally one Go module with two internal packages. The HTTP package owns transport and the embedded UI; the repository package owns all data, Git, validation, and concurrency rules.

```text
.
├── cmd/origoa/main.go                 process setup and shutdown
├── internal/
│   ├── httpapi/
│   │   ├── server.go                  routes, middleware, JSON, embedded files
│   │   ├── server_test.go             HTTP lifecycle and hostile-input tests
│   │   └── web/
│   │       ├── index.html             browser entry page
│   │       ├── app.js                 native Web Component application
│   │       └── styles.css              UI styling
│   └── repository/
│       ├── repository.go              domain model, Git storage, validation, queries
│       ├── repository_test.go         functional tests and benchmarks
│       └── adversarial_test.go        process, race, failure, limit, and fuzz tests
├── .github/workflows/ci.yml           release gates
├── .dockerignore                      container build exclusions
├── .gitattributes                     consistent text and binary handling
├── .gitignore                         local runtime and development exclusions
├── Dockerfile                         non-root production container
├── go.mod                             module and Go version
├── README.md                          project overview and API summary
├── INSTALLATION.md                    installation and operation tutorial
└── SPEC_ADAPTATION.md                 design decisions, lessons, and gaps
```

## Dependency direction

Runtime calls flow in one direction:

```text
browser or API client -> internal/httpapi -> internal/repository -> Git and filesystem
                              ^
                       cmd/origoa wires it together
```

The repository package does not know about HTTP or the browser. The browser is compiled into the Go binary with `go:embed`, so deployment does not require a separate static-file server or JavaScript build.

## `cmd/origoa`

[`cmd/origoa/main.go`](cmd/origoa/main.go) is the executable entrypoint. It:

1. Reads `ORIGOA_HOST`, `ORIGOA_PORT`, and `ORIGOA_REPOSITORY`.
2. Opens or initializes the managed Git repository.
3. Constructs the HTTP handler and server timeouts.
4. Listens for requests.
5. Gracefully shuts down on `SIGINT` or `SIGTERM`.

Keep this package limited to process-level concerns. Domain behavior belongs in `internal/repository`; HTTP behavior belongs in `internal/httpapi`.

## `internal/httpapi`

[`internal/httpapi/server.go`](internal/httpapi/server.go) is the transport boundary. It contains:

- The standard-library `http.ServeMux` route table.
- JSON decoding and consistent JSON error responses.
- ETag parsing for optimistic concurrency.
- Request-body and concurrent-request limits.
- Security headers, panic recovery, and request logging.
- Static serving of the embedded browser assets.

Handlers translate HTTP input into repository calls and translate results back to HTTP. They should not duplicate repository validation because non-HTTP callers need the same guarantees.

[`internal/httpapi/web/app.js`](internal/httpapi/web/app.js) contains the dependency-free browser application. It calls the `/api` routes and renders the repository, forms, links, overlays, workflows, and history. `index.html` only mounts the Web Component; `styles.css` owns presentation.

## `internal/repository`

[`internal/repository/repository.go`](internal/repository/repository.go) is the core. It contains five closely related areas:

- Domain types for artifacts, links, overlays, workflows, history, filters, and errors.
- Repository initialization and bounded Git command execution.
- Immutable `HEAD` snapshot construction and commit-keyed caching.
- Create, read, update, delete, search, tree, relationship, overlay, workflow, and history operations.
- Schema, reference, path, JSON, size, and integrity validation.

Git `HEAD` is authoritative. Reads use an immutable snapshot of managed blobs at one commit. Writes acquire a repository-wide advisory file lock, validate against one snapshot, replace files atomically, commit through Git, and verify ambiguous commit outcomes. This keeps the worktree, API result, and committed state aligned for the supported single-node deployment.

The package lives under `internal/`, so Go prevents other modules from importing it as a public library. The supported external interface is the HTTP API.

## Runtime data

Runtime artifacts are not tracked as source. By default they are written to the ignored `.origoa-data/` directory inside the working directory:

```text
.origoa-data/
├── .git/
├── .origoa/
│   ├── config.json
│   ├── schemas/<type>.json
│   └── workflows/<workflow>.json
└── <folder>/<artifact-guid>/artifact.json
```

The entire directory is operational data. Back up `.git` as well as the visible JSON because revisions, history, and authoritative state depend on it.

## Tests

Tests sit beside the package they protect:

- `repository_test.go` covers CRUD, schemas, overlays, workflows, references, search, hierarchy, strict decoding, and read benchmarks.
- `adversarial_test.go` attacks cross-handle and cross-process writes, scheduler interleavings, partial commits, caller mutation, corruption, oversized content, filesystem failures, and ambiguous Git results. It also defines three repository fuzz targets.
- `server_test.go` covers HTTP lifecycle, headers, malformed requests, exact numbers, concurrent updates, overload behavior, and two HTTP fuzz targets.

The CI workflow runs formatting, `go vet`, race and shuffle tests, low/high scheduler stress, all five fuzz targets, JavaScript syntax validation, a stripped production build, and a container build.

## Where changes belong

| Change | Primary location | Required verification |
| --- | --- | --- |
| Add or alter a REST endpoint | `internal/httpapi/server.go` | HTTP tests plus repository tests for new rules |
| Change artifact or workflow behavior | `internal/repository/repository.go` | Functional and adversarial repository tests |
| Change browser behavior | `internal/httpapi/web/` | `node --check` and manual browser verification |
| Change startup, environment, or shutdown | `cmd/origoa/main.go` | Build and process-level smoke test |
| Change release gates | `.github/workflows/ci.yml` | Run the equivalent command locally |
| Change the container | `Dockerfile` | Build and run the image with persistent storage |

Before adding a new package, check whether the behavior belongs to the existing transport or repository boundary. A third layer is justified only when it owns a genuinely separate lifecycle or source of truth.
