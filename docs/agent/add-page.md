# Recipe: add a page (frontend)

Static-export constraints shape everything here: pages are client components,
there is no SSR, no `next/link` middleware, no API routes, and routing is
file-based under `web/src/app` with `trailingSlash: true` (link to
`/tasks/`, not `/tasks`).

## Simple page

1. Create `web/src/app/<route>/page.tsx` starting with `"use client"`.
2. Data access goes through `@/lib/api` helpers (`apiJSON`, or a typed
   function added next to the others). There is **no auth and no session** —
   the UI never has to ask who is signed in, because nobody is: DockerPull is
   a single-user local tool. `AuthGuard`, `useUser`, `isAdmin` and
   `user-context` no longer exist; do not reintroduce them.
3. Fetch inside an async IIFE in `useEffect`:
   `useEffect(() => { (async () => { await reload(); })(); }, [reload])`.
   The `useEffect(() => reload(), [reload])` idiom fails `pnpm lint` —
   `eslint-plugin-react-hooks` 7 (via `eslint-config-next` 16) reports
   `set-state-in-effect` on a direct call to any function that calls
   `setState`.
4. Show a `Skeleton` while the first fetch is in flight rather than
   rendering an empty table.
5. Register it in `NAV` inside `web/src/components/app-shell.tsx`, and keep
   both `--- gen:nav ---` / `--- gen:nav-icons ---` marker lines intact.

## Realtime data

```ts
const close = subscribeEvents({
  onTask: (t) => ...,        // full task after create/update
  onProgress: (p) => ...,    // byte counters, high frequency
  onArtifact: (a) => ...,    // a new .tar landed
  onAny: (type, data) => ...,// everything, including ping/hello
  onStatus: (s) => ...,      // "open" | "closed"
});
return close;
```

`subscribeEvents` takes a **handler object**, not a single callback, and
returns an unsubscribe function — call it in the `useEffect` cleanup. Event
types are `task.created|updated|progress|finished`,
`artifact.created|deleted`, `source.changed`, `settings.changed`, plus
`hello`/`ping` keep-alives (ignore them). Progress is high frequency, so
patch state in place instead of refetching on every frame.

## UI primitives

- `@/components/ui/*` are shadcn-style components on **@base-ui/react** (not
  Radix). Composition uses the `render` prop:
  `<Button render={<Link href="/x/" />}>Label</Button>` — there is no
  `asChild`. A label inside a menu must sit in a `DropdownMenuGroup`.
- Design tokens: use semantic Tailwind classes (`bg-background`,
  `text-muted-foreground`, `border-border`); the palette lives in
  `web/src/app/globals.css` (`:root` + `.dark`).
- Theme is two independent axes: `.dark` (mode) and `data-theme` (accent
  preset). Read them through `useTheme()` from
  `@/components/theme-provider`; never hard-code a color. SVG attributes
  cannot use CSS `var()` — resolve a token in JS first.
- There is no progress-bar primitive: the existing pages hand-roll
  `div`-based bars. Reuse the helpers in `web/src/lib/format.ts`
  (`formatBytes`, `formatSpeed`, `percent`, `platformString`, status labels)
  and add a new formatter there rather than duplicating one.

## Files and downloads

Artifacts are served by the Go process at `/api/artifacts/{id}` — link to it
with a plain `<a href={artifactURL(id)}>`; the browser handles the download.
Every path is resolved server-side from the store row, so the client never
sends a filesystem path.

## Streaming UI

The markdown/streaming stack was removed with the scaffold's chat page: there
is no `chat` route, no `streamdown`, and no `MARKDOWN_FULL` build option. If
you need streaming again, add an SSE reader that decodes with
`TextDecoder(value, { stream: true })` plus a final flush, and keep every
frame whole-rune — a multi-byte CJK character split across reads renders as
U+FFFD (AGENTS.md convention 9).
