# Origoa Foundation

A small, Git-backed foundation for structured information. It implements the design guide’s backend MVP as one Go binary with an embedded native Web Component UI and no runtime libraries.

## Documentation

- [Installation](INSTALLATION.md): native and Docker setup, verification, upgrades, and production boundaries.
- [Source structure](SOURCE_STRUCTURE.md): package responsibilities, request flow, runtime data, and test layout.
- [Specification adaptation](SPEC_ADAPTATION.md): design decisions, adversarial lessons, and remaining gaps.

## Run

Requirements: Go 1.26+, Git, and Linux or macOS with local filesystem advisory locking.

```sh
go run ./cmd/origoa
```

Open <http://127.0.0.1:3000>. The managed repository is created at `.origoa-data/` and is itself a Git repository.

```sh
go vet ./... && go test -race ./...
docker build -t origoa .
docker run --rm -p 3000:3000 -v origoa-data:/data origoa
```

Environment:

| Variable | Default | Purpose |
| --- | --- | --- |
| `ORIGOA_HOST` | `127.0.0.1` | Listen address. Keep local unless placed behind an authenticating proxy. |
| `ORIGOA_PORT` | `3000` | HTTP port. |
| `ORIGOA_REPOSITORY` | `.origoa-data` | Managed Git repository. |

## Repository format

Artifacts live in `<folder>/<guid>/artifact.json`. Configuration is inherited from root to artifact through `.origoa/` folders:

```text
.origoa/
  schemas/<type>.json
  workflows/<workflow>.json
teams/core/
  .origoa/schemas/<type>.json
  <guid>/artifact.json
```

An effective schema composes matching files from root to artifact; nearer field definitions win. `"inheritance": "off"` resets inherited definitions. Example:

```json
{
  "id": "requirement",
  "fields": {
    "priority": {
      "type": "enumeration",
      "required": true,
      "values": ["low", "high"]
    },
    "owner": { "type": "text" }
  },
  "workflows": ["review"]
}
```

```json
{
  "id": "review",
  "initial": "draft",
  "states": ["draft", "approved"],
  "transitions": [
    { "id": "approve", "from": "draft", "to": "approved" }
  ]
}
```

Commit configuration changes before starting the server. Every API mutation creates one structured Git commit.

## API

| Method | Path | Purpose |
| --- | --- | --- |
| `GET/POST` | `/api/artifacts` | List or create artifacts. Filters: `kind`, `type`, `path`. |
| `GET/PUT/DELETE` | `/api/artifacts/{guid}` | Read, update, or delete an artifact. |
| `GET` | `/api/artifacts/{guid}/schema` | Resolve the effective schema. |
| `GET` | `/api/artifacts/{guid}/overlay` | Resolve an entry overlay chain. |
| `GET` | `/api/artifacts/{guid}/links` | List incoming and outgoing links. |
| `GET` | `/api/artifacts/{guid}/workflows` | List states and available transitions. |
| `POST` | `/api/artifacts/{guid}/transitions` | Execute a workflow transition. |
| `GET` | `/api/artifacts/{guid}/history` | Read Git history. |
| `GET` | `/api/search?q=...` | Search and filter artifacts. |
| `GET` | `/api/repository/tree` | Browse the hierarchy. |
| `GET` | `/api/health` | Health and repository revision. |

`PUT`, `DELETE`, and transitions require the latest strong `ETag` in `If-Match`; stale writes return `412`.

## Guarantees and scope

- Permanent server-generated GUIDs and GUID-only references.
- Lexically inherited schemas and workflows.
- Entry, document, link, and comment persistence; overlays; search; history; relationship and workflow views.
- Cross-process serialized writes, atomic file replacement, optimistic concurrency, strict request limits, path containment, reference integrity, security headers, and non-root container execution.
- Git is authoritative. Reads use a bounded, immutable snapshot keyed by commit, so concurrent callers cannot observe partial state or drift.

This is the deliberately small single-repository MVP. PostgreSQL projections, Git plumbing without a worktree, BlockSuite, WebSockets, extensions, permissions, branching, and distributed repositories are not included. Add them only when repository size, collaboration, or deployment requirements demonstrate the need.

The browser UI covers hierarchy browsing, search/filtering, schema-driven fields, CRUD, overlays, relationships, workflows, and history. It intentionally uses a safe text/JSON document editor rather than claiming full BlockSuite/WYSIWYG composition.

Before exposing the service beyond localhost, put it behind TLS and authentication. The MVP intentionally has no user or permission model.
