"use client";

import { Fragment, useCallback, useEffect, useState } from "react";
import Link from "next/link";
import {
  ChevronDown,
  ChevronRight,
  Pause,
  Play,
  RotateCcw,
  Trash2,
  XCircle,
} from "lucide-react";
import {
  cancelTask,
  deleteTask,
  listTasks,
  pauseTask,
  resumeTask,
  retryTask,
  subscribeEvents,
  type Envelope,
  type Task,
  type TaskFinished,
  type TaskProgress,
} from "@/lib/api";
import {
  formatBytes,
  formatSpeed,
  formatTime,
  isActiveStatus,
  layerStatusLabel,
  percent,
  taskStatusLabel,
  taskStatusVariant,
} from "@/lib/format";
import { Badge } from "@/components/ui/badge";
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
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
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
import { Label } from "@/components/ui/label";

// 进度管理 (F3 + F4): the task table. Rows are patched in place from the
// SSE stream (task.progress / task.updated / task.finished) with a 2s
// refetch as a fallback while anything is running.

export default function TasksPage() {
  const [tasks, setTasks] = useState<Task[] | null>(null);
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const [removing, setRemoving] = useState<Task | null>(null);
  const [removeFiles, setRemoveFiles] = useState(false);
  const [busyId, setBusyId] = useState("");
  const [actionError, setActionError] = useState("");

  const reload = useCallback(async () => {
    const res = await listTasks();
    if (res.ok) setTasks(res.tasks ?? []);
  }, []);

  useEffect(() => {
    (async () => {
      await reload();
    })();
  }, [reload]);

  // Patch one row in place; a task we have never seen is prepended.
  const patch = useCallback((id: string, update: (t: Task) => Task, prepend?: Task) => {
    setTasks((list) => {
      if (list === null) return list;
      if (!list.some((t) => t.id === id)) {
        return prepend ? [prepend, ...list] : list;
      }
      return list.map((t) => (t.id === id ? update(t) : t));
    });
  }, []);

  useEffect(() => {
    return subscribeEvents({
      onTask: (t) => patch(t.id, () => t, t),
      onProgress: (p: TaskProgress) => {
        patch(p.id, (t) => ({
          ...t,
          downloadedBytes: p.downloadedBytes,
          totalBytes: p.totalBytes || t.totalBytes,
          speed: p.speed,
          layers: mergeLayers(t, p),
        }));
      },
      onAny: (type, data) => {
        if (type !== "task.finished") return;
        const f = data as TaskFinished;
        patch(f.id, (t) => ({
          ...t,
          status: f.status || t.status,
          error: f.error ?? t.error,
          tarPath: f.tarPath || t.tarPath,
          speed: 0,
        }));
      },
    });
  }, [patch]);

  const hasActive = (tasks ?? []).some((t) => isActiveStatus(t.status));
  useEffect(() => {
    if (!hasActive) return;
    const id = setInterval(() => {
      reload();
    }, 2000);
    return () => clearInterval(id);
  }, [hasActive, reload]);

  async function act(id: string, fn: () => Promise<Envelope>) {
    setBusyId(id);
    setActionError("");
    try {
      const res = await fn();
      if (!res.ok) setActionError(res.error || "操作失败");
    } finally {
      setBusyId("");
      reload();
    }
  }

  return (
    <div className="grid gap-6">
      <div>
        <h1 className="font-heading text-2xl font-semibold">下载任务</h1>
        <p className="text-sm text-muted-foreground">
          任务支持暂停、续传、取消与重试；断点续传的状态保存在任务工作目录中。
        </p>
      </div>

      {actionError && <p className="text-sm text-destructive">{actionError}</p>}

      <Card>
        <CardHeader>
          <CardTitle>任务列表</CardTitle>
          <CardDescription>
            {tasks === null ? "加载中…" : `${tasks.length} 个任务`}
            {hasActive ? " · 实时更新中" : ""}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {tasks === null ? (
            <div className="grid gap-2">
              <Skeleton className="h-9 w-full" />
              <Skeleton className="h-9 w-full" />
              <Skeleton className="h-9 w-full" />
            </div>
          ) : tasks.length === 0 ? (
            <p className="py-10 text-center text-sm text-muted-foreground">
              还没有任务 — 去
              <Link href="/search/" className="mx-1 text-primary underline underline-offset-2">
                镜像搜索
              </Link>
              创建第一个下载任务。
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>镜像</TableHead>
                  <TableHead className="hidden w-28 sm:table-cell">架构</TableHead>
                  <TableHead className="w-24">状态</TableHead>
                  <TableHead className="w-56">进度</TableHead>
                  <TableHead className="hidden w-32 md:table-cell">创建时间</TableHead>
                  <TableHead className="w-40 text-right">操作</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {tasks.map((t) => (
                  <Fragment key={t.id}>
                    <TableRow data-state={expanded[t.id] ? "open" : undefined}>
                      <TableCell className="font-medium">
                        <div className="flex items-center gap-1">
                          <Button
                            variant="ghost"
                            size="icon-xs"
                            aria-label={expanded[t.id] ? "收起详情" : "展开详情"}
                            onClick={() =>
                              setExpanded((e) => ({ ...e, [t.id]: !e[t.id] }))
                            }
                          >
                            {expanded[t.id] ? (
                              <ChevronDown className="h-3.5 w-3.5" />
                            ) : (
                              <ChevronRight className="h-3.5 w-3.5" />
                            )}
                          </Button>
                          <span className="min-w-0 truncate font-mono text-xs">{t.ref}</span>
                        </div>
                        {t.status === "failed" && t.error && (
                          <p className="mt-1 pl-7 text-xs text-destructive">{t.error}</p>
                        )}
                      </TableCell>
                      <TableCell className="hidden font-mono text-xs text-muted-foreground sm:table-cell">
                        {t.platform || "—"}
                      </TableCell>
                      <TableCell>
                        <Badge variant={taskStatusVariant(t.status)}>
                          {taskStatusLabel(t.status)}
                        </Badge>
                      </TableCell>
                      <TableCell>
                        <Bar value={percent(t.downloadedBytes, t.totalBytes)} />
                        <div className="mt-1 flex items-center justify-between gap-2 text-xs tabular-nums text-muted-foreground">
                          <span>
                            {formatBytes(t.downloadedBytes)} /{" "}
                            {t.totalBytes > 0 ? formatBytes(t.totalBytes) : "未知"}
                          </span>
                          <span>{formatSpeed(t.speed)}</span>
                        </div>
                      </TableCell>
                      <TableCell className="hidden text-xs text-muted-foreground md:table-cell">
                        {formatTime(t.createdAt)}
                      </TableCell>
                      <TableCell className="text-right">
                        <div className="flex justify-end gap-0.5">
                          {(t.status === "running" || t.status === "pending") && (
                            <Button
                              variant="ghost"
                              size="icon-sm"
                              title="暂停"
                              aria-label="暂停"
                              disabled={busyId === t.id}
                              onClick={() => act(t.id, () => pauseTask(t.id))}
                            >
                              <Pause className="h-4 w-4" />
                            </Button>
                          )}
                          {(t.status === "paused" ||
                            t.status === "failed" ||
                            t.status === "canceled") && (
                            <Button
                              variant="ghost"
                              size="icon-sm"
                              title="继续"
                              aria-label="继续"
                              disabled={busyId === t.id}
                              onClick={() => act(t.id, () => resumeTask(t.id))}
                            >
                              <Play className="h-4 w-4" />
                            </Button>
                          )}
                          {(t.status === "running" ||
                            t.status === "pending" ||
                            t.status === "paused") && (
                            <Button
                              variant="ghost"
                              size="icon-sm"
                              title="取消"
                              aria-label="取消"
                              disabled={busyId === t.id}
                              onClick={() => act(t.id, () => cancelTask(t.id))}
                            >
                              <XCircle className="h-4 w-4" />
                            </Button>
                          )}
                          {(t.status === "failed" ||
                            t.status === "canceled" ||
                            t.status === "succeeded") && (
                            <Button
                              variant="ghost"
                              size="icon-sm"
                              title="重试"
                              aria-label="重试"
                              disabled={busyId === t.id}
                              onClick={() => act(t.id, () => retryTask(t.id))}
                            >
                              <RotateCcw className="h-4 w-4" />
                            </Button>
                          )}
                          <Button
                            variant="ghost"
                            size="icon-sm"
                            title="删除"
                            aria-label="删除"
                            disabled={busyId === t.id}
                            onClick={() => {
                              setRemoveFiles(false);
                              setRemoving(t);
                            }}
                          >
                            <Trash2 className="h-4 w-4 text-destructive" />
                          </Button>
                        </div>
                      </TableCell>
                    </TableRow>
                    {expanded[t.id] && (
                      <TableRow>
                        <TableCell colSpan={6} className="bg-muted/30">
                          <LayerDetail task={t} />
                        </TableCell>
                      </TableRow>
                    )}
                  </Fragment>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <AlertDialog open={!!removing} onOpenChange={(open) => !open && setRemoving(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除任务？</AlertDialogTitle>
            <AlertDialogDescription>
              「{removing?.ref}」的任务记录将被删除。可选同时删除工作目录与已生成的镜像包，此操作不可撤销。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <div className="flex items-center gap-3">
            <Switch
              id="remove-files"
              checked={removeFiles}
              onCheckedChange={(checked) => setRemoveFiles(checked)}
            />
            <Label htmlFor="remove-files">同时删除工作目录与镜像包</Label>
          </div>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <Button
              variant="destructive"
              onClick={async () => {
                const target = removing;
                setRemoving(null);
                if (target) await act(target.id, () => deleteTask(target.id, removeFiles));
              }}
            >
              删除
            </Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function Bar({ value }: { value: number }) {
  return (
    <div className="h-1.5 w-full overflow-hidden rounded-full bg-muted">
      <div
        className="h-full rounded-full bg-primary transition-[width] duration-300"
        style={{ width: `${value}%` }}
      />
    </div>
  );
}

function LayerDetail({ task }: { task: Task }) {
  const layers = task.layers ?? [];
  return (
    <div className="grid gap-2 py-1">
      <div className="flex flex-wrap gap-3 text-xs text-muted-foreground">
        <span>任务 ID：{task.id}</span>
        {task.workers > 0 && <span>并发数：{task.workers}</span>}
        {task.workDir && <span className="font-mono">工作目录：{task.workDir}</span>}
        {task.tarPath && <span className="font-mono">镜像包：{task.tarPath}</span>}
      </div>
      {layers.length === 0 ? (
        <p className="text-xs text-muted-foreground">暂无层信息。</p>
      ) : (
        <ul className="grid gap-1.5">
          {layers.map((l) => (
            <li key={l.id || l.digest} className="grid gap-1 rounded-md border bg-background px-3 py-1.5">
              <div className="flex items-center gap-2 text-xs">
                <span className="w-14 shrink-0 text-muted-foreground">
                  {l.kind === "config" ? "配置" : "层"}
                </span>
                <span className="min-w-0 flex-1 truncate font-mono">{l.name || l.digest.slice(0, 24)}</span>
                <span className="shrink-0 text-muted-foreground">{layerStatusLabel(l.status)}</span>
                <span className="w-28 shrink-0 text-right tabular-nums text-muted-foreground">
                  {formatBytes(l.downloaded)} / {formatBytes(l.size)}
                </span>
              </div>
              <Bar value={percent(l.downloaded, l.size)} />
              {l.status === "failed" && l.error && (
                <p className="text-xs text-destructive">{l.error}</p>
              )}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function mergeLayers(t: Task, p: TaskProgress): Task["layers"] {
  if (!p.layers || p.layers.length === 0) return t.layers;
  const byDigest = new Map(p.layers.map((l) => [l.digest, l]));
  const merged = t.layers.map((l) => {
    const upd = byDigest.get(l.digest);
    return upd ? { ...l, downloaded: upd.downloaded, status: upd.status } : l;
  });
  // Layers the task row does not know about yet (created mid-pull) are
  // appended with the fields the progress frame carries.
  for (const upd of p.layers) {
    if (!t.layers.some((l) => l.digest === upd.digest)) {
      merged.push({
        id: upd.digest,
        taskId: t.id,
        digest: upd.digest,
        kind: "layer",
        name: upd.digest.slice(0, 12),
        position: merged.length,
        size: 0,
        downloaded: upd.downloaded,
        status: upd.status,
        error: "",
      });
    }
  }
  return merged;
}
