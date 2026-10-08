# Recipe: add an entity (full CRUD vertical)

> The generator automates everything in this document:
>
> ```bash
> go run ./cmd/generator entity note --field title:string --field body:text --field done:bool
> ```
>
> Use the manual recipe when you need something the generator doesn't emit
> (extra endpoints, custom queries, computed fields, different UI).

Goal: a new domain object with list/create/read/update/delete over HTTP,
live SSE updates, and a table UI. Example entity name: `note`, fields:
title (string), body (text), done (bool).

**DockerPull is a single-user local tool: there is no ownership and no
auth.** A row belongs to whoever is sitting at the machine, so generated
tables have no `user_id` column, handlers do no ownership check, and routes
are registered plainly (AGENTS.md convention 2). The reference vertical to
read first is `internal/store/tasks.go` + `internal/server/handlers_tasks.go`
+ `web/src/app/tasks/page.tsx`.

## 1. Store slice — `internal/store/notes.go`

Copy the shape of `internal/store/tasks.go`:

- Record struct: `ID`, your fields, `CreatedAt`, `UpdatedAt`. **No
  `UserID`.**
- A `NoteStore` sub-interface with Create/Get/List/Update/Delete, added to
  the `Store` interface in `store.go` (generated code embeds it at the
  `--- gen:store-interfaces ---` marker).
- DBStore methods with plain `?` SQL wrapped in `d.rebind(...)`.
- `Create*` fills `CreatedAt`/`UpdatedAt` when the caller left them zero;
  `Update*` refreshes only `UpdatedAt` and returns `store.ErrNotFound` when
  no row matched (a silent success hides a deleted row from a worker).
- `List*` takes no owner argument and orders by `created_at DESC`.

## 2. Schema — `internal/store/db.go`

Add to `migrationSQL()` (fresh installs), at the `--- gen:migrations ---`
marker:

```go
`CREATE TABLE IF NOT EXISTS notes (
    id         TEXT PRIMARY KEY,
    title      TEXT NOT NULL DEFAULT '',
    done       INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
)`,
```

Column type map: string/text → `TEXT NOT NULL DEFAULT ''`, bool/int →
`INTEGER NOT NULL DEFAULT 0`, timestamps stay `TIMESTAMP`. Booleans are
converted at the store boundary with `boolToInt`/`intToBool`.

**Changing an EXISTING table** is layer 2: add a `tableHasColumn`-guarded
`migrate*` step and call it from `Migrate()`. See that function's doc comment
for the recipe. Never alter an existing table destructively.

## 3. Handlers — `internal/server/handlers_notes.go`

Copy `handlers_tasks.go` (or the simpler `handlers_settings.go`):

- `noteDTO` with json camelCase tags — the DTO is the only wire shape; never
  expose `store.Note` directly.
- `handleListNotes`: `writeOK(w, map[string]any{"notes": out})` — always an
  array, never `null`.
- `handleCreateNote`: `randomID("note_")` for the id, map `ErrDuplicate` to
  409, publish `note.created` via `s.hub.Publish(...)`.
- `handleGetNote` / `handleUpdateNote` / `handleDeleteNote`: load by
  `r.PathValue("id")`; a missing row goes through `storeError` → 404.
- `handleUpdateNote`: pointer fields so "field omitted" differs from "field
  set to zero".
- There is **no** `loadOwned*` helper and **no** auth gate to add.

## 4. Routes — `internal/server/routes.go`

Append rows to the declarative table (generated code uses the
`--- gen:routes ---` marker):

```go
{http.MethodGet, "/api/notes", s.handleListNotes},
{http.MethodPost, "/api/notes", s.handleCreateNote},
{http.MethodGet, "/api/notes/{id}", s.handleGetNote},
{http.MethodPut, "/api/notes/{id}", s.handleUpdateNote},
{http.MethodDelete, "/api/notes/{id}", s.handleDeleteNote},
```

There is no auth level to choose: the `route` struct is just
method/pattern/handler. Row-level verbs must name their row with `{id}`
(`routes_test.go` enforces this, with a documented exception list for
collection-level mutations such as `PUT /api/settings`).

## 5. Frontend API client — `web/src/lib/api/notes.ts`

Copy the generated `api.ts.tmpl` shape: a `Note` interface (camelCase) plus
`listNotes/getNote/createNote/updateNote/deleteNote` built on `apiJSON` /
`jsonInit` imported from `../api`. Never call bare `fetch`.

## 6. Page — `web/src/app/notes/page.tsx`

Copy `web/src/app/tasks/page.tsx`: `"use client"`, a table plus a
create/edit dialog and a delete confirm, and live updates via
`subscribeEvents({ onAny: (type) => ... })` filtered on
`type.startsWith("note.")`.

Two gotchas this codebase has already hit:

- `subscribeEvents` takes a **handler object** (`{onTask, onProgress,
  onArtifact, onAny, onStatus}`), not a single callback.
- Wrap an effect-initiated fetch in an async IIFE —
  `useEffect(() => { (async () => { await reload(); })(); }, [reload])` —
  because `eslint-plugin-react-hooks` (via `eslint-config-next` 16) errors on
  the `useEffect(() => reload(), [reload])` idiom.

## 7. Navigation — `web/src/components/app-shell.tsx`

Add to the `NAV` Main section (generated code uses the `--- gen:nav ---`
marker) plus the lucide icon import at `--- gen:nav-icons ---`. Never delete
or reorder those marker lines.

## 8. Verify

```bash
go build ./... && go vet ./... && go test ./...
cd web && pnpm build
powershell -File scripts\smoke.ps1    # drives the REAL route table
make build && ./bin/dockerpull        # exercise the flow in the browser
```

Extend `internal/server/server_test.go` with the new routes and, for
behaviour changes, `scripts/smoke.ps1`.
