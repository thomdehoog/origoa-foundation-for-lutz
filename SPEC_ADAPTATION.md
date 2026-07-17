# From Design Guide to This Repository

## The decision

This repository implements the smallest credible Origoa Foundation MVP: one Go service, one Git repository as the source of truth, and one dependency-free browser UI. It deliberately does not pretend to be the full target architecture described in the design guide.

The reason is risk concentration. The first version needed to prove stable identity, Git history, schemas, overlays, relationships, workflows, and a usable API before adding a projection database, distributed coordination, an editor framework, or an extension runtime. Each deferred subsystem creates another source of state or execution that must be recovered, secured, and kept consistent.

## What was adapted

### Git remains authoritative

Artifacts, schemas, and workflows are read from committed `HEAD`, not from the mutable worktree or a secondary database. Every API mutation produces one focused Git commit, and history is read from Git.

Why: this preserves the guide's strongest rule—derived state must never overrule primary data. The in-memory snapshot is keyed by the commit hash and is discarded when `HEAD` changes, so it is an optimization rather than a second authority.

### Identity is independent of location

Entries, documents, links, and comments have permanent server-generated GUIDs. References use GUIDs, while folders are used only for organization and lexical configuration. Optional HIDs are repository-unique.

Why: moves and reorganizations should not invalidate relationships. GUID normalization and reference checks also make identity rules consistent across mixed-case input and direct Git content.

### The four native artifact kinds are present

The backend persists entries, documents, links, and comments using `artifact.json` inside GUID-named folders. It supports CRUD, link navigation, comment parent/subject references, overlay chains, history, search, filtering, and hierarchy views.

Why: keeping these concepts orthogonal demonstrates that domain behavior can be composed without adding business-specific core objects.

### Repository-local configuration drives behavior

Schemas and workflows are inherited lexically through `.origoa` folders. Schemas drive field validation and the UI. Workflows are assigned by schemas, expose only valid transitions, and cannot be bypassed through ordinary updates. Overlay fields are resolved from their base chain before validation.

Why: configuration is useful only if it fails closed. Unknown field types, malformed assignments, missing workflows, invalid states, cycles, and broken references are rejected instead of silently weakening validation.

### The REST and browser layers are thin

The API exposes lifecycle, schema, overlay, link, workflow, history, search, and tree operations. Strong ETags protect updates, request bodies are bounded, excess concurrent work receives `503` with `Retry-After`, and the server has explicit HTTP timeouts and security headers. The embedded Web Component UI provides browsing, type/kind search, schema-driven fields, CRUD, relationships, overlays, workflows, and history.

Why: a single embedded UI keeps deployment and version compatibility simple while still proving the repository can be used without direct Git interaction.

## Deliberate adaptations from the guide

### Immutable snapshot instead of PostgreSQL projections

The guide proposes PostgreSQL for GUID lookup, hierarchy, metadata, and full-text projections. This MVP uses a bounded immutable snapshot of the current Git commit and batched Git object reads.

Why: at the demonstrated scale, a database would add synchronization, migrations, recovery, deployment, and failure modes before providing measured value. The snapshot reduced a 25-artifact list from about 1.17 seconds to roughly 0.08 milliseconds on the development machine; a single lookup is roughly 0.03 milliseconds. PostgreSQL becomes justified when repository size, query complexity, or measured latency exceeds this design.

### Git CLI and a worktree instead of full plumbing

The guide constructs trees and commits through plumbing and publishes with compare-and-swap. This implementation uses narrowly scoped Git CLI operations against a local worktree, repository-wide OS file locking, atomic file replacement, and commit-result verification.

Why: it is substantially smaller and easier to audit for a single local repository. It also retains ordinary Git inspectability. The tradeoff is explicit: direct Git changes must be committed before the server starts, and all online writers must use the service's locking protocol. Live external writers and crash-free temporary-index plumbing remain future work.

### Native JavaScript instead of TypeScript, Lit, and BlockSuite

The UI uses one native JavaScript module and Web Components without a build step. Structured document content is preserved and edited as JSON rather than presented as a WYSIWYG composition surface.

Why: the build-free UI proves the backend contract with almost no dependency or supply-chain surface. Lit and BlockSuite should be introduced only with the actual hierarchical document editor; adding them before that would provide framework cost without the promised editing capability.

### Canonical JSON instead of formatting-preserving JSON edits

The service writes deterministic indented JSON, but it does not preserve the original property order, indentation, or local whitespace of a manually edited artifact.

Why: formatting-preserving mutation requires an ordered concrete-syntax tree and significantly more code. Deterministic canonical output prevents random churn, but exact minimal-diff preservation is still a real gap relative to the guide.

### Fixed managed markers instead of pluggable indexers

The initial repository config records `artifact.json`, `.origoa`, and the foundation indexer, but the scanner itself recognizes the Foundation's fixed artifact, schema, and workflow paths.

Why: no extension indexer exists in the MVP. Pretending the scanner is dynamically extensible would create configuration that cannot be honored safely.

## What is not implemented

These are genuine gaps, not hidden claims:

- PostgreSQL projections, incremental replay, reindex phases, maintenance mode, full-text indexes, and deleted-artifact projection. Add them when measured repository/query load requires them.
- WYSIWYG hierarchical document composition and insertion of reusable entry references. This is the largest missing MVP user capability and the clearest reason to add BlockSuite.
- Move/rename APIs, bulk structural transactions, and automatic metadata relocation. GUID references remain stable, but structural maintenance is currently performed offline through Git.
- Automatic HID generation and historical HID aliases. Current HIDs are unique and editable, while Git retains their history; alias lookup is absent.
- Configured relationship source/target rules, cardinality, and presentation metadata. Links are validated for existence, not for domain-specific relationship policy.
- Field/document-range comment anchors and anchor maintenance. Artifact comments and threading primitives exist; rich editor anchoring does not.
- Extension hooks, user scripts, custom indexers, and UI extension loading. These were outside the guide's MVP and would require a sandbox and trust model.
- WebSocket presence, repository notifications, progress reporting, and collaborative editing. Optimistic concurrency is used instead.
- Built-in authentication, authorization, or TLS. The service defaults to localhost and must sit behind an authenticating TLS proxy before network exposure.
- Multi-branch, distributed, or multi-server operation. Online operation is one branch, one local Unix-style filesystem, and one shared advisory-lock domain.
- Safe coexistence with live external Git writers. Offline direct Git commits are supported and invalidate the cache; concurrent external commits do not participate in the service lock or validation transaction.
- Pagination for full artifact and tree listings. Search is capped, but list/tree responses are still whole-repository views; deployments approaching the configured repository limits need pagination before increasing those limits.
- End-to-end cancellation for repository reads. HTTP timeouts and Git subprocess deadlines are bounded, but public read methods currently use internal background contexts and do not stop all in-memory scanning immediately when a client disconnects.
- Lossless browser editing of integers beyond JavaScript's safe integer range. The Go API and repository preserve such JSON numbers exactly, but the dependency-free UI uses native `JSON.parse`; clients editing those values need an arbitrary-precision JSON strategy.

## What adversarial testing taught us

### A mutex inside one Go object is not repository safety

Separate repository handles and separate processes initially raced on Git's index and could both pass uniqueness checks. Writes and first-time initialization now use crash-released OS file locks tied to the shared repository.

Why it matters: the protected resource is the repository, not a Go instance or artifact GUID.

### A Git error does not prove a commit failed

Fault injection showed that Git can publish a commit and still return an error to the caller. Create, update, and delete now verify committed `HEAD` before deciding whether to roll back.

Why it matters: reporting failure after durable success encourages a retry, which can duplicate logical work or confuse clients.

### Fast reads need immutable ownership

The first implementation launched one Git process per artifact. Batching and commit-keyed caching removed that cost, but returning cached maps directly would have allowed callers to corrupt shared state or create races. Public results are cloned only when returned; internal validation reads immutable values.

Why it matters: performance and safety reinforce each other when the cache is immutable and ownership boundaries are explicit.

### Corrupt repositories must fail in bounded ways

Tests injected overlay cycles, oversized blobs, malformed stored JSON, hidden artifact paths, stale worktrees, missing workflows, invalid schema types, ambiguous commits, and filesystem-sync failures. Tree output, file count, blob size, total managed content, JSON depth, node count, request body size, and concurrent request work are bounded.

Why it matters: Git is authoritative, but authoritative input can still be malformed or hostile. Trusting Git does not mean trusting every blob.

### JSON numbers need exact representation

Default Go JSON decoding converts untyped numbers to `float64`, which silently changes integers above 2^53. Untyped values now use `json.Number` through HTTP decoding, storage decoding, cloning, patching, and validation.

Why it matters: an unrelated title edit must never rewrite business data.

## Readiness boundary

Within its declared scope, the repository is ready for a local or single-node Unix deployment behind TLS and authentication. CI is configured to verify formatting, static analysis, JavaScript syntax, race/shuffle tests, multi-scheduler stress, five fuzz targets, the optimized production build, and the container build.

Production operation still needs environment-specific backup, restore rehearsal, monitoring, log collection, proxy configuration, and capacity thresholds. If the deployment requires live external Git writers, Windows-native hosting, horizontal scaling, full document composition, or database-grade query behavior, those are architecture changes—not configuration switches.
