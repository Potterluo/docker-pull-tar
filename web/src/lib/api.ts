// api.ts — the single place the frontend talks to the backend.
//
// Layers, top to bottom:
//
//   1. Types mirroring the Go DTOs (docs/CONTRACT.md §6/§7)
//   2. apiFetch — thin same-origin wrapper (no auth: this is a local tool)
//   3. Typed endpoint functions (status, search, tasks, artifacts, sources,
//      settings, stats)
//   4. Realtime helper (EventSource subscription to /api/events)
//
// Convention: every JSON endpoint returns the parsed JSON envelope
// ({ok, error?, ...}) — callers branch on `ok`, never on HTTP codes.

// --- 1. Types ---------------------------------------------------------------

export interface Envelope {
  ok: boolean;
  error?: string;
}

export interface Ref {
  registry: string;
  repository: string;
  tag: string;
  digest: string;
  useHTTP: boolean;
}

export interface Platform {
  os: string;
  architecture: string;
  variant?: string;
}

export interface SearchResult {
  source: string;
  name: string;
  repository: string;
  image: string;
  description: string;
  stars: number;
  pulls: number;
  updated: string;
  official: boolean;
}

export interface SearchPage {
  results: SearchResult[];
  total: number;
  page: number;
  pageSize: number;
}

export interface TagInfo {
  name: string;
  platforms?: Platform[];
  size?: number;
  updated?: string;
}

export interface InspectResult {
  ref: Ref;
  mediaType: string;
  isIndex: boolean;
  platforms: Platform[];
  actual?: Platform;
  tags: string[];
  /**
   * Where the tag list came from: the registry host being inspected, or a
   * searcher id when the registry has no tags/list endpoint. A searcher can be a
   * different registry's API, so the UI labels a fallback rather than
   * presenting it as authoritative.
   */
  tagsFrom?: string;
  totalBytes: number;
  layerCount: number;
}

export interface TaskLayer {
  id: string;
  taskId: string;
  digest: string;
  kind: string;
  name: string;
  position: number;
  size: number;
  downloaded: number;
  status: string;
  error: string;
}

export interface Task {
  id: string;
  ref: string;
  registry: string;
  repository: string;
  tag: string;
  digest: string;
  platform: string;
  status: string;
  error: string;
  totalBytes: number;
  downloadedBytes: number;
  speed: number;
  workDir: string;
  tarPath: string;
  tarSize: number;
  workers: number;
  insecure: boolean;
  verifyTls: boolean;
  createdAt: string;
  startedAt: string;
  updatedAt: string;
  finishedAt: string;
  layers: TaskLayer[];
}

// TaskProgress is the coalesced payload of a `task.progress` event
// (bytes only — the full Task arrives via `task.updated`).
export interface TaskProgress {
  id: string;
  downloadedBytes: number;
  totalBytes: number;
  speed: number;
  layers?: { digest: string; downloaded: number; status: string }[];
}

// TaskFinished is the payload of a `task.finished` event.
export interface TaskFinished {
  id: string;
  status: string;
  tarPath?: string;
  error?: string;
}

export interface Artifact {
  id: string;
  name: string;
  path: string;
  repository: string;
  tag: string;
  platform: string;
  size: number;
  taskId: string;
  createdAt: string;
}

export interface Source {
  id: string;
  kind: string; // "search" | "mirror"
  name: string;
  url: string; // search sources
  host: string; // mirror sources
  enabled: boolean;
  priority: number;
  isDefault: boolean;
  createdAt: string;
  /**
   * For a SEARCH source: the registry host its results must be pulled from.
   *
   * A search result is a NAME, not a location — searching "nginx" on the 1ms
   * accelerator returns "library/nginx", which parsed bare reaches
   * registry-1.docker.io, i.e. exactly the host the user chose 1ms to avoid.
   * Empty means "use the configured default mirror".
   */
  pullHost?: string;
}

/**
 * A public registry the tool can pull from directly (ghcr.io, quay.io, …).
 *
 * Catalog data, not a stored row: it comes from the binary's own
 * `registry.BuiltinRegistries`, so it can never drift from what the code
 * supports. `searchId` is present only when a keyword Searcher exists — GHCR
 * publishes no search API, so it is reached by owner/repo instead.
 */
export interface BuiltinRegistry {
  id: string;
  name: string;
  host: string;
  searchId?: string;
  note?: string;
}

/**
 * A stored registry login. There is deliberately NO secret field: a saved
 * password is write-only through the API and can never be read back, so the
 * browser never holds a copy of it.
 */
export interface Credential {
  id: string;
  host: string;
  username: string;
  kind: string; // "basic" | "token"
  note: string;
  hasSecret: boolean;
  createdAt: string;
  updatedAt: string;
}

/** Setting rows are a flat string → string map (bulk PUT accepts a patch). */
export interface Settings {
  [key: string]: string;
}

export interface Stats {
  tasksTotal: number;
  tasksRunning: number;
  tasksSucceeded: number;
  tasksFailed: number;
  bytesTotal: number;
  artifactsTotal: number;
  artifactsBytes: number;
}

export interface Status {
  version: string;
  configured: boolean;
  dataDir: string;
  outputDir: string;
  platform: string;
  auth: boolean;
}

// --- 2. apiFetch ------------------------------------------------------------

// apiFetch is deliberately thin: the frontend is served same-origin by the
// Go binary (no CORS, no base URL, no bearer token, no actAs mirroring —
// there is no auth in this product).
export async function apiFetch(url: string, init?: RequestInit): Promise<Response> {
  return fetch(url, { credentials: "same-origin", ...init });
}

// apiJSON: apiFetch + parsed envelope. Covers 90% of calls.
export async function apiJSON<T extends Envelope>(
  url: string,
  init?: RequestInit
): Promise<T> {
  const res = await apiFetch(url, init);
  return res.json() as Promise<T>;
}

// jsonInit builds a JSON request init for the mutating helpers below.
export function jsonInit(method: string, body: unknown): RequestInit {
  return {
    method,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  };
}

// --- 3. Endpoints -----------------------------------------------------------

// Server / dashboard

export async function getStatus(): Promise<Status & Envelope> {
  return apiJSON("/api/status");
}

export async function getStats(): Promise<Stats & Envelope> {
  return apiJSON("/api/stats");
}

// Search (F1)

export async function searchImages(
  q: string,
  source: string,
  page: number,
  pageSize = 20
): Promise<SearchPage & Envelope> {
  const params = new URLSearchParams({
    q,
    source,
    page: String(page),
    pageSize: String(pageSize),
  });
  return apiJSON(`/api/search?${params.toString()}`);
}

export async function getTags(
  repository: string,
  source: string
): Promise<{ tags: TagInfo[] } & Envelope> {
  const params = new URLSearchParams({ repository, source });
  return apiJSON(`/api/tags?${params.toString()}`);
}

export async function inspectImage(
  image: string,
  registry?: string,
  platform?: string,
  insecure?: boolean
): Promise<InspectResult & Envelope> {
  return apiJSON(
    "/api/images/inspect",
    jsonInit("POST", { image, registry, platform, insecure })
  );
}

// Tasks (F2/F3/F4)

export async function listTasks(): Promise<{ tasks: Task[] } & Envelope> {
  return apiJSON("/api/tasks");
}

export async function getTask(id: string): Promise<{ task: Task } & Envelope> {
  return apiJSON(`/api/tasks/${id}`);
}

export async function createTask(body: {
  image: string;
  registry?: string;
  platform?: string;
  workers?: number;
  insecure?: boolean;
  verifyTls?: boolean;
  outputDir?: string;
}): Promise<{ task: Task } & Envelope> {
  return apiJSON("/api/tasks", jsonInit("POST", body));
}

export async function pauseTask(id: string): Promise<Envelope> {
  return apiJSON(`/api/tasks/${id}/pause`, { method: "POST" });
}

export async function resumeTask(id: string): Promise<Envelope> {
  return apiJSON(`/api/tasks/${id}/resume`, { method: "POST" });
}

export async function cancelTask(id: string): Promise<Envelope> {
  return apiJSON(`/api/tasks/${id}/cancel`, { method: "POST" });
}

export async function retryTask(id: string): Promise<Envelope> {
  return apiJSON(`/api/tasks/${id}/retry`, { method: "POST" });
}

export async function deleteTask(id: string, files = false): Promise<Envelope> {
  return apiJSON(`/api/tasks/${id}${files ? "?files=true" : ""}`, { method: "DELETE" });
}

// Artifacts (F5)

export async function listArtifacts(): Promise<{ artifacts: Artifact[] } & Envelope> {
  return apiJSON("/api/artifacts");
}

/**
 * Opens the artifact's folder in the OS file manager, selecting the file.
 *
 * Server-side by necessity: a browser cannot open a local folder. The client
 * sends an ID and the server resolves + validates the path itself.
 */
export async function revealArtifact(id: string): Promise<Envelope> {
  // No body: same shape as pause/resume (a bodyless action).
  return apiJSON(`/api/artifacts/${id}/reveal`, { method: "POST" });
}

export async function deleteArtifact(id: string): Promise<Envelope> {
  return apiJSON(`/api/artifacts/${id}`, { method: "DELETE" });
}

// artifactURL is the direct browser download of a finished .tar. A plain
// <a href> works — no headers, no auth.
export function artifactURL(id: string): string {
  return `/api/artifacts/${id}`;
}

// Sources (search backends + registry mirrors)

export async function listCredentials(): Promise<
  { credentials: Credential[]; protection: string } & Envelope
> {
  return apiJSON("/api/credentials");
}

/**
 * Save a login for a registry host. Upserts by host on the server.
 *
 * `secret` is required here: this is the only way a password enters the system,
 * and omitting it on an EXISTING host is what tells the server to keep the
 * stored one (see updateCredential).
 */
export async function createCredential(body: {
  host: string;
  username?: string;
  secret: string;
  kind?: string;
  note?: string;
}): Promise<{ credential: Credential } & Envelope> {
  return apiJSON("/api/credentials", jsonInit("POST", body));
}

/** Edit a login. Omit `secret` to keep the stored one (the browser has no copy). */
export async function updateCredential(
  id: string,
  body: { host?: string; username?: string; secret?: string; kind?: string; note?: string }
): Promise<{ credential: Credential } & Envelope> {
  return apiJSON(`/api/credentials/${encodeURIComponent(id)}`, jsonInit("PUT", body));
}

export async function deleteCredential(id: string): Promise<Envelope> {
  return apiJSON(`/api/credentials/${encodeURIComponent(id)}`, { method: "DELETE" });
}

/** The built-in public registry catalog (read-only). */
export async function listRegistries(): Promise<
  { registries: BuiltinRegistry[]; defaultPullHost: string } & Envelope
> {
  return apiJSON("/api/registries");
}

export async function listSources(
  kind?: string
): Promise<{ sources: Source[] } & Envelope> {
  const url = kind ? `/api/sources?kind=${encodeURIComponent(kind)}` : "/api/sources";
  return apiJSON(url);
}

export async function createSource(
  body: Partial<Source>
): Promise<{ source: Source } & Envelope> {
  return apiJSON("/api/sources", jsonInit("POST", body));
}

export async function updateSource(
  id: string,
  body: Partial<Source>
): Promise<{ source: Source } & Envelope> {
  return apiJSON(`/api/sources/${id}`, jsonInit("PUT", body));
}

export async function deleteSource(id: string): Promise<Envelope> {
  return apiJSON(`/api/sources/${id}`, { method: "DELETE" });
}

// Settings

export async function getSettings(): Promise<{ settings: Settings } & Envelope> {
  return apiJSON("/api/settings");
}

export async function updateSettings(patch: Settings): Promise<Envelope> {
  return apiJSON("/api/settings", jsonInit("PUT", patch));
}

// --- 4. Realtime helpers ----------------------------------------------------

export interface LiveEvent {
  type: string; // "hello" | "ping" | "task.created" | "artifact.deleted" | ...
  data?: unknown;
}

export interface EventHandlers {
  /** full Task — `task.created`, `task.updated` */
  onTask?: (t: Task) => void;
  /** bytes-only coalesced update — `task.progress` */
  onProgress?: (p: TaskProgress) => void;
  /** full Artifact — `artifact.created` */
  onArtifact?: (a: Artifact) => void;
  /** every frame, including `ping`/`hello` and `task.finished` */
  onAny?: (type: string, data: unknown) => void;
  /** stream lifecycle, for a connection badge */
  onStatus?: (s: "open" | "closed") => void;
}

// subscribeEvents opens the GET /api/events SSE stream and dispatches each
// frame by `event.type`. EventSource reconnects by itself; the server sends
// a `hello` on open and a `ping` every 25s. Returns an unsubscribe function.
export function subscribeEvents(handlers: EventHandlers): () => void {
  const es = new EventSource("/api/events");

  es.onopen = () => handlers.onStatus?.("open");
  es.onerror = () => handlers.onStatus?.("closed");

  es.onmessage = (m) => {
    let evt: LiveEvent;
    try {
      evt = JSON.parse(m.data) as LiveEvent;
    } catch {
      return; // malformed frame — ignore, the stream stays open
    }
    const type = evt.type;
    switch (type) {
      case "task.created":
      case "task.updated":
        handlers.onTask?.(evt.data as Task);
        break;
      case "task.progress":
        handlers.onProgress?.(evt.data as TaskProgress);
        break;
      case "artifact.created":
        handlers.onArtifact?.(evt.data as Artifact);
        break;
    }
    handlers.onAny?.(type, evt.data);
  };

  return () => es.close();
}
