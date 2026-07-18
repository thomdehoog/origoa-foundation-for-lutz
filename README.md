<div align="center">

# Origoa Foundation

**A Git-backed storage platform for building information management applications** —
requirements management, issue tracking, PLM, documentation, and anything in between.

[![CI](https://github.com/thomdehoog/origoa-foundation-for-lutz/actions/workflows/ci.yml/badge.svg)](https://github.com/thomdehoog/origoa-foundation-for-lutz/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.24-00ADD8?logo=go&logoColor=white)](go.mod)
[![Frontend](https://img.shields.io/badge/Frontend-Lit%20%2B%20TypeScript-324FFF?logo=lit&logoColor=white)](web/)
[![PostgreSQL](https://img.shields.io/badge/Projection-PostgreSQL%20%7C%20in--memory-4169E1?logo=postgresql&logoColor=white)](docs/INSTALL.md)
[![Source of truth](https://img.shields.io/badge/Source%20of%20truth-Git-F05032?logo=git&logoColor=white)](#the-core-idea)

<img src="docs/screenshot.png" alt="Origoa web interface: folder navigation, artifact table with HIDs and workflow states, and a schema-driven detail editor" width="900">

*The schema-driven web UI: repository navigation, artifact overview with HIDs and
workflow states, and a detail editor generated entirely from your schemas.*

</div>

---

The Foundation stores, versions, organizes and relates structured information.
Domain semantics come entirely from repository configuration — define schemas
and workflows as JSON files, and the API and UI adapt to your domain.

## The core idea

> **Never trust metadata when the primary data already exists.**

Git is the single source of truth. Everything else — GUID resolution,
hierarchy, search — is a derived projection that can be rebuilt from the
repository at any time. The repository is a **bare Git repo**: you can clone
it, edit files by hand and push — the projection tolerates malformed files and
can always be rebuilt (`POST /api/admin/reindex`).

## Highlights

- 🗃️ **Four artifact kinds, one complete information model** — entries,
  documents, links and comments cover requirements, tickets, parts, specs and
  their relationships.
- 🧬 **Schema-driven everything** — artifact types, fields, workflows and
  allowed relationships are JSON files in the repository; they compose
  lexically from root to artifact, and the UI renders itself from them.
- 🔀 **Every operation is one Git commit** — with a structured message and
  compare-and-swap publication. Full history for free; `ETag`/`If-Match`
  optimistic concurrency with RFC 9110 semantics.
- 🏷️ **Permanent GUIDs, human-friendly HIDs** — references never break when
  folders are reorganized; entries carry editable IDs like `REQ-42`.
- 🧩 **Entry overlays** — an entry can reference a `base` entry and inherit
  unset fields through the chain (cycles are rejected).
- 🐘 **Pluggable projections** — a zero-dependency in-memory index for
  development, or PostgreSQL (plain SQL, no ORM) with full-text search for
  production. Both are rebuildable from Git at any time.

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

See [docs/INSTALL.md](docs/INSTALL.md) for the full installation tutorial:
build, run, define your first domain, deploy.

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
  message (human subject + `Origoa-Op`/`Origoa-Guid` trailers), published
  via compare-and-swap `update-ref` (no working directory, plumbing only).
  Optimistic concurrency uses the artifact's blob SHA as a quoted `ETag`
  honored by `If-Match` (RFC 9110 semantics, including `*` and `W/`).

## Write path (design guide §10.1)

Every write runs a validated retry loop: synchronize the projection to the
Git head, validate and build the changeset against that state, publish with
one CAS `update-ref`, then project the commit. If the branch moved (a second
server process, a direct `git push`), the loop re-synchronizes and re-runs
the whole validation — If-Match, HID uniqueness and move/delete file sets
are never applied to a stale state, and nothing is ever silently rebased.
The PostgreSQL projection mirrors the Git CAS on `processed_hash`, so two
servers sharing one database can never silently skip a commit; any
divergence (crash, foreign push) is repaired by a full rebuild from Git.

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

Errors are JSON `{"error": ...}` with 400 (validation), 404 (also for a
GUID reached through the wrong collection route), 409 (HID conflict /
concurrent modification), 412 (stale `If-Match`), 503 (projection store
unavailable — reads fail closed rather than fabricating empty answers).
List/search/tree accept `?limit=` (default 1000, max 10000). Request
bodies are capped at 4 MiB; searchable text per artifact is capped at
256 KiB and sanitized so no payload can wedge a projection backend.

## Architecture

```
web/              Lit + TypeScript SPA (schema-driven, no framework)
cmd/origoad       server entry point
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

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for a guided tour of the
source tree and how a request flows through it.

## Testing

`make test` runs `go vet` and the full test suite with the race detector
against the in-memory projection. Set `ORIGOA_TEST_DSN` to run the same
suite against PostgreSQL — CI runs both. Coverage includes:

- **Adversarial suites** — path traversal, pathspec-hostile and unicode
  folder names, NUL bytes, multi-megabyte and deeply nested payloads,
  duplicate HIDs, overlay cycles, stale ETags.
- **Concurrency torture** — mixed operations under concurrent reindexing
  with live-equals-rebuild invariants; two writer processes sharing one
  repository and one projection database.
- **Fuzzing** — Go fuzz targets for the JSON codec, folder validation and
  file classification (`go test -fuzz FuzzRoundTrip ./internal/ojson`).
- **End-to-end** — `make e2e` drives the full REST API against a live
  server; CI runs it against both projections.

## Documentation

| Document | What's inside |
|---|---|
| [docs/INSTALL.md](docs/INSTALL.md) | Installation tutorial: build, run (in-memory and PostgreSQL), define your first domain, deploy. |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Guided tour of the source tree and how a request flows through it. |
| [DESIGN_NOTES.md](DESIGN_NOTES.md) | Spec vs. implementation: what was adopted, adapted, skipped, and why. |

## Deliberate MVP deviations from the design guide

The full rationale — what was adopted, adapted, skipped, what adversarial
testing taught us, and the remaining gaps — lives in
[DESIGN_NOTES.md](DESIGN_NOTES.md). Summary, so they are decisions, not
accidents:

- **Recovery jumps straight to a full rebuild.** The design prefers
  replaying missing commits sequentially and reserves full rebuild for when
  replay can't continue (§5.14). Rebuild is the safe superset and MVP repos
  are small; sequential replay is an optimization for later. Both
  projections apply each own commit incrementally; rebuild is only for
  divergence.
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
