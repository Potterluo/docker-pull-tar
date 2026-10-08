"use client";

import { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { ArrowRight, Search } from "lucide-react";
import {
  getStats,
  getStatus,
  listTasks,
  subscribeEvents,
  type Stats,
  type Status,
  type Task,
} from "@/lib/api";
import { formatBytes, percent, taskStatusLabel, taskStatusVariant } from "@/lib/format";
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
import { ScrollArea } from "@/components/ui/scroll-area";

// 概览 (Dashboard): stat cards + recent tasks + a live event feed.
// The feed is fed by GET /api/events (SSE) — no polling.

interface FeedEntry {
  id: number;
  time: string;
  text: string;
  kind: string;
}

export default function DashboardPage() {
  const router = useRouter();
  const [status, setStatus] = useState<Status | null>(null);
  const [stats, setStats] = useState<Stats | null>(null);
  const [tasks, setTasks] = useState<Task[] | null>(null);
  const [feed, setFeed] = useState<FeedEntry[]>([]);
  const [keyword, setKeyword] = useState("");

  const reload = useCallback(async () => {
    const [st, sts, ts] = await Promise.all([getStatus(), getStats(), listTasks()]);
    if (st.ok) setStatus(st);
    if (sts.ok) setStats(sts);
    if (ts.ok) setTasks(ts.tasks ?? []);
  }, []);

  useEffect(() => {
    (async () => {
      await reload();
    })();
  }, [reload]);

  useEffect(() => {
    let counter = 0;
    return subscribeEvents({
      onAny: (type, data) => {
        if (type === "ping" || type === "hello") return;
        counter += 1;
        const entry: FeedEntry = {
          id: counter,
          time: new Date().toLocaleTimeString("zh-CN", { hour12: false }),
          text: describeEvent(type, data),
          kind: type,
        };
        setFeed((f) => [entry, ...f].slice(0, 40));
        // Counters change on lifecycle events only — not on every byte tick.
        if (type.startsWith("artifact.") || type === "task.created" || type === "task.finished") {
          reload();
        }
      },
    });
  }, [reload]);

  function submitSearch(e: React.FormEvent) {
    e.preventDefault();
    const q = keyword.trim();
    router.push(q ? `/search/?q=${encodeURIComponent(q)}` : "/search/");
  }

  const recent = (tasks ?? []).slice(0, 5);

  return (
    <div className="grid gap-6">
      <div>
        <h1 className="font-heading text-2xl font-semibold">概览</h1>
        <p className="text-sm text-muted-foreground">
          {status
            ? `${status.version || "dev"} · ${status.platform} · 输出目录 ${status.outputDir || status.dataDir}`
            : "正在读取运行状态…"}
        </p>
      </div>

      <Card>
        <CardContent className="pt-0">
          <form onSubmit={submitSearch} className="flex flex-col gap-2 sm:flex-row">
            <Input
              value={keyword}
              onChange={(e) => setKeyword(e.target.value)}
              placeholder="搜索镜像，例如 nginx、redis、mysql:8"
              className="h-10 text-base"
              aria-label="镜像关键字"
            />
            <Button type="submit" size="lg" className="h-10 px-4">
              <Search className="h-4 w-4" /> 搜索镜像
            </Button>
          </form>
        </CardContent>
      </Card>

      <div className="grid gap-4 sm:grid-cols-3">
        <StatCard label="任务总数" value={stats ? String(stats.tasksTotal) : "…"} />
        <StatCard label="进行中" value={stats ? String(stats.tasksRunning) : "…"} />
        <StatCard label="成功" value={stats ? String(stats.tasksSucceeded) : "…"} />
        <StatCard label="失败" value={stats ? String(stats.tasksFailed) : "…"} />
        <StatCard label="已下载字节" value={stats ? formatBytes(stats.bytesTotal) : "…"} />
        <StatCard
          label="本地镜像包"
          value={stats ? `${stats.artifactsTotal} 个` : "…"}
          hint={stats ? `占用 ${formatBytes(stats.artifactsBytes)}` : undefined}
        />
      </div>

      <Card>
        <CardHeader>
          <div className="flex items-center justify-between gap-2">
            <CardTitle>最近任务</CardTitle>
            <Button variant="ghost" size="sm" render={<Link href="/tasks/" />}>
              全部任务 <ArrowRight className="h-3.5 w-3.5" />
            </Button>
          </div>
          <CardDescription>最近创建的 5 个下载任务。</CardDescription>
        </CardHeader>
        <CardContent>
          {tasks === null ? (
            <p className="py-6 text-center text-sm text-muted-foreground">加载中…</p>
          ) : recent.length === 0 ? (
            <p className="py-6 text-center text-sm text-muted-foreground">
              还没有任务 — 去
              <Link href="/search/" className="mx-1 text-primary underline underline-offset-2">
                镜像搜索
              </Link>
              创建第一个下载任务。
            </p>
          ) : (
            <ul className="grid gap-2">
              {recent.map((t) => (
                <li key={t.id} className="grid gap-1.5 rounded-lg border p-3">
                  <div className="flex items-center gap-2">
                    <span className="min-w-0 flex-1 truncate font-mono text-sm">{t.ref}</span>
                    <Badge variant={taskStatusVariant(t.status)}>{taskStatusLabel(t.status)}</Badge>
                  </div>
                  <div className="flex items-center gap-3">
                    <MiniBar value={percent(t.downloadedBytes, t.totalBytes)} />
                    <span className="shrink-0 text-xs tabular-nums text-muted-foreground">
                      {formatBytes(t.downloadedBytes)} / {formatBytes(t.totalBytes)}
                    </span>
                  </div>
                </li>
              ))}
            </ul>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <div className="flex items-center gap-2">
            <CardTitle>实时事件</CardTitle>
            <Badge variant="secondary">SSE</Badge>
          </div>
          <CardDescription>
            服务端通过 <code>GET /api/events</code> 推送的任务与镜像包事件。
          </CardDescription>
        </CardHeader>
        <CardContent>
          {feed.length === 0 ? (
            <p className="py-6 text-center text-sm text-muted-foreground">
              等待事件…（心跳每 25 秒一次）
            </p>
          ) : (
            <ScrollArea className="h-56">
              <ul className="grid gap-1.5">
                {feed.map((e) => (
                  <li
                    key={e.id}
                    className="flex items-center gap-3 rounded-md border px-3 py-1.5 text-sm"
                  >
                    <span className="shrink-0 font-mono text-xs text-muted-foreground">{e.time}</span>
                    <span className="min-w-0 flex-1 truncate">{e.text}</span>
                  </li>
                ))}
              </ul>
            </ScrollArea>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

function StatCard({ label, value, hint }: { label: string; value: string; hint?: string }) {
  return (
    <Card>
      <CardHeader>
        <CardDescription>{label}</CardDescription>
        <CardTitle className="text-2xl">{value}</CardTitle>
      </CardHeader>
      {hint && (
        <CardContent className="pt-0 text-xs text-muted-foreground">{hint}</CardContent>
      )}
    </Card>
  );
}

function MiniBar({ value }: { value: number }) {
  return (
    <div className="h-1.5 min-w-0 flex-1 overflow-hidden rounded-full bg-muted">
      <div
        className="h-full rounded-full bg-primary transition-[width] duration-300"
        style={{ width: `${value}%` }}
      />
    </div>
  );
}

function describeEvent(type: string, data: unknown): string {
  const d = data as Record<string, unknown> | undefined;
  const str = (k: string) => (typeof d?.[k] === "string" ? (d[k] as string) : "");
  switch (type) {
    case "task.created":
      return `新建任务：${str("ref") || str("id")}（${str("platform")}）`;
    case "task.finished": {
      const status = str("status");
      const err = str("error");
      return `任务结束：${str("id").slice(0, 8)} → ${taskStatusLabel(status)}${err ? ` · ${err}` : ""}`;
    }
    case "task.updated":
      return `任务更新：${str("ref") || str("id").slice(0, 8)} → ${taskStatusLabel(str("status"))}`;
    case "artifact.created":
      return `生成镜像包：${str("name")}（${formatBytes(Number(d?.size) || 0)}）`;
    case "artifact.deleted":
      return `删除镜像包：${str("id").slice(0, 8)}`;
    default:
      return type;
  }
}
