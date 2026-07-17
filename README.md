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

`make test` runs `go vet` and the full test suite (including adversarial
tests) with the race detector.

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
internal/core     Foundation: artifacts, schemas, workflows, projection
internal/gitx     bare-repo Git plumbing (CAS commits, batch reads)
internal/ojson    order-preserving JSON (stable repository serialization)
```

The repository is a **bare Git repo**; you can clone it, edit files by hand
and push — the projection tolerates malformed files and can always be
rebuilt (`POST /api/admin/reindex`).

## Deliberate MVP deviations from the design guide

Documented so they are decisions, not accidents:

- **Projection is in-memory, not PostgreSQL.** The design requires the
  database to be a disposable projection of Git; at MVP scale the leanest
  correct projection is memory, rebuilt from HEAD on start and after every
  write (which also guarantees it can never drift). The `core` API is the
  seam where a PostgreSQL projection slots in when repository size demands
  it — nothing above `internal/core` would change.
- **Full rebuild per write** instead of incremental projection updates.
  Correct by construction; optimize only when profiling says so.
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
