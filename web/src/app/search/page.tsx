"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { ArrowRight, Download, Loader2, Search as SearchIcon } from "lucide-react";
import {
  createTask,
  inspectImage,
  listRegistries,
  listSources,
  searchImages,
  type InspectResult,
  type SearchPage,
  type SearchResult,
  type Source,
  type TagInfo,
} from "@/lib/api";
import { formatBytes, platformString } from "@/lib/format";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectLabel,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { cn } from "@/lib/utils";

// 镜像搜索 (F1 + F2): the original tool's three-step flow —
//   1. keyword + source → result table
//   2. pick a row → tags / architecture / concurrency
//   3. 开始下载 → POST /api/tasks

const PAGE_SIZE = 20;

// Used when an inspect yields no platform list at all (unreachable
// registry, single-arch manifest without `actual`).
const FALLBACK_ARCHES = [
  "linux/amd64",
  "linux/arm64",
  "linux/arm/v7",
  "linux/s390x",
  "linux/ppc64le",
];

/**
 * One selectable DOWNLOAD source.
 *
 * `value` is what the API is given: a registry host. It works for both an
 * upstream registry (quay.io) and a Docker Hub accelerator (docker.1ms.run),
 * because the pull path resolves a host, and an accelerator is only a host that
 * happens to proxy Docker Hub's namespace.
 */
interface PullSource {
  value: string;
  label: string;
  host: string;
  group: string;
}

export default function SearchPage() {
  const [sources, setSources] = useState<Source[] | null>(null);
  const [source, setSource] = useState("");
  const [keyword, setKeyword] = useState("");
  const [page, setPage] = useState(1);
  const [results, setResults] = useState<SearchPage | null>(null);
  const [searching, setSearching] = useState(false);
  const [searchError, setSearchError] = useState("");

  const [selected, setSelected] = useState<SearchResult | null>(null);
  const [inspect, setInspect] = useState<InspectResult | null>(null);
  const [tags, setTags] = useState<TagInfo[]>([]);
  const [tag, setTag] = useState("latest");
  const [arch, setArch] = useState("");
  const [workers, setWorkers] = useState("4");
  const [detailLoading, setDetailLoading] = useState(false);
  const [detailError, setDetailError] = useState("");

  const [creating, setCreating] = useState(false);
  const [createError, setCreateError] = useState("");
  const [created, setCreated] = useState<{ id: string; ref: string } | null>(null);

  // `?q=` from the dashboard search box: pre-fill and auto-search once a
  // source is known. Read from window.location (static export means no
  // server-provided searchParams on a client page).
  const [autoQ, setAutoQ] = useState<string | null>(null);
  // `?source=` lets another page deep-link to a specific backend — the public
  // registry catalog on /settings links here for the searchable registries.
  // Without this the link would load the page but silently search whichever
  // source happened to be the default.
  const [wantedSource, setWantedSource] = useState<string | null>(null);
  // Direct full-reference entry: not every image is findable by keyword (a
  // private repo, or anything on a registry with no search API such as GHCR).
  const [manual, setManual] = useState("");
  const [manualError, setManualError] = useState("");
  // The DOWNLOAD source: the registry the chosen image is actually pulled from.
  // A first-class choice rather than a side effect of the search source, because
  // these are independent upstream registries — `nginx` on docker.io and
  // `nginx` on registry.k8s.io are different things, and only Docker Hub
  // implies "library/". Pulling the SAME name from a different source is a
  // legitimate thing to want, which is why choosing one sticks (`sourceTouched`)
  // instead of being overwritten the next time a result is clicked.
  const [pullSource, setPullSource] = useState("");
  const [sourceTouched, setSourceTouched] = useState(false);
  const [pullOptions, setPullOptions] = useState<PullSource[]>([]);
  // Plain-HTTP transport for a private registry. It must be sent to BOTH steps:
  // inspect resolves the tags/archs and the task fetches the layers, so a flag
  // that reached only one of them would show a picker that the download cannot
  // follow (or vice versa).
  const [plainHTTP, setPlainHTTP] = useState(false);

  useEffect(() => {
    (async () => {
      const params = new URLSearchParams(window.location.search);
      const q = params.get("q") ?? "";
      setWantedSource(params.get("source"));
      if (!q.trim()) return;
      setKeyword(q);
      setAutoQ(q.trim());
    })();
  }, []);

  const runSearch = useCallback(async (q: string, src: string, p: number) => {
    setSearching(true);
    setSearchError("");
    try {
      const res = await searchImages(q, src, p, PAGE_SIZE);
      if (res.ok) {
        setResults(res);
        setSelected(null);
        setInspect(null);
        setCreated(null);
      } else {
        setResults(null);
        setSearchError(res.error || "搜索失败");
      }
    } finally {
      setSearching(false);
    }
  }, []);

  // Load the configured search sources, then default to the enabled
  // default (or the first enabled one). A `?source=` hint wins, but ONLY when
  // it names an enabled source — otherwise a stale or hand-typed id would
  // leave the page with no backend selected at all.
  useEffect(() => {
    (async () => {
      const res = await listSources("search");
      const list = res.ok ? res.sources ?? [] : [];
      setSources(list);
      const hinted = wantedSource ? list.find((s) => s.enabled && s.id === wantedSource) : undefined;
      const def =
        hinted ??
        list.find((s) => s.enabled && s.isDefault) ??
        list.find((s) => s.enabled) ??
        list[0];
      if (def) setSource(def.id);
    })();
  }, [wantedSource]);

  useEffect(() => {
    if (autoQ === null || !source) return;
    (async () => {
      setAutoQ(null);
      setPage(1);
      await runSearch(autoQ, source, 1);
    })();
  }, [autoQ, source, runSearch]);

  function submit(e: React.FormEvent) {
    e.preventDefault();
    const q = keyword.trim();
    if (!q || !source) return;
    setPage(1);
    runSearch(q, source, 1);
  }

  function goPage(p: number) {
    if (p < 1 || !results) return;
    setPage(p);
    runSearch(keyword.trim(), source, p);
  }

  const totalPages = results ? Math.max(1, Math.ceil(results.total / (results.pageSize || PAGE_SIZE))) : 1;

  // Load the selectable download sources once: the upstream registries from the
  // read-only catalog, plus the Docker Hub accelerators (mirror rows). The
  // server reports which accelerator is the configured default, so nothing here
  // hardcodes docker.io.
  useEffect(() => {
    (async () => {
      const [regs, mirrors] = await Promise.all([listRegistries(), listSources("mirror")]);
      const options: PullSource[] = [];
      for (const r of regs.ok ? regs.registries ?? [] : []) {
        options.push({ value: r.host, label: r.name, host: r.host, group: "原始镜像站" });
      }
      for (const m of mirrors.ok ? mirrors.sources ?? [] : []) {
        options.push({
          value: m.host,
          label: m.name,
          host: m.host,
          group: "Docker Hub 加速源",
        });
      }
      // The DEFAULT comes from the server, which resolves the `default_mirror`
      // setting — it may name a built-in accelerator by id ("nju"), a user-added
      // mirror by host, or nothing. Guessing here is how docker.io ends up
      // assumed for someone who deliberately configured an accelerator.
      const serverDefault = regs.ok ? regs.defaultPullHost ?? "" : "";
      const chosen =
        options.find((o) => o.value === serverDefault)?.value ?? options[0]?.value ?? "";
      setPullOptions(options);
      setPullSource((cur) => cur || chosen);
    })();
  }, []);

  // Where a result from the selected source has to be pulled from.
  //
  // Searching "nginx" on the 1ms accelerator returns "library/nginx" — a Docker
  // Hub repository NAME. Parsed bare it resolves to registry-1.docker.io, which
  // is the host the user picked 1ms to avoid, so every call that touches the
  // registry (inspect, tags, the download itself) has to carry the source's
  // host. Empty means "let the server use the configured default mirror".
  const pullHost = useMemo(
    () => (sources ?? []).find((s) => s.id === source)?.pullHost ?? "",
    [sources, source]
  );

  // select resolves an image's tags and architectures, then shows the detail
  // panel. It is shared by the search table and the manual reference box, so a
  // pasted "ghcr.io/owner/repo:tag" behaves exactly like a clicked result.

  // loadDetail resolves tags and platforms for a repo ON A GIVEN HOST and stores
  // the result. It is the only place that talks to the registry, and the host is
  // always passed in rather than read from state: reading `pullSource` here would
  // race the setState in changePullSource and inspect the PREVIOUS source, which
  // is how the tag/arch list ends up describing a different registry than the
  // download uses.
  async function loadDetail(row: SearchResult, host: string, http: boolean = plainHTTP): Promise<string> {
    const repo = row.repository || row.name;
    setSelected(row);
    setInspect(null);
    setTags([]);
    setTag("latest");
    setArch("");
    setDetailError("");
    setCreated(null);
    setDetailLoading(true);
    try {
      // One call: inspect already returns the tags, read from the registry that
      // will actually be pulled, with a fallback the SERVER gates by namespace.
      // Asking a searcher here as well duplicated that rule client-side, which
      // is how a ghcr.io repository came to be shown Docker Hub's tags.
      const ins = await inspectImage(repo, host || undefined, undefined, http);
      let failure = "";
      if (ins.ok) {
        setInspect(ins);
        setArch(defaultArch(ins));
      } else {
        failure = ins.error || "读取镜像信息失败";
        setDetailError(failure);
        setArch(FALLBACK_ARCHES[0]);
      }
      // The registry's own list is the only one guaranteed pullable, and the
      // server has already applied its same-namespace fallback rule — so asking
      // a searcher here as well would duplicate that rule on the client and
      // could show another registry's tags (Docker Hub's list for a ghcr.io repo).
      const fromRegistry = (ins.ok ? ins.tags ?? [] : []).map((name) => ({ name }));
      if (fromRegistry.length > 0) {
        setTags(fromRegistry);
        setTag(fromRegistry[0].name);
      }
      return failure;
    } finally {
      setDetailLoading(false);
    }
  }

  // inspectManual takes a full image reference typed by the user.
  async function inspectManual() {
    const raw = manual.trim();
    if (!raw) return;
    setManualError("");
    // Cheap shape check first, so an obvious typo does not become a confusing
    // registry error 200 lines later.
    if (/\s/.test(raw)) {
      setManualError("镜像名里不能有空格");
      return;
    }
    // A tag typed inline ("nginx:1.26") becomes the selected tag, so the
    // detail panel's tag picker starts on what the user actually asked for.
    let repo = raw;
    let wanted = "";
    const at = raw.lastIndexOf("@");
    const colon = raw.lastIndexOf(":");
    const slash = raw.lastIndexOf("/");
    if (at < 0 && colon > slash) {
      repo = raw.slice(0, colon);
      wanted = raw.slice(colon + 1);
    }
    const failure = await loadDetail(
      {
        source: "manual",
        name: repo,
        repository: repo,
        image: repo.split("/").pop() || repo,
        description: "",
        stars: 0,
        pulls: 0,
        updated: "",
        official: false,
      },
      // A typed full reference goes to the chosen download source; a host in
      // the string itself still wins, because that is the server's rule.
      pullSource
    );
    if (wanted) setTag(wanted);
    if (failure) setManualError(failure);
  }

  const archOptions = useMemo(() => {
    if (!inspect) return FALLBACK_ARCHES;
    const list = (inspect.platforms ?? []).map(platformString);
    if (list.length > 0) return list;
    if (inspect.actual) return [platformString(inspect.actual)];
    return FALLBACK_ARCHES;
  }, [inspect]);

  const tagOptions = tags.length > 0 ? tags.map((t) => t.name) : ["latest"];

  // Grouped for the picker: an upstream registry and a Docker Hub accelerator
  // are different KINDS of source and the distinction is the user's to make.
  // What the pull will really resolve to: a host written in the reference wins
  // over the picker (that is the documented rule), so show the outcome rather
  // than just the selection.
  const effectiveRef = useMemo(() => {
    if (!selected) return "";
    // Once inspect has run, the SERVER's ref is authoritative: it already
    // applied the rules (a host written in the reference wins over the picker,
    // Docker Hub implies library/) and resolved aliases. Re-deriving that here
    // would be a second implementation of the same fact, free to disagree.
    if (inspect?.ref?.registry) {
      const { registry: host, repository } = inspect.ref;
      return tag ? `${host}/${repository}:${tag}` : `${host}/${repository}`;
    }
    const repo = selected.repository || selected.name;
    const name = tag ? `${repo}:${tag}` : repo;
    return pullSource ? `${pullSource}/${name}` : name;
  }, [selected, tag, pullSource, inspect]);

  const pullGroups = useMemo(() => {
    const groups: { name: string; items: PullSource[] }[] = [];
    for (const option of pullOptions) {
      let group = groups.find((g) => g.name === option.group);
      if (!group) {
        group = { name: option.group, items: [] };
        groups.push(group);
      }
      group.items.push(option);
    }
    return groups;
  }, [pullOptions]);

  // Changing the download source invalidates the tag/arch lists, because BOTH
  // come from the chosen registry's own manifest — a tag on quay.io need not
  // exist on docker.io. Re-resolve instead of silently offering stale options.
  async function changePullSource(next: string) {
    setSourceTouched(true);
    setPullSource(next);
    // Authoritative: go straight to loadDetail so the re-resolution uses `next`
    // and not whatever pullSource still holds in this render's closure.
    if (selected) await loadDetail(selected, next);
  }

  // select handles a clicked search result. The result's own source PROPOSES a
  // download host, but an explicit user choice wins — and when the chosen source
  // cannot serve the name, inspect fails loudly (download disabled) instead of
  // silently retargeting an own-namespace name onto another registry.
  async function select(row: SearchResult, proposedHost: string = pullHost): Promise<string> {
    if (!sourceTouched && proposedHost) setPullSource(proposedHost);
    const host = sourceTouched && pullSource ? pullSource : proposedHost;
    return loadDetail(row, host);
  }

  async function startDownload() {
    if (!selected) return;
    const repo = selected.repository || selected.name;
    const image = tag ? `${repo}:${tag}` : repo;
    const parsedWorkers = Number.parseInt(workers, 10);
    setCreating(true);
    setCreateError("");
    setCreated(null);
    try {
      const res = await createTask({
        image,
        // Without this the pull goes to the configured default mirror, which is
        // how a 1ms search ended up fetching from registry-1.docker.io.
        registry: pullSource || undefined,
        insecure: plainHTTP || undefined,
        platform: arch || undefined,
        workers: Number.isFinite(parsedWorkers) && parsedWorkers > 0 ? parsedWorkers : undefined,
      });
      if (res.ok && res.task) {
        setCreated({ id: res.task.id, ref: res.task.ref });
      } else {
        setCreateError(res.error || "创建下载任务失败");
      }
    } finally {
      setCreating(false);
    }
  }

  return (
    <div className="grid gap-6">
      <div>
        <h1 className="font-heading text-2xl font-semibold">镜像搜索</h1>
        <p className="text-sm text-muted-foreground">
          按关键字搜索镜像 → 选择标签与架构 → 创建下载任务。
        </p>
      </div>

      {/* ---- Direct reference: not everything is findable by keyword ---- */}
      <Card>
        <CardHeader>
          <CardTitle>直接指定镜像</CardTitle>
          <CardDescription>
            不搜索，直接填镜像全称。<strong>没有搜索接口的仓库都走这里</strong>：ghcr.io、registry.k8s.io、
            mcr.microsoft.com、public.ecr.aws、nvcr.io 等（例如
            <code className="mx-1">ghcr.io/owner/repo:tag</code>）。
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-3">
          <form
            className="flex flex-col gap-2 sm:flex-row"
            onSubmit={(e) => {
              e.preventDefault();
              void inspectManual();
            }}
          >
            <Input
              value={manual}
              onChange={(e) => setManual(e.target.value)}
              placeholder="例如 nginx:1.26 / ghcr.io/owner/repo:tag / harbor.internal:5000/app:1.0"
              aria-label="镜像全称"
            />
            <Button type="submit" disabled={detailLoading || !manual.trim()}>
              {detailLoading ? (
                <Loader2 className="h-4 w-4 animate-spin" />
              ) : (
                <SearchIcon className="h-4 w-4" />
              )}
              检查并下载
            </Button>
          </form>
          <p className="text-xs text-muted-foreground">
            下载来源：<span className="font-mono">{pullHost || "当前默认镜像源（可在「设置」中修改）"}</span>
            <span className="mx-1">·</span>
            镜像里写了主机名（如 <code>ghcr.io/...</code>）时，以镜像里的主机为准。
          </p>
          {manualError && <p className="text-sm text-destructive">{manualError}</p>}
        </CardContent>
      </Card>

      {/* ---- Step 1: keyword + source + results ---- */}
      <Card>
        <CardHeader>
          <CardTitle>1 · 搜索镜像</CardTitle>
          <CardDescription>
            数据源可在「设置 → 数据源管理」中启用或新增。这里只列出<strong>有公开搜索接口</strong>的站点
            （Docker Hub / Quay / 1ms / MCR）；ghcr.io、registry.k8s.io、ECR、NGC 等没有搜索接口，
            请用上面的「直接指定镜像」填全称。
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4">
          <form onSubmit={submit} className="flex flex-col gap-2 sm:flex-row">
            <Input
              value={keyword}
              onChange={(e) => setKeyword(e.target.value)}
              placeholder="关键字，例如 nginx"
              aria-label="搜索关键字"
            />
            <Select value={source} onValueChange={(v) => setSource((v as string) ?? "")}>
              <SelectTrigger className="w-full sm:w-52" aria-label="搜索源">
                {/* The value is a stored row id (`src_ab12…`). Select.Value can
                    only resolve a label from items that are MOUNTED, and the
                    popup's items are not while it is closed — so without this
                    render function the trigger displayed the raw id. */}
                <SelectValue placeholder="选择数据源">
                  {(v) => (sources ?? []).find((s) => s.id === v)?.name ?? "选择数据源"}
                </SelectValue>
              </SelectTrigger>
              <SelectContent>
                {(sources ?? []).map((s) => (
                  <SelectItem key={s.id} value={s.id}>
                    {s.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Button type="submit" disabled={searching || !source}>
              {searching ? <Loader2 className="h-4 w-4 animate-spin" /> : <SearchIcon className="h-4 w-4" />}
              搜索
            </Button>
          </form>

          {searchError && <p className="text-sm text-destructive">{searchError}</p>}

          {searching && (
            <div className="grid gap-2">
              <Skeleton className="h-9 w-full" />
              <Skeleton className="h-9 w-full" />
              <Skeleton className="h-9 w-full" />
            </div>
          )}

          {!searching && results && (
            <>
              <p className="text-xs text-muted-foreground">
                共 {results.total} 个结果 · 点击任意一行选择镜像
              </p>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>镜像</TableHead>
                    <TableHead className="w-20 text-right">Stars</TableHead>
                    <TableHead className="w-24 text-right">Pulls</TableHead>
                    <TableHead className="hidden w-28 md:table-cell">更新日期</TableHead>
                    <TableHead className="hidden lg:table-cell">描述</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {results.results.length === 0 ? (
                    <TableRow>
                      <TableCell colSpan={5} className="py-8 text-center text-sm text-muted-foreground">
                        没有匹配的镜像。
                      </TableCell>
                    </TableRow>
                  ) : (
                    results.results.map((r) => (
                      <TableRow
                        key={`${r.source}-${r.name}`}
                        onClick={() => select(r)}
                        className={cn(
                          "cursor-pointer",
                          selected?.name === r.name && "bg-muted/60"
                        )}
                      >
                        <TableCell className="font-medium">
                          <span className="flex items-center gap-2">
                            <span className="truncate">{r.image || r.name}</span>
                            {r.official && <Badge variant="secondary">官方</Badge>}
                          </span>
                        </TableCell>
                        <TableCell className="text-right tabular-nums">{r.stars}</TableCell>
                        <TableCell className="text-right tabular-nums">{r.pulls}</TableCell>
                        <TableCell className="hidden text-muted-foreground md:table-cell">
                          {r.updated || "—"}
                        </TableCell>
                        <TableCell className="hidden max-w-72 truncate text-muted-foreground lg:table-cell">
                          {r.description}
                        </TableCell>
                      </TableRow>
                    ))
                  )}
                </TableBody>
              </Table>

              <div className="flex items-center justify-between">
                <span className="text-xs text-muted-foreground">
                  第 {results.page} / {totalPages} 页
                </span>
                <div className="flex gap-2">
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={page <= 1 || searching}
                    onClick={() => goPage(page - 1)}
                  >
                    上一页
                  </Button>
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={page >= totalPages || searching}
                    onClick={() => goPage(page + 1)}
                  >
                    下一页
                  </Button>
                </div>
              </div>
            </>
          )}
        </CardContent>
      </Card>

      {/* ---- Step 2: the DOWNLOAD source (before tag/arch, because both
           are read from the chosen registry) ---- */}
      {selected && (
        <Card>
          <CardHeader>
            <CardTitle>2 · 选择下载源</CardTitle>
            <CardDescription>
              从哪个镜像站拉取。docker.io / quay.io / ghcr.io / registry.k8s.io 等是
              <strong className="mx-1">各自独立的原始镜像站</strong>
              ，同一个仓库名在不同站点上并不等价；Docker Hub 加速源只代理 Docker Hub 的命名空间。
            </CardDescription>
          </CardHeader>
          <CardContent className="grid gap-3">
            <div className="grid gap-2 sm:max-w-md">
              <Select value={pullSource} onValueChange={(v) => void changePullSource((v as string) ?? "")}>
                <SelectTrigger className="w-full" aria-label="下载源">
                  {/* Name first (readable), then the host that will actually be
                      contacted — the same two facts the list shows. */}
                  <SelectValue placeholder="选择下载源">
                    {(v) => {
                      const opt = pullOptions.find((o) => o.value === v);
                      return opt ? `${opt.label}（${opt.host}）` : "选择下载源";
                    }}
                  </SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {pullGroups.map((g) => (
                    <SelectGroup key={g.name}>
                      <SelectLabel>{g.name}</SelectLabel>
                      {g.items.map((o) => (
                        <SelectItem key={`${g.name}-${o.value}`} value={o.value}>
                          {o.label}
                          <span className="ml-2 font-mono text-xs text-muted-foreground">{o.host}</span>
                        </SelectItem>
                      ))}
                    </SelectGroup>
                  ))}
                </SelectContent>
              </Select>
              <p className="text-xs text-muted-foreground">
                实际拉取：<span className="font-mono">{effectiveRef || "—"}</span>
                <span className="mx-1">·</span>
                镜像名里写了主机名（如 <code>ghcr.io/owner/repo</code>）时，以镜像里的主机为准。
              </p>
              {/* Transport belongs to the source, and BOTH steps need it: inspect
                  resolves the tags/archs, the task fetches the layers. Private
                  registries are frequently plain HTTP. */}
              <div className="flex items-center gap-3 pt-1">
                <Switch
                  id="plain-http"
                  checked={plainHTTP}
                  onCheckedChange={(checked) => {
                    setPlainHTTP(checked);
                    // The tags/archs must be re-read over the same transport the
                    // download will use, or the picker describes a different
                    // connection than the pull.
                    if (selected) void loadDetail(selected, pullSource, checked);
                  }}
                />
                <Label htmlFor="plain-http" className="text-sm font-normal">
                  使用 HTTP 明文连接（私有 registry）
                </Label>
              </div>
            </div>
            {detailError && (
              <div className="grid gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-3">
                <p className="text-sm text-destructive">{detailError}</p>
                <p className="text-xs text-muted-foreground">
                  未能从该下载源读到清单，标签/架构无法确定，下载已禁用。可以换一个下载源重试。
                </p>
                {/* One click instead of a dead end. The accelerators come first
                    because the common failure is a blocked network to Docker
                    Hub, and re-resolving is required either way — a tag that
                    exists here need not exist there. */}
                <div className="flex flex-wrap items-center gap-2">
                  <span className="text-xs text-muted-foreground">换一个下载源：</span>
                  {pullOptions
                    .filter((o) => o.value !== pullSource)
                    .sort((a, b) => (a.group === b.group ? 0 : a.group === "Docker Hub 加速源" ? -1 : 1))
                    .slice(0, 4)
                    .map((o) => (
                      <Button
                        key={o.value}
                        size="sm"
                        variant="outline"
                        onClick={() => void changePullSource(o.value)}
                      >
                        {o.label}
                      </Button>
                    ))}
                </div>
              </div>
            )}
          </CardContent>
        </Card>
      )}

      {/* ---- Step 3: tags / arch / concurrency ---- */}
      {selected && (
        <Card>
          <CardHeader>
            <CardTitle>3 · 选择版本与架构</CardTitle>
            <CardDescription className="font-mono">{selected.repository || selected.name}</CardDescription>
          </CardHeader>
          <CardContent className="grid gap-5">
            {detailLoading ? (
              <div className="grid gap-2">
                <Skeleton className="h-8 w-full max-w-md" />
                <Skeleton className="h-8 w-full max-w-md" />
              </div>
            ) : (
              <>
                <div className="grid gap-4 sm:grid-cols-3">
                  <div className="grid gap-2">
                    <Label>
                      标签
                      {inspect?.tagsFrom && (
                        <span className="ml-2 font-normal text-muted-foreground">
                          {inspect.tagsFrom === inspect.ref?.registry
                            ? "来自该仓库"
                            : `来自搜索源 ${inspect.tagsFrom}`}
                        </span>
                      )}
                    </Label>
                    <Select value={tag} onValueChange={(v) => setTag((v as string) ?? "")}>
                      <SelectTrigger className="w-full" aria-label="标签">
                        <SelectValue placeholder="选择标签" />
                      </SelectTrigger>
                      <SelectContent>
                        {tagOptions.map((t) => (
                          <SelectItem key={t} value={t}>
                            {t}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="grid gap-2">
                    <Label>架构</Label>
                    <Select value={arch} onValueChange={(v) => setArch((v as string) ?? "")}>
                      <SelectTrigger className="w-full" aria-label="架构">
                        <SelectValue placeholder="选择架构" />
                      </SelectTrigger>
                      <SelectContent>
                        {archOptions.map((a) => (
                          <SelectItem key={a} value={a}>
                            {a}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="workers">并发数</Label>
                    <Input
                      id="workers"
                      type="number"
                      min={1}
                      max={16}
                      value={workers}
                      onChange={(e) => setWorkers(e.target.value)}
                    />
                  </div>
                </div>

                <div className="flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
                  {inspect && (
                    <>
                      <span>
                        清单类型：{inspect.isIndex ? "多架构索引" : "单架构清单"} · 层数 {inspect.layerCount}
                      </span>
                      {inspect.totalBytes > 0 && <span>· 约 {formatBytes(inspect.totalBytes)}</span>}
                      {inspect.platforms?.length > 0 && (
                        <span>· 可用架构 {inspect.platforms.length} 个</span>
                      )}
                    </>
                  )}
                  {tags.length === 0 && <span>· 未获取到标签，默认使用 latest</span>}
                </div>

                {detailError && (
              <div className="grid gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-3">
                <p className="text-sm text-destructive">{detailError}</p>
                <p className="text-xs text-muted-foreground">
                  未能从该下载源读到清单，标签/架构无法确定，下载已禁用。可以换一个下载源重试。
                </p>
                {/* One click instead of a dead end. The accelerators come first
                    because the common failure is a blocked network to Docker
                    Hub, and re-resolving is required either way — a tag that
                    exists here need not exist there. */}
                <div className="flex flex-wrap items-center gap-2">
                  <span className="text-xs text-muted-foreground">换一个下载源：</span>
                  {pullOptions
                    .filter((o) => o.value !== pullSource)
                    .sort((a, b) => (a.group === b.group ? 0 : a.group === "Docker Hub 加速源" ? -1 : 1))
                    .slice(0, 4)
                    .map((o) => (
                      <Button
                        key={o.value}
                        size="sm"
                        variant="outline"
                        onClick={() => void changePullSource(o.value)}
                      >
                        {o.label}
                      </Button>
                    ))}
                </div>
              </div>
            )}
              </>
            )}

            <div className="flex flex-wrap items-center gap-3">
              {/* A failed inspect means the tag/arch pairing is a guess: the
                  platform list came from the fallback constants, not from the
                  registry, so starting here produces exactly the "I selected it
                  but it cannot download" failure. Require a successful inspect. */}
              <Button
                size="lg"
                onClick={startDownload}
                disabled={creating || !arch || !!detailError || !inspect}
              >
                {creating ? <Loader2 className="h-4 w-4 animate-spin" /> : <Download className="h-4 w-4" />}
                开始下载
              </Button>
              {created && (
                <span className="flex items-center gap-2 text-sm text-muted-foreground">
                  任务已创建
                  <Badge variant="secondary" className="font-mono">
                    {created.ref}
                  </Badge>
                  <Link
                    href="/tasks/"
                    className="inline-flex items-center gap-1 text-primary underline underline-offset-2"
                  >
                    查看进度 <ArrowRight className="h-3.5 w-3.5" />
                  </Link>
                </span>
              )}
              {createError && <span className="text-sm text-destructive">{createError}</span>}
            </div>
          </CardContent>
        </Card>
      )}
    </div>
  );
}

function defaultArch(ins: InspectResult): string {
  const list = (ins.platforms ?? []).map(platformString);
  if (list.length > 0) {
    return list.find((p) => p.includes("amd64")) ?? list[0];
  }
  if (ins.actual) return platformString(ins.actual);
  return FALLBACK_ARCHES[0];
}
