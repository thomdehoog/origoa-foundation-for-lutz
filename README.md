# Origoa Foundation

A generic, Git-backed storage platform for building information management
applications (requirements management, issue tracking, PLM, documentation, …).
The Foundation stores, versions, organizes and relates structured information;
domain semantics come entirely from repository configuration.

**First design goal: never trust metadata when the primary data already
exists.** Git is the single source of truth. Everything else — GUID
resolution, hierarchy, search — is a derived projection that can be rebuilt
from the repository at any time.

## Concepts

Four native artifact kinds form the complete information model:

| Kind | Purpose | Stored at |
|---|---|---|
| **Entry** | Reusable structured object (requirement, ticket, part, …) | `<folder>/<guid>/.origoa.json` |
| **Document** | Hierarchical composition of text and entry references | `<folder>/<guid>/.origoa.json` |
| **Link** | Directed, typed relationship between two artifacts | `<scope>/.origoa/links/<guid>.json` |
| **Comment** | Threaded annotation on any artifact | `<scope>/.origoa/comments/<guid>.json` |

- Every artifact has a permanent **GUID**; references are always GUID-based,
  so folders can be reorganized freely without breaking anything.
- Entries and documents may carry a unique, editable **HID**
  (e.g. `REQ-42`), auto-generated from a schema prefix.
- **Entry overlays**: an entry may reference a `base` entry; unset fields
  resolve from the base chain (cycles are rejected).
- **Schemas** (`<scope>/.origoa/schemas/*.json`) define artifact types,
  fields, workflow assignments and allowed relationships. They compose
  lexically from repository root to artifact; the nearest definition wins,
  and `"inheritance": "off"` severs everything above.
- **Workflows** (`<scope>/.origoa/workflows/*.json`) are state machines
  resolved lexically; an artifact can participate in several independently.
- Every logical operation is exactly one Git commit with a structured
  message, published via compare-and-swap `update-ref` (no working
  directory, plumbing only). Optimistic concurrency uses the artifact's
  blob SHA as ETag / `If-Match`.

## Quickstart

```sh
make build           # builds web/dist and bin/origoad
make run             # serves http://127.0.0.1:8080 with repo in data/origoa.git
./examples/seed.sh   # populate a demo requirements domain
```

With PostgreSQL (plain SQL, no ORM — tables are auto-created):

```sh
./bin/origoad -repo data/origoa.git -web web/dist \
  -db "postgres://user:pass@localhost:5432/origoa?sslmode=disable"
```

`make test` runs `go vet` and the full test suite (including adversarial
tests) with the race detector against the in-memory projection. Set
`ORIGOA_TEST_DSN` to run the same suite against PostgreSQL — CI runs both.

## REST API

```
GET    /api/tree                          folders + all artifact metas
GET    /api/search?q=&kind=&type=         full-text + metadata search
POST   /api/entries | /api/documents      {path, type, title, hid?, base?, fields?, content?}
GET    /api/entries/{guid}?resolve=1      artifact (+ overlay-resolved fields); ETag header
PUT    /api/entries/{guid}                patch title/hid/base/fields/content; honors If-Match
DELETE /api/{entries|documents|links|comments}/{guid}
POST   /api/links                         {type, source, target, fields?}
POST   /api/comments                      {subject, text, parent?, author?}
GET    /api/artifacts/{guid}/links        incoming + outgoing
GET    /api/artifacts/{guid}/comments
GET    /api/artifacts/{guid}/history      structured commit log
POST   /api/artifacts/{guid}/move         {path}
POST   /api/artifacts/{guid}/transition   {workflow, to}
GET    /api/schemas                       all definitions by scope
GET    /api/schemas/effective?type=&path= composed schema
PUT    /api/schemas/{name}?scope=         store a schema definition
GET    /api/workflows/{id}?path=          resolved workflow definition
PUT    /api/workflows/{name}?scope=       store a workflow definition
```

Errors are JSON `{"error": ...}` with 400 (validation), 404, 409 (HID or
concurrent-edit conflict).

## Architecture

```
web/            Lit + TypeScript SPA (schema-driven, no framework)
cmd/origoad     server entry point
internal/httpapi  REST layer
internal/core     Foundation: artifacts, schemas, workflows, projections
internal/gitx     bare-repo Git plumbing (CAS commits, batch reads)
internal/ojson    order-preserving JSON (stable repository serialization)
```

The query layer is a `Projection` with two implementations, selected by the
`-db` flag:

- **In-memory** (default): zero dependencies, rebuilt from Git HEAD on start
  and after every write. Ideal for development, tests, and small repos.
- **PostgreSQL** (per the design guide): plain SQL, `processed_hash`
  revision tracking, folder-prefix and GIN full-text indexes. Each commit is
  projected in one transaction; on startup, a matching `processed_hash`
  reuses the stored projection, any divergence (crash, foreign push)
  triggers a full rebuild from Git.

The repository is a **bare Git repo**; you can clone it, edit files by hand
and push — the projection tolerates malformed files and can always be
rebuilt (`POST /api/admin/reindex`).

## Deliberate MVP deviations from the design guide

Documented so they are decisions, not accidents:

- **Recovery jumps straight to a full rebuild.** The design prefers
  replaying missing commits sequentially and reserves full rebuild for when
  replay can't continue (§5.14). Rebuild is the safe superset and MVP repos
  are small; sequential replay is an optimization for later.
- **The in-memory projection applies writes by full rebuild** — correct by
  construction; the PostgreSQL projection is the incremental path.
- **Cardinality is stored but not enforced** — the design assigns
  validation and automation to the application layer. Link *type/target
  allowlists* are enforced because the design names them as constraints.
- **No auth, no WebSocket presence service, no extension hooks, no
  BlockSuite editor** — all explicitly outside or beyond MVP scope in the
  design guide.
- **Metadata locality on move**: links/comments stay where they were
  created; they reference GUIDs, so this is a "preferred invariant" the
  design allows to be restored later by maintenance operations.
- **HID history** lives in Git history (each HID change is a commit)
  rather than a separate lookup structure.
