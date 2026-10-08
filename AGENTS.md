# AGENTS.md — operating guide for AI coding agents

This file is the contract between this repository and AI coding agents
(ZCode, Claude Code, Codex, Cursor, …). Read it before writing code. It
tells you how the project is shaped, which rules are load-bearing, and
which command does a task for you so you don't have to hand-copy code.

**Read first:** `docs/CONTRACT.md` (the frozen interfaces every
workstream codes against — change it there first, then in the code) and
`docs/FEATURES.md` (the Chinese functional breakdown: the five feature
domains, the resume semantics, and what the old Python version did).

## What this repo is

**DockerPull** — a Docker image puller with a CLI and a GUI. It pulls
images (multi-arch, multi-source, with registry auth) and exports
`docker load`-compatible `.tar` files for offline/air-gapped machines.

Still a Go backend and a Next.js frontend compiled into ONE static
binary (frontend is a `output: "export"` static build embedded via
`go:embed`). SQLite by default (pure-Go driver, no CGO), PostgreSQL
optional. Same-origin: no CORS, no API base URL config.

**No auth — this is a single-user local tool, by decision.** No login,
no onboard wizard, no sessions, no API keys, no roles, no `user_id`
columns, no ownership checks. Do not reintroduce a gate "for safety".

## Commands

| Task | Command |
|---|---|
| Backend build + vet | `go build ./... && go vet ./...` |
| Backend test | `go test ./...` (CI runs with `-race`) |
| E2E smoke (no build needed) | `scripts/smoke.ps1` (PowerShell; boots a throwaway server and drives the REAL route table: status → sources → task create → status → artifact reconcile → cleanup. Any onboard/items/authz legs still in the script are retired with auth.) |
| Windows build (no make) | `scripts/build.ps1 [-Desktop]` |
| Frontend build (static export) | `cd web && pnpm build` |
| Frontend lint | `cd web && pnpm lint` |
| Full single binary | `make build` (frontend → embed → go build) |
| Desktop app (Wails native window) | `make desktop` |
| Desktop startup tests (Windows) | `go test -tags desktop ./cmd/desktop/` (also run by `make test` on Windows and in CI) |
| Save a private-registry login | `dockerpull login <host> -u <user> -p <pass>` (or the `/credentials` page) |
| Dev with hot reload | `make dev-frontend` + `make dev` (browser on :8080) |
| Regenerate desktop icon resources | `make icons` (after re-rasterizing `web/src/app/icon.svg`) |
| Regenerate README screenshots | `pwsh -File scripts/screenshots.ps1 -BaseURL http://127.0.0.1:8080 [-Theme dark -Suffix -dark]` against a server whose data dir has real tasks/artifacts, or every page screenshots as an empty table |
| Generate an entity's full CRUD | `go run ./cmd/generator entity <name> --field title:string ...` |

The binary is **`dockerpull`** (`bin/dockerpull`, `bin/dockerpull.exe` on
Windows; `bin/dockerpull-desktop` for the Wails shell). `make build`,
`scripts/build.ps1`, the Dockerfile and the CI/release workflows all name it
that, so there is no rename step.

Definition of done for any code change: `go build ./...`, `go vet ./...`,
`go test ./...`, and `cd web && pnpm build` all pass. Frontend type errors
surface in `pnpm build` (no separate tsc step).

## Layout map

```
cmd/server/            entry point + CLI subcommands (serve|pull|search|tags|tasks|login|logout|credentials|version)
cmd/desktop/           Wails shell: ephemeral loopback port + native window (build tag desktop)
cmd/generator/         scaffolding CLI (init / entity) — templates in templates/
internal/app/          Boot: assembles config/store/hub/tasks/server for both delivery targets
internal/config/       env-only bootstrap config (APP_* variables)
internal/secrets/      sealed-at-rest secrets: DPAPI (Windows) / AES-256-GCM + key file
internal/registry/     image references, registry auth, manifests, arch resolution, search sources
internal/puller/       resumable blob download, digest verification, docker-load tar assembly
internal/tasks/        task lifecycle + progress fan-out onto the shared events hub
internal/store/        Store interface + sqlite/postgres drivers + migrations
internal/events/       in-process pub/sub hub (SSE fan-out)
internal/server/       HTTP layer: declarative routes in server.go, one handler file per domain
internal/server/dist/  build output for go:embed (never edit; make build-web fills it)
web/src/app/           Next.js App Router pages (/search /tasks /artifacts /credentials /settings), static export
web/src/components/ui/ shadcn-style primitives on @base-ui/react (NOT Radix)
web/src/lib/api.ts     central typed API client (apiFetch, envelope, subscribeEvents)
web/src/lib/api/<x>.ts per-entity generated clients
```

`internal/registry/`, `internal/puller/`, `internal/tasks/`, `internal/store/`
and `internal/server/` are all in place. `internal/auth/` and the scaffold's
example domains (`items`, `chat`, `files`, `analytics`, `onboard`, admin,
API keys) have been **deleted** along with the auth feature — do not look for
them and do not recreate them.

## Load-bearing conventions (break these and the design leaks)

1. **One response envelope.** Every JSON endpoint returns
   `{"ok": true, ...}` or `{"ok": false, "error": "..."}`. Use
   `writeOK`/`writeError`/`readJSON` in `internal/server/respond.go`.
2. **No auth, no ownership.** Routes are registered plainly on the
   declarative table in `internal/server/server.go` (one row per
   handler). There is no `s.public`/`s.protected`/`s.writable`/
   `s.adminOnly` wrapper, no `?actAs=`, no `user_id` column, and no
   ownership-404 rule — the tool is single-user and local. Never add an
   auth gate "just in case" and never reveal/hide a row based on who is
   asking: everything on this machine belongs to the same operator.
3. **Store: one interface, two dialects.** Domain files define a
   `<Name>Store` sub-interface embedded into `Store` (store.go) and
   DBStore methods. Write SQL as plain `?` literals and wrap with
   `d.rebind(...)` (rewrites to `$n` for postgres). Missing rows →
   `store.ErrNotFound`; unique violations → `store.ErrDuplicate`.
4. **Migrations are layered.** New tables: add `CREATE TABLE IF NOT
   EXISTS` to `migrationSQL()` in internal/store/db.go. Changing an
   EXISTING table: add a `tableHasColumn`-guarded imperative step (the
   existing guards in db.go are the template). Never destructive without
   a migration.
5. **Mutations publish events.** Handlers publish
   `task.updated` / `artifact.created` / `settings.changed` (see
   `docs/CONTRACT.md` §5) via `s.hub.Publish(events.Event{...})` — there
   is no per-user targeting any more. Pages subscribe with
   `subscribeEvents` and refresh. Never create a second hub.
6. **Frontend API calls go through apiFetch/apiJSON**
   (`web/src/lib/api.ts`) — same-origin, no auth header, no `?actAs=`
   mirroring. Never call bare `fetch`.
7. **base-ui, not Radix.** UI primitives take a `render={<Tag />}` prop
   for composition — there is NO `asChild`. Dialog labels must sit
   inside a `DropdownMenuGroup`/Menu.Group when used in menus.
8. **SVG/attributes don't do CSS `var()`.** Chart colors are resolved in
   JS from the theme tokens (`theme-provider.tsx`); never write
   `var(--x)` into an SVG attribute.
9. **Streaming must be UTF-8-safe end to end.** Network chunks are
   byte-oriented; a 3-byte CJK character split across reads renders as
   U+FFFD mojibake. Request bodies are validated as UTF-8 in readJSON
   (invalid bytes get a 400, never silent replacement), and every SSE
   frame written by hand must come from whole runes — build it from the
   JSON encoder, not from raw read buffers.
10. **Two delivery targets share one wiring.** `internal/app.Boot`
    assembles config/store/hub/tasks/server; `cmd/server` serves over
    TCP, `cmd/desktop` (build tag `desktop`) serves on an EPHEMERAL
    loopback port and points the Wails window at it. Streaming (SSE)
    must ride real TCP — WebView2 buffers custom-scheme responses, so do
    NOT "simplify" the desktop shell back to the asset-server fallback.
    Server changes must keep `server.BuildHandler` working.
11. **Brand icon** lives at `web/src/app/icon.svg` — it IS the favicon
    (App Router file convention) AND the in-app logo (the sidebar
    references `/icon.svg`). Desktop derives it, and the chain is:

    ```
    web/src/app/icon.svg          the artwork (hand-edited)
    build/appicon.svg             the same file, for the desktop pipeline
    scripts/render-icons.ps1      → build/appicon.png (1024) + icon-{16,32,48,256}.png
    python build/windows/mkico.py → build/windows/icon.ico
    go run github.com/tc-hib/go-winres@v0.3.3 make --in build/winres.json --out cmd/desktop/rsrc
                                  → cmd/desktop/rsrc_windows_{386,amd64}.syso
    ```

    So, after changing the SVG: `pwsh -File scripts/render-icons.ps1`,
    then the mkico + go-winres commands above (that pair is what `make icons`
    runs), then rebuild the desktop target. **`make` is not installed on every
    Windows box** — the three commands work on their own.

    The GROUP_ICON MUST stay at resource ID 3 — the Wails runtime loads the
    window icon from `winc.AppIconID = 3` (hardcoded).

    Verify without opening the app:
    `[System.Drawing.Icon]::ExtractAssociatedIcon("bin\dockerpull-desktop.exe")`
    returns the resource Windows will actually show.
12. **Two theme axes.** Mode (`.dark` class: light/dark/system) and
    accent preset (`data-theme` attribute: violet/ocean/forest/sunset/
    rose/candy/mono) are independent. Presets are MORE than color: they
    may override `--radius`, `--background`, `--card`, `--sidebar` for
    personality (mono = sharp corners, candy = extra-round pink, ocean =
    cool-tinted surfaces). Adding a preset = one `[data-theme]` CSS pair
    in globals.css + one entry in `THEME_PRESETS` (theme-provider.tsx).
    Both axes are applied pre-paint by the inline head script in
    layout.tsx — keep that script in sync with the storage keys.
13. **The server is the authority on filesystem paths.** There is no
    client auth guard any more. What the client CAN still get wrong is a
    path: every delete/download resolves the real path from the store
    row (`artifacts`, task `WorkDir`) and validates it against the
    configured directories — never `filepath.Join(userSuppliedString)`.

## Prefer the generator before hand-writing

```bash
go run ./cmd/generator entity note --field title:string --field body:text --field done:bool
```

This creates the store slice + migration DDL, handlers, routes, typed
API client, CRUD page, and nav entry in one shot, inserting code at the
`--- gen:* ---` markers (store.go, db.go, server.go,
web/src/components/app-shell.tsx).

Rules around the generator:

- **Never delete or reorder the `--- gen:* ---` marker lines.** Multiple
  entities accumulate at the same markers.
- Generated files are normal code — edit them freely AFTER generation.
  The generator only ever appends at markers and writes new files; it
  never regenerates over an edited file (it fails if the entity already
  exists).
- Field types: `string` (Input), `text` (Textarea), `bool` (toggle),
  `int` (number Input). `id`, timestamps, and live events are added
  automatically.
- Run `go build ./... && go vet ./... && cd web && pnpm build` after
  generating and before committing.

Manual recipes (what the generator automates, for when you need to go
beyond it): `docs/agent/add-entity.md`, `docs/agent/add-page.md`.

**The scaffold example domains (`items`, `chat`, `files`, `analytics`,
`onboard`, admin) are retired** along with auth — `docs/CONTRACT.md` §7
lists what goes away. The reference vertical to copy is now DockerPull's
own (`tasks`, `task_layers`, `sources`, `settings`, `artifacts`); if
those store files have not landed yet, read `docs/CONTRACT.md` §4 for the
row shapes and treat generator output as the shape template.

## Verification workflow (how to be trusted here)

1. After any change: `go test ./... -race`, `go vet ./...`,
   `cd web && pnpm build`. All four (with `go build ./...`) are the
   definition of done.
2. For behavior changes, extend `scripts/smoke.ps1` (status → sources →
   task create → status → artifacts → SSE → input validation) or add a
   handler test against the REAL route table — `internal/server/server_test.go`
   has the `newTestServer(t)` helper, which wires a temp SQLite store, a
   temp output dir and the real handler. `internal/registry/registrytest/`
   is the in-process fake registry those tests pull from.
3. **Adversarial re-review**: for non-trivial changes, run a SECOND
   review pass by a separate agent with the contract (AGENTS.md +
   `docs/CONTRACT.md`) as ground truth, not your diff as context. This
   workflow has caught real contract bugs that self-review missed — five
   during this rewrite, including a Basic-auth cache bug that made
   credential-protected registries un-pullable, an ARMv6-for-ARMv7
   architecture mix-up, and a path check that validated a path against
   its own parent directory and was therefore vacuous.
4. Never leave tool run-artifacts in the repo root (a stray `main.go`
   with an `//go:embed` breaks `go build ./...` — the root package must
   stay empty).

## Traps that have already bitten this codebase

Each of these was a real defect, not a hypothetical. Read them before
touching the same area.

- **A positive `bool` whose zero value disables something.** `VerifyTLS
  bool` meant "verify certificates", so the zero value turned verification
  *off* and `--no-verify-tls` was silently ignored. Flags that weaken a
  security property are negative (`SkipVerifyTLS`, `InsecureTLS`), so the
  zero value is always the safe one. Same reasoning for `MaxRetries`: 0
  means "do not retry", so "unset" is the named `MaxRetriesUnset = -1`.
- **Two columns for one fact.** `tasks.use_http` duplicated what
  `tasks.insecure` already meant and was never written. One column,
  documented, or the next reader has to guess which is authoritative.
- **Go's `flag` package stops at the first positional.** `tasks show <id>
  --json` silently dropped `--json` and `--data-dir`. Reorder with
  `splitFlags` before parsing (see `cmd/server/cli.go`), and cover it with
  a test — silent flag loss looks like "the feature doesn't work".
- **A range check against a directory derived from the checked path.**
  Allowing `filepath.Dir(art.Path)` as a root made every path valid. An
  allow-list must contain only directories the *application* chose.
- **`httptest.ResponseRecorder` is not concurrency-safe.** An SSE test has
  one goroutine writing frames and another watching for the first one; use
  the mutex-guarded `safeRecorder` in `server_test.go`.
- **`go:embed all:dist` needs a file to exist.** `internal/server/dist`
  must contain at least `.gitkeep` or `go build ./...` fails with
  "pattern all:dist: no matching files found".
- **`APP_BIND` defaults to loopback.** A container publishing port 8080
  answers nothing unless `APP_BIND=all`; the Dockerfile sets it.
- **Generator snippets must be gofmt'd after insertion.** The indentation
  that fits a marker line is not always the indentation the surrounding
  literal wants; `patch()` formats `.go` files for this reason.
- **A GUI binary has no console, so its failures are invisible.** The desktop
  target is linked `-H windowsgui`: `slog.Error` + `os.Exit(1)` writes the
  reason to a stdout handle that does not exist, and the user sees only
  "double-click does nothing". Anything that can fail before a window exists
  must report through a channel that survives: a log file in the data
  directory plus a native message box. `cmd/desktop` does both.
- **A `%APPDATA%` write is not a safe assumption — and there are TWO of them.**
  Hardcoding `os.UserConfigDir()` (== `%APPDATA%`) makes the app unredirectable
  when roaming is disabled, redirected to OneDrive, or blocked by EDR; there is
  no console to explain the refusal. WebView2 adds a second, independent
  `%APPDATA%\<exe>` requirement for its profile, so fixing only the database
  path still fails. `resolveDataDir` probes candidates by writing a real file
  (a directory you can create is not necessarily one you can write to), and
  `windows.Options.WebviewUserDataPath` puts WebView2 under the chosen data
  directory. Tests: `go test -tags desktop ./cmd/desktop/`.
- **`cmd/desktop` is behind a build tag, so plain `go build ./...` never
  touches it.** `go vet -tags desktop ./cmd/desktop/` is part of `make lint` and
  `make test` on Windows and runs in the CI `desktop` job. The `-tags desktop`
  tests need WebKit headers on Linux/macOS, so they are Windows-gated.
- **Reporting only the error is not enough; report the paths.** When the data
  directory itself is the problem, the user cannot guess where the app looked.
  The dialog and the log both list every rejected candidate with its reason,
  and the log records them at startup even when nothing fails.
- **The GUI/console distinction is a LINKER FLAG, not source.** Nothing in Go
  records that `cmd/desktop` must be built with `-H windowsgui`; a plain
  `go build ./cmd/desktop` produces a PE console-subsystem binary that opens a
  command-line window on every launch. `scripts/build.ps1`, `make desktop` and
  the CI `desktop` job all pass the flag AND then assert the artifact's PE
  subsystem is 2 (WINDOWS_GUI), because the build line is exactly what a new
  script forgets. If a user reports "a cmd window opens", check the artifact
  first: `[BitConverter]::ToUInt16(bytes, peOffset + 0x5C)`.
- **A spawned server must not inherit the caller's stdout.** A child that keeps
  a PIPE open after the parent script exits makes the CALLER block until its own
  timeout — the test looks hung although it finished. `scripts/smoke.ps1`
  redirects the server it starts to `<dataDir>/server.out.log` /
  `server.err.log`; the bonus is that a failed startup now shows the server's own
  reason instead of only "did not become ready".
- **Secrets must be write-only across every boundary.** Stored registry
  credentials are sealed by `internal/secrets` (DPAPI on Windows) and exposed
  only as `tasks.CredentialView`, which has NO secret field — so no handler can
  leak one by forgetting a strip, and `--json`, logs and events carry nothing.
  Never add a `Secret` field to a response type, never log a credential, and
  remember `FindCredentialByHost` is an EXACT match: normalise with
  `registry.NormalizeAuthHost` before storing or looking up, or
  `docker.io`/`registry-1.docker.io` become separate logins that both miss.
- **Rasterizing an SVG on Windows has four traps, all now in
  `scripts/render-icons.ps1`.** An SVG loaded directly renders at its intrinsic
  size in the corner, so it is inlined in a full-viewport page; without
  `--default-background-color=00000000` the screenshot is opaque white; the
  browser EXITS BEFORE ITS BYTES LAND, so the PNG header must be validated
  (retrying) rather than trusting the file length; and piping the browser's
  stdout makes it inherit a pipe and take no screenshot at all. Also: small
  `--window-size` values do NOT give small renders (Windows clamps the window,
  so the screenshot is a cropped zoom of the artwork) — render one 1024 master
  and resample. PowerShell's `-shl` on a `[byte]` returns 0, so parse PNG
  dimensions with plain arithmetic.
- **`build/windows/mkico.py` used to look in the wrong two directories.** It
  opened its inputs next to itself (`build/windows/`) while the render step
  writes them to `build/`, and it wrote its output to
  `build/windows/windows/icon.ico`. It therefore always failed, which made
  `make icons` — and `make desktop`, which depends on it — fail too, and left
  `build/windows/icon.ico` stale since the template was generated. Both paths
  are fixed; keep them aligned with `scripts/render-icons.ps1`.
- **A search result is a NAME, not a location.** Searching "nginx" on the 1ms
  accelerator returns `library/nginx` — a Docker Hub repository name. A pull that
  parses it bare reaches `registry-1.docker.io`, i.e. exactly the host the user
  chose 1ms to avoid, and on a firewalled network it fails with a connection
  error naming the wrong registry. The search source must travel with the result
  into the pull: `GET /api/sources?kind=search` reports a `pullHost` per source
  (`registry.PullHostForSearchSource`), the GUI passes it as `registry` to
  inspect/tags/createTask, and the CLI appends `--mirror`. Docker Hub's own entry
  reports NO host on purpose, so the operator's configured default accelerator
  still wins.
- **The download source is chosen BEFORE tag/arch, and it is not always
  docker.io.** The tag list and the architecture list are both read from the
  chosen registry's manifest, so the registry has to be fixed first — a tag on
  quay.io need not exist on docker.io, and changing the source must re-resolve
  them (the GUI does). `docker.io`/`quay.io`/`ghcr.io`/`registry.k8s.io` are
  independent upstream registries with their own namespaces, unlike
  `BuiltinMirrors`, which only proxy Docker Hub's. The initial selection comes
  from `defaultPullHost` in `GET /api/registries`, which the server resolves from
  the `default_mirror` setting (an id like `nju`, a host, or nothing) — do not
  guess it client-side, and never assume docker.io.
- **A foreign layer is not in the registry; it lives at its `urls`.** A Windows
  base image declares its base layers as
  `application/vnd.docker.image.rootfs.foreign.diff.tar.gzip` with a `urls` array
  (`hello-world:nanoserver1709` → `go.microsoft.com/fwlink/?linkid=860906` and
  `…863171`). Three things were wrong at once and all three are fixed: the
  `urls` were dropped at JSON-parse time (`Descriptor` had no field for them),
  the puller only ever called `client.OpenBlob(ctx, ref, digest, offset)` so a
  foreign layer was requested from `/v2/<repo>/blobs/<digest>` and answered
  **403 AccessDenied**, and the size fallback HEADed the registry — pointless for
  a blob the registry never had. Now `Descriptor.URLs` + `IsForeign()` exist,
  `job.openRange` routes a foreign layer to `Client.OpenBlobURLRange`, and the
  bytes are still digest-verified by `seal`, so a CDN cannot substitute content.
  **`OpenBlobURLRange` must never send the registry's Authorization header** —
  that would leak a credential to an unrelated host and break presigned URLs,
  which reject an unexpected header. Verified end to end: the windows/amd64 pull
  of that tag now fetches 17.2MB + 77.3MB from Microsoft's CDN, produces a
  242.9MB tar, and `docker load` reports `os=windows arch=amd64 layers=4`.
- **A bearer challenge's `scope` can be a TEMPLATE, so never use it verbatim.**
  ghcr.io answers the plain `/v2/` ping with
  `scope="repository:user/image:pull"`, and asking the token service for that
  requests a token to the repository `user/image`. GitHub answers **403 DENIED**,
  not 401 — so it does not even read as an auth problem: `ghcr.io/…` failed in
  this tool while `docker manifest inspect` fetched the same image fine. Docker
  Hub and Quay send no scope on the ping, which is why only GHCR broke. The scope
  must always name the repository being fetched (`tokenScope`); the challenge's
  is used only when there is no repository to name. The error message names the
  token URL, which is what made this findable.
- **A tag list may only be answered by a searcher in the SAME namespace.** The
  fallback exists for registries with no `tags/list`, but the Docker Hub API
  describing a ghcr.io repository returns a *different list* — ghcr's
  `linuxcontainers/alpine` has 38 tags and Docker Hub's has 42 — so the fallback
  offered tags the pull could not have. `registry.SearcherServesRegistry` gates
  it: the pinned host must match, or both sides must be Docker Hub-family (Hub
  itself and its accelerators, which do share the namespace). The GUI no longer
  keeps a second copy of this rule — `inspect` returns the tags and the client
  uses them as-is.
- **A tag list must come from the registry you will pull from.** The inspect
  handler asked a SEARCHER first, and a searcher is frequently a different
  registry's API (Docker Hub's while pulling from ghcr.io), so the tag dropdown
  could offer another publisher's tags for the same-looking `owner/repo` name.
  `puller.Inspect` now carries `Tags`/`TagsFrom` from `Client.ListTags`
  (per-registry `/v2/<repo>/tags/list`) and the searcher is only a fallback for a
  registry with no such endpoint. `dockerpull tags` follows the same order.
- **`tags/list` is lexicographic AND paginated, so `latest` can be absent.**
  `library/nginx` has thousands of tags; no 200-entry window starting at "1"
  contains `latest`, so a picker that took the page as-is opened on "1" and did
  not offer the most-pulled tag at all. `Client.ensureDefaultTags` probes
  (one HEAD each) for the tag the caller asked for and for `latest`, puts what
  exists at the front, and leaves the rest in the registry's own order.
- **Inspect and pull must agree on the transport.** `inspectRequest` has its own
  `Insecure` flag, mirroring `taskRequest`: without it a private registry on
  `http://` was inspectable from the CLI (`pull --list-arch --insecure`) but never
  from the GUI, so the tag/arch picker died with "server gave HTTP response to
  HTTPS client" for exactly the registries that need the flag. A transport flag
  that reaches only one of the two steps shows a picker the download cannot
  follow (or vice versa) — the GUI sends it to both.
- **An index may offer only a non-Linux platform, and the tool must say so.**
  `Manifest.Platforms()` used to drop every non-linux entry, so a windows-only
  index reported NO platforms: the picker showed nothing, `--list-arch` claimed
  "单架构镜像", and a failed pull printed "可用架构:" followed by nothing.
  Reporting is now truthful (`Platforms()` lists everything an index declares)
  and the OS guard lives in `ResolveArch`, which MUST refuse a cross-OS match —
  a linux/amd64 request served a windows/amd64 image yields a tar that cannot
  load on the host it was fetched for, and nothing downstream notices. Keep
  those two responsibilities separate, and keep the test that pins a
  windows-only index (manifest_test.go).
- **One knob, every spelling.** `--mirror` and the API's `registry` field are the
  same setting, and users reach for whichever name they saw: an accelerator id
  ("1ms"), its host ("docker.1ms.run"), an UPSTREAM-registry id ("mcr", "quay"), a
  display name ("MCR (Microsoft)", "南大镜像源"), or a private host. Resolving only
  mirror ids meant `--mirror mcr` fell through to DNS and failed with "lookup mcr:
  no such host". `registry.PullHost` is now the single resolver and the settings
  validator (`isMirrorIDOrHost`) calls it too, so validation and resolution cannot
  drift apart; anything that is neither a known source nor host-shaped is refused
  (CLI exit 2, API 400) rather than dialled.
- **MCR is searchable only because its catalog is.** It has no keyword API (its
  own `/api/v1/catalog/all` answers 400), but `/v2/_catalog` is anonymous, returns
  everything (3861 repositories, ~160 KB) and IGNORES `n`/`last`/`query`/`search` —
  so `MCRSearcher` fetches once, caches for 15 minutes, sorts (stable paging) and
  filters locally. Every other upstream registry has no anonymous search OR
  catalog: ghcr.io `/v2/_catalog` → 401, registry.k8s.io → 404, public.ecr.aws and
  nvcr.io → 401. Those are reachable only by typing a full reference, which is why
  the UI says so instead of offering a search box that cannot work.
- **A PowerShell `Task` awaiter leaks a `VoidTaskResult` into the pipeline.**
  `scripts/screenshots.ps1` drives Chrome over CDP with `ClientWebSocket`, and
  `$task.GetAwaiter().GetResult()` on a NON-generic Task returns a
  `VoidTaskResult` VALUE that PowerShell emits. A helper ending in that call then
  returns `[VoidTaskResult, ClientWebSocket]`, and the caller fails with
  `VoidTaskResult does not contain a method named SendAsync` — an error that
  points at the wrong line entirely. Every such call needs `$null =` in front.
  (Chaining `.GetAwaiter().GetResult()` straight onto a multi-line call is also
  worth avoiding for the same class of confusion.)
- **Screenshots must wait for the DATA, not for the load event.** `chrome
  --screenshot` fires as soon as the document loads, and this UI fetches
  everything afterwards — so a plain capture documents skeleton placeholders.
  `scripts/screenshots.ps1` drives CDP instead: it polls a DOM condition, can
  click (the tag/arch panel only exists after selecting a result), can scroll
  (steps 2 and 3 sit below a 20-row results table), and seeds the theme in
  localStorage via `Page.addScriptToEvaluateOnNewDocument` so it is applied
  pre-paint. `Page.captureScreenshot` returns the bytes, which also side-steps
  the "Chrome exits before its bytes land" trap entirely. Its demo data dir
  (`docs/screenshots/.data/`) is gitignored and must stay that way: it contains
  real downloaded blobs.
- **A deep link is only as good as the id it carries, and it must be resolved
  before anything else runs.** `/settings` links to `/search?source=<searchId>`
  (the BUILT-IN id, "mcr"), while the page matched stored ROW ids (random hex), so
  the link silently searched the default source with the picker showing the
  linked one. `GET /api/sources` now reports `searchId` alongside the row id and
  the page matches either — and it reads the hint while loading the source list,
  because choosing a default first let the auto-search fire against it before the
  hint landed. Reported as "picker says 1ms, error says hub.docker.com".
- **Opening a local folder has to be a server action, so it needs the path
  guard.** A browser cannot open a directory and a `file://` link is blocked, so
  `POST /api/artifacts/{id}/reveal` spawns the file manager. That makes the server
  the thing that decides which path is opened: it must go through
  `lookupArtifact` (id → row → `ValidateArtifactPath` against the directories the
  APPLICATION chose), exactly like download and delete. Without that guard the
  endpoint would be "run the file manager on any path the caller names". Use
  `Start` + a goroutine `Wait`, never `Run`: a file manager outlives the request.
- **A mirror is not a registry.** `registry.BuiltinMirrors` are Docker Hub
  accelerators: single-segment repositories there mean `library/<name>`.
  `registry.BuiltinRegistries` (ghcr.io, quay.io, …) have their own namespaces,
  so `NeedsLibraryPrefix` must stay false for them. `BuiltinRegistries` is
  served read-only by `GET /api/registries` rather than seeded as `sources`
  rows: seeding would duplicate the list, and a user could "delete" a built-in
  only to have it reappear on the next boot.

## Reference examples to read before similar work

`internal/registry/` and `internal/puller/` have landed;
`internal/tasks/` is still being written. Read the engine package itself
first — the scaffold analogues below are for shape only, and the ones
marked retired are on their way out.

| Task | Read first |
|---|---|
| Registry client: references, auth, manifests, arch, search sources | `internal/registry/` — already on disk (`ref.go`, `auth.go`, `manifest.go`, `platform.go`, `search.go`, `client.go`) |
| Resumable download + `docker load` tar assembly | `internal/puller/` (`blob.go` for byte/Range handling, `state.go` for the resume ledger, `tar.go` for the tar layout) |
| Task lifecycle + SSE fan-out | Target: `internal/tasks/` + CONTRACT §5. Read `internal/events/hub.go` (the one hub) and `internal/server/events.go` (SSE transport). |
| Store entity (store slice + handlers + typed client + page) | `internal/store/tasks.go` (the live shape) or `internal/store/items.go` + `internal/server/handlers_items.go` (the scaffold vertical, **retired with the scaffold domains** but still the exact shape to copy). The page shape lives at `web/src/app/tasks/page.tsx`. |
| Aggregation/chart endpoint | `internal/server/handlers_stats.go` (becomes `/api/stats`) |
| File upload / artifact download | `internal/server/handlers_files.go` (multipart/streaming shape, retired) + `web/src/app/artifacts/page.tsx` (the live artifact UI) |

## Environment & runtime facts

- Config is env-only (`APP_*`), no config file; `.env` is a dev
  convenience and real environment variables win.
- Default port 8080; health probes `/healthz` `/livez` `/readyz`.
- Data lives under `APP_DATA_DIR` (default `./data`): `app.db` (SQLite,
  WAL) plus the default output dir `<APP_DATA_DIR>/downloads`.
- **The desktop target does not default to `./data`.** Because a GUI app has
  no meaningful working directory, `cmd/desktop` resolves one by probing for a
  writable location, in order: `APP_DATA_DIR` → `%APPDATA%\DockerPull` →
  `%LOCALAPPDATA%\DockerPull` → `<exe dir>\data`. It also puts WebView2's
  profile at `<dataDir>\webview2`, and logs to
  `<dataDir>\dockerpull-desktop.log`. Setting `APP_DATA_DIR` always wins.
- Registry credentials come from `-u/-p` per invocation OR from the stored
  logins (`login` / the `/credentials` page). A stored secret is sealed by
  `internal/secrets` and is write-only through the API. Proxy settings come
  from `settings` (`proxy_mode`, `proxy_url`) or the environment.
- The `credentials` table stores CIPHERTEXT in `secret`. On Windows that is a
  DPAPI blob tied to the current user account, so copying `app.db` to another
  machine or account makes it unreadable (reported as 无法解密, never as an
  empty password).
- `verify_tls` defaults to **true** — puller/registry defaults are
  per-call values, so a new code path must pass the setting through
  rather than relying on a zero value.
- `APP_DEV_PROXY=http://localhost:3000` turns the Go server into a dev
  proxy for `next dev` (HMR websocket included) — the browser talks to
  :8080 only.
