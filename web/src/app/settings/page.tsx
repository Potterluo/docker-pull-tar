"use client";

import Link from "next/link";
import { useCallback, useEffect, useState } from "react";
import { Check, Pencil, Plus, Star, Trash2 } from "lucide-react";
import {
  createSource,
  deleteSource,
  getSettings,
  listRegistries,
  listSources,
  updateSettings,
  updateSource,
  type BuiltinRegistry,
  type Settings,
  type Source,
} from "@/lib/api";
import { useTheme, THEME_PRESETS, type Theme } from "@/components/theme-provider";
import { Button } from "@/components/ui/button";
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import {
  Card,
  CardContent,
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { cn } from "@/lib/utils";

// 设置: 网络与代理 / 下载选项 / 数据源管理 / 外观.
// Server-side preferences live in the settings key/value table and are
// written as a bulk patch; the appearance card is browser-local and is
// owned by theme-provider.tsx.

const DEFAULTS: Settings = {
  output_dir: "",
  workers: "4",
  verify_tls: "true",
  proxy_mode: "system",
  proxy_url: "",
  chunk_threshold_mb: "50",
  max_retries: "10",
};

export default function SettingsPage() {
  const [settings, setSettings] = useState<Settings | null>(null);
  const [loadError, setLoadError] = useState("");

  const load = useCallback(async () => {
    const res = await getSettings();
    if (res.ok) setSettings({ ...DEFAULTS, ...(res.settings ?? {}) });
    else setLoadError(res.error || "读取设置失败");
  }, []);

  useEffect(() => {
    (async () => {
      await load();
    })();
  }, [load]);

  return (
    <div className="grid gap-6">
      <div>
        <h1 className="font-heading text-2xl font-semibold">设置</h1>
        <p className="text-sm text-muted-foreground">
          代理、下载参数、数据源与外观。服务端设置保存后会立即对新的下载任务生效。
        </p>
      </div>

      {loadError && <p className="text-sm text-destructive">{loadError}</p>}

      {settings === null ? (
        <div className="grid gap-6">
          <Skeleton className="h-48 w-full" />
          <Skeleton className="h-64 w-full" />
        </div>
      ) : (
        <>
          <ProxyCard settings={settings} />
          <DownloadCard settings={settings} />
          <SourcesCard />
        <RegistryCatalogCard />
          <AppearanceCard />
        </>
      )}
    </div>
  );
}

function Saved({ msg, error }: { msg: string; error: string }) {
  if (error) return <span className="text-sm text-destructive">{error}</span>;
  if (!msg) return null;
  return (
    <span className="flex items-center gap-1 text-sm text-muted-foreground">
      <Check className="h-3.5 w-3.5 text-primary" />
      {msg}
    </span>
  );
}

function ProxyCard({ settings }: { settings: Settings }) {
  const [mode, setMode] = useState(settings.proxy_mode || "system");
  const [url, setUrl] = useState(settings.proxy_url || "");
  const [msg, setMsg] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function save() {
    setBusy(true);
    setMsg("");
    setError("");
    try {
      const res = await updateSettings({
        proxy_mode: mode,
        proxy_url: mode === "custom" ? url : "",
      });
      if (res.ok) setMsg("已保存");
      else setError(res.error || "保存失败");
    } finally {
      setBusy(false);
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>网络与代理</CardTitle>
        <CardDescription>
          访问镜像仓库时的网络方式。<code>system</code> 表示跟随环境变量（HTTP_PROXY 等）。
        </CardDescription>
      </CardHeader>
      <CardContent className="grid max-w-xl gap-4">
        <div className="grid gap-2">
          <Label>代理模式</Label>
          <Select value={mode} onValueChange={(v) => setMode((v as string) ?? "system")}>
            <SelectTrigger className="w-full sm:w-48" aria-label="代理模式">
              <SelectValue placeholder="选择代理模式" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="system">跟随系统环境变量</SelectItem>
              <SelectItem value="none">不使用代理</SelectItem>
              <SelectItem value="custom">自定义代理</SelectItem>
            </SelectContent>
          </Select>
        </div>
        <div className="grid gap-2">
          <Label htmlFor="proxy-url">代理地址</Label>
          <Input
            id="proxy-url"
            value={url}
            disabled={mode !== "custom"}
            placeholder="http://127.0.0.1:7890"
            onChange={(e) => setUrl(e.target.value)}
          />
          {mode !== "custom" && (
            <p className="text-xs text-muted-foreground">仅在选择「自定义代理」时可填写。</p>
          )}
        </div>
      </CardContent>
      <CardFooter className="gap-3">
        <Button size="sm" onClick={save} disabled={busy}>
          保存
        </Button>
        <Saved msg={msg} error={error} />
      </CardFooter>
    </Card>
  );
}

function DownloadCard({ settings }: { settings: Settings }) {
  const [outputDir, setOutputDir] = useState(settings.output_dir || "");
  const [workers, setWorkers] = useState(settings.workers || "4");
  const [chunkThresholdMb, setChunk] = useState(settings.chunk_threshold_mb || "50");
  const [maxRetries, setMaxRetries] = useState(settings.max_retries || "10");
  const [verifyTls, setVerifyTls] = useState(settings.verify_tls !== "false");
  const [msg, setMsg] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function save() {
    setBusy(true);
    setMsg("");
    setError("");
    try {
      const res = await updateSettings({
        output_dir: outputDir.trim(),
        workers: String(clampInt(workers, 1, 32, 4)),
        chunk_threshold_mb: String(clampInt(chunkThresholdMb, 1, 10240, 50)),
        max_retries: String(clampInt(maxRetries, 0, 100, 10)),
        verify_tls: verifyTls ? "true" : "false",
      });
      if (res.ok) setMsg("已保存");
      else setError(res.error || "保存失败");
    } finally {
      setBusy(false);
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>下载选项</CardTitle>
        <CardDescription>输出目录、并发与重试策略，作为新任务的默认值。</CardDescription>
      </CardHeader>
      <CardContent className="grid max-w-xl gap-4">
        <div className="grid gap-2">
          <Label htmlFor="output-dir">输出目录</Label>
          <Input
            id="output-dir"
            value={outputDir}
            placeholder="留空使用默认下载目录"
            onChange={(e) => setOutputDir(e.target.value)}
          />
        </div>
        <div className="grid gap-4 sm:grid-cols-3">
          <div className="grid gap-2">
            <Label htmlFor="workers">并发数</Label>
            <Input
              id="workers"
              type="number"
              min={1}
              max={32}
              value={workers}
              onChange={(e) => setWorkers(e.target.value)}
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="chunk-threshold">分片阈值 (MB)</Label>
            <Input
              id="chunk-threshold"
              type="number"
              min={1}
              value={chunkThresholdMb}
              onChange={(e) => setChunk(e.target.value)}
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="max-retries">最大重试次数</Label>
            <Input
              id="max-retries"
              type="number"
              min={0}
              max={100}
              value={maxRetries}
              onChange={(e) => setMaxRetries(e.target.value)}
            />
          </div>
        </div>
        <div className="flex items-center gap-3">
          <Switch
            id="verify-tls"
            checked={verifyTls}
            onCheckedChange={(checked) => setVerifyTls(checked)}
          />
          <Label htmlFor="verify-tls">TLS 证书校验</Label>
          <span className="text-xs text-muted-foreground">
            关闭后接受自签名证书的镜像仓库
          </span>
        </div>
      </CardContent>
      <CardFooter className="gap-3">
        <Button size="sm" onClick={save} disabled={busy}>
          保存
        </Button>
        <Saved msg={msg} error={error} />
      </CardFooter>
    </Card>
  );
}

/**
 * The built-in public registry catalog, read-only.
 *
 * These are NOT mirrors: a mirror proxies Docker Hub's namespace (so "nginx"
 * means "library/nginx"), while these have their own namespaces — on quay.io
 * the image is "prometheus/prometheus", not "library/prometheus". They are
 * listed here purely to be discoverable: pulling `ghcr.io/owner/repo` needs no
 * configuration, but nothing said so before.
 */
function RegistryCatalogCard() {
  const [registries, setRegistries] = useState<BuiltinRegistry[] | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    (async () => {
      const res = await listRegistries();
      if (res.ok) setRegistries(res.registries ?? []);
      else setError(res.error || "读取镜像源目录失败");
    })();
  }, []);

  return (
    <Card>
      <CardHeader>
        <CardTitle>公共镜像源</CardTitle>
        <CardDescription>
          内置的公共仓库，直接按 <code>主机/命名空间/仓库</code> 拉取即可，无需额外配置；
          要下载私有仓库请到「凭证管理」添加登录信息。
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-2">
        {error && <p className="text-sm text-destructive">{error}</p>}
        {registries === null && !error && <p className="text-sm text-muted-foreground">加载中…</p>}
        {(registries ?? []).map((r) => (
          <div
            key={r.id}
            className="flex flex-wrap items-baseline justify-between gap-2 rounded-md border p-3"
          >
            <div className="grid gap-0.5">
              <span className="text-sm font-medium">{r.name}</span>
              <span className="font-mono text-xs text-muted-foreground">{r.host}</span>
              {r.note && <span className="text-xs text-muted-foreground">{r.note}</span>}
            </div>
            {r.searchId ? (
              <Button
                variant="outline"
                size="sm"
                render={<Link href={`/search?source=${encodeURIComponent(r.searchId)}`} />}
              >
                搜索
              </Button>
            ) : (
              <span className="text-xs text-muted-foreground">按引用拉取</span>
            )}
          </div>
        ))}
      </CardContent>
    </Card>
  );
}

function SourcesCard() {
  const [search, setSearch] = useState<Source[] | null>(null);
  const [mirrors, setMirrors] = useState<Source[] | null>(null);
  const [editor, setEditor] = useState<{ open: boolean; kind: "search" | "mirror"; source: Source | null }>(
    { open: false, kind: "search", source: null }
  );
  const [removing, setRemoving] = useState<Source | null>(null);
  const [error, setError] = useState("");

  const reload = useCallback(async () => {
    const [a, b] = await Promise.all([listSources("search"), listSources("mirror")]);
    if (a.ok) setSearch(a.sources ?? []);
    else setError(a.error || "读取搜索源失败");
    if (b.ok) setMirrors(b.sources ?? []);
    else setError(b.error || "读取镜像源失败");
  }, []);

  useEffect(() => {
    (async () => {
      await reload();
    })();
  }, [reload]);

  async function mutate(fn: () => Promise<{ ok: boolean; error?: string }>) {
    setError("");
    const res = await fn();
    if (!res.ok) setError(res.error || "操作失败");
    reload();
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>数据源管理</CardTitle>
        <CardDescription>
          搜索源决定「镜像搜索」的查询后端，镜像源是 Docker Hub 的 host 替换。
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-6">
        {error && <p className="text-sm text-destructive">{error}</p>}
        <SourceList
          title="搜索源"
          kind="search"
          sources={search}
          onToggle={(s, enabled) => mutate(() => updateSource(s.id, { enabled }))}
          onDefault={(s) => mutate(() => updateSource(s.id, { isDefault: true }))}
          onEdit={(s) => setEditor({ open: true, kind: "search", source: s })}
          onRemove={(s) => setRemoving(s)}
          onAdd={() => setEditor({ open: true, kind: "search", source: null })}
        />
        <SourceList
          title="镜像源"
          kind="mirror"
          sources={mirrors}
          onToggle={(s, enabled) => mutate(() => updateSource(s.id, { enabled }))}
          onDefault={(s) => mutate(() => updateSource(s.id, { isDefault: true }))}
          onEdit={(s) => setEditor({ open: true, kind: "mirror", source: s })}
          onRemove={(s) => setRemoving(s)}
          onAdd={() => setEditor({ open: true, kind: "mirror", source: null })}
        />
      </CardContent>

      <SourceEditor
        key={editor.open ? editor.source?.id ?? `new-${editor.kind}` : "closed"}
        open={editor.open}
        kind={editor.kind}
        source={editor.source}
        onOpenChange={(open) => setEditor((e) => ({ ...e, open }))}
        onSaved={() => {
          setEditor((e) => ({ ...e, open: false }));
          reload();
        }}
      />

      <AlertDialog open={!!removing} onOpenChange={(open) => !open && setRemoving(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除数据源？</AlertDialogTitle>
            <AlertDialogDescription>
              「{removing?.name}」将从数据源列表中移除。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <Button
              variant="destructive"
              onClick={async () => {
                const target = removing;
                setRemoving(null);
                if (target) await mutate(() => deleteSource(target.id));
              }}
            >
              删除
            </Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Card>
  );
}

function SourceList({
  title,
  kind,
  sources,
  onToggle,
  onDefault,
  onEdit,
  onRemove,
  onAdd,
}: {
  title: string;
  kind: "search" | "mirror";
  sources: Source[] | null;
  onToggle: (s: Source, enabled: boolean) => void;
  onDefault: (s: Source) => void;
  onEdit: (s: Source) => void;
  onRemove: (s: Source) => void;
  onAdd: () => void;
}) {
  return (
    <div className="grid gap-2">
      <div className="flex items-center justify-between">
        <p className="text-sm font-medium">{title}</p>
        <Button variant="outline" size="sm" onClick={onAdd}>
          <Plus className="h-3.5 w-3.5" /> 新增
        </Button>
      </div>
      {sources === null ? (
        <Skeleton className="h-16 w-full" />
      ) : sources.length === 0 ? (
        <p className="rounded-lg border border-dashed py-6 text-center text-sm text-muted-foreground">
          暂无{title}。
        </p>
      ) : (
        <ul className="grid gap-1.5">
          {sources.map((s) => (
            <li
              key={s.id}
              className="flex flex-wrap items-center gap-3 rounded-lg border px-3 py-2"
            >
              <div className="min-w-0 flex-1">
                <p className="flex items-center gap-2 text-sm font-medium">
                  <span className="truncate">{s.name}</span>
                  {s.isDefault && (
                    <span className="flex items-center gap-1 text-xs text-primary">
                      <Star className="h-3 w-3" /> 默认
                    </span>
                  )}
                </p>
                <p className="truncate font-mono text-xs text-muted-foreground">
                  {kind === "search" ? s.url : s.host}
                </p>
              </div>
              <Switch
                checked={s.enabled}
                onCheckedChange={(checked) => onToggle(s, checked)}
                aria-label={`${s.enabled ? "停用" : "启用"} ${s.name}`}
              />
              <Button
                variant="ghost"
                size="sm"
                disabled={s.isDefault}
                onClick={() => onDefault(s)}
              >
                设为默认
              </Button>
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label="编辑"
                title="编辑"
                onClick={() => onEdit(s)}
              >
                <Pencil className="h-4 w-4" />
              </Button>
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label="删除"
                title="删除"
                onClick={() => onRemove(s)}
              >
                <Trash2 className="h-4 w-4 text-destructive" />
              </Button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function SourceEditor({
  open,
  kind,
  source,
  onOpenChange,
  onSaved,
}: {
  open: boolean;
  kind: "search" | "mirror";
  source: Source | null;
  onOpenChange: (open: boolean) => void;
  onSaved: () => void;
}) {
  const editing = !!source;
  const isSearch = kind === "search";
  const label = isSearch ? "搜索源" : "镜像源";
  // Initialised from props: the parent re-keys this component on every
  // open, so "edit" always mounts with the row's current values.
  const [name, setName] = useState(source?.name ?? "");
  const [value, setValue] = useState(
    ((isSearch ? source?.url : source?.host) ?? "") as string
  );
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function save() {
    if (!name.trim()) {
      setError("名称不能为空");
      return;
    }
    if (!value.trim()) {
      setError(isSearch ? "URL 不能为空" : "Host 不能为空");
      return;
    }
    setBusy(true);
    setError("");
    try {
      const body: Partial<Source> = {
        kind,
        name: name.trim(),
        url: isSearch ? value.trim() : "",
        host: isSearch ? "" : value.trim(),
      };
      const res = editing
        ? await updateSource(source!.id, body)
        : await createSource({ ...body, enabled: true, priority: 0, isDefault: false });
      if (res.ok) onSaved();
      else setError(res.error || "保存失败");
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>
            {editing ? `编辑${label}` : `新增${label}`}
          </DialogTitle>
          <DialogDescription>
            {isSearch
              ? "搜索源是一个搜索后端地址，例如 https://hub.docker.com。"
              : "镜像源是 Docker Hub 的 host 替换，例如 docker.1ms.run。"}
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid gap-2">
            <Label htmlFor="source-name">名称</Label>
            <Input
              id="source-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder={isSearch ? "1ms 加速源" : "南大镜像源"}
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="source-value">{isSearch ? "URL" : "Host"}</Label>
            <Input
              id="source-value"
              value={value}
              onChange={(e) => setValue(e.target.value)}
              placeholder={isSearch ? "https://1ms.run/api/v1/registry" : "docker.nju.edu.cn"}
            />
          </div>
          {error && <p className="text-sm text-destructive">{error}</p>}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            取消
          </Button>
          <Button onClick={save} disabled={busy}>
            {busy ? "保存中…" : "保存"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function AppearanceCard() {
  const { theme, setTheme, accent, setAccent } = useTheme();
  const modes: { value: Theme; label: string }[] = [
    { value: "light", label: "浅色" },
    { value: "dark", label: "深色" },
    { value: "system", label: "跟随系统" },
  ];
  return (
    <Card>
      <CardHeader>
        <CardTitle>外观</CardTitle>
        <CardDescription>
          深浅色模式与主题配色相互独立，保存在当前浏览器中。
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-5">
        <div>
          <p className="mb-2 text-sm font-medium">模式</p>
          <div className="flex flex-wrap gap-2">
            {modes.map((o) => (
              <Button
                key={o.value}
                size="sm"
                variant={theme === o.value ? "default" : "outline"}
                onClick={() => setTheme(o.value)}
              >
                {o.label}
              </Button>
            ))}
          </div>
        </div>
        <div>
          <p className="mb-2 text-sm font-medium">主题配色</p>
          <div className="flex flex-wrap gap-2">
            {THEME_PRESETS.map((p) => (
              <button
                key={p.id}
                onClick={() => setAccent(p.id)}
                aria-pressed={accent === p.id}
                className={cn(
                  "flex items-center gap-2 rounded-lg border px-3 py-1.5 text-sm transition-colors hover:bg-accent",
                  accent === p.id ? "border-primary ring-1 ring-primary" : "border-border"
                )}
              >
                <span
                  className="h-4 w-4 rounded-full border border-black/10"
                  style={{ background: p.swatch }}
                />
                {p.label}
              </button>
            ))}
          </div>
        </div>
      </CardContent>
    </Card>
  );
}

function clampInt(raw: string, min: number, max: number, fallback: number): number {
  const n = Number.parseInt(raw, 10);
  if (!Number.isFinite(n)) return fallback;
  return Math.max(min, Math.min(max, n));
}
