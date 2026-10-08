"use client";

import { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { Check, Copy, Download, FolderOpen, Loader2, Trash2 } from "lucide-react";
import {
  artifactURL,
  deleteArtifact,
  revealArtifact,
  listArtifacts,
  subscribeEvents,
  type Artifact,
} from "@/lib/api";
import { formatBytes, formatTime } from "@/lib/format";
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
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";

// 本地镜像包 (F5): the finished .tar files on disk. Each row carries the
// `docker load` hint — the whole point of the tool is the tar you import
// elsewhere.

export default function ArtifactsPage() {
  const [artifacts, setArtifacts] = useState<Artifact[] | null>(null);
  const [removing, setRemoving] = useState<Artifact | null>(null);
  const [copied, setCopied] = useState("");
  const [revealing, setRevealing] = useState("");
  const [error, setError] = useState("");

  const reload = useCallback(async () => {
    const res = await listArtifacts();
    if (res.ok) setArtifacts(res.artifacts ?? []);
    else setError(res.error || "读取镜像包列表失败");
  }, []);

  useEffect(() => {
    (async () => {
      await reload();
    })();
  }, [reload]);

  // A finished pull (or a scan) shows up without a manual refresh.
  useEffect(() => {
    return subscribeEvents({
      onAny: (type) => {
        if (type === "artifact.created" || type === "artifact.deleted" || type === "task.finished") {
          reload();
        }
      },
    });
  }, [reload]);

  // Opening the folder is a server call, so it can fail (the file may be gone,
  // or the launcher missing) and must surface that instead of looking dead.
  async function reveal(id: string) {
    setError("");
    setRevealing(id);
    try {
      const res = await revealArtifact(id);
      if (!res.ok) setError(res.error || "无法打开文件夹");
    } finally {
      setRevealing("");
    }
  }

  async function copyCommand(a: Artifact) {
    const cmd = `docker load -i ${a.path}`;
    try {
      await navigator.clipboard.writeText(cmd);
      setCopied(a.id);
      setTimeout(() => setCopied((c) => (c === a.id ? "" : c)), 1500);
    } catch {
      setError("复制失败，请手动复制");
    }
  }

  const totalBytes = (artifacts ?? []).reduce((sum, a) => sum + (a.size || 0), 0);

  return (
    <div className="grid gap-6">
      <div>
        <h1 className="font-heading text-2xl font-semibold">本地镜像包</h1>
        <p className="text-sm text-muted-foreground">
          下载完成的 <code>.tar</code> 文件。在目标机器上执行每行给出的 <code>docker load</code> 命令即可导入。
        </p>
      </div>

      {error && <p className="text-sm text-destructive">{error}</p>}

      <Card>
        <CardHeader>
          <CardTitle>镜像包列表</CardTitle>
          <CardDescription>
            {artifacts === null
              ? "加载中…"
              : `${artifacts.length} 个文件 · 合计 ${formatBytes(totalBytes)}`}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {artifacts === null ? (
            <div className="grid gap-2">
              <Skeleton className="h-9 w-full" />
              <Skeleton className="h-9 w-full" />
              <Skeleton className="h-9 w-full" />
            </div>
          ) : artifacts.length === 0 ? (
            <p className="py-10 text-center text-sm text-muted-foreground">
              还没有镜像包 — 去
              <Link href="/search/" className="mx-1 text-primary underline underline-offset-2">
                镜像搜索
              </Link>
              下载一个镜像。
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>文件名</TableHead>
                  <TableHead className="hidden sm:table-cell">镜像</TableHead>
                  <TableHead className="hidden w-28 md:table-cell">架构</TableHead>
                  <TableHead className="w-24 text-right">大小</TableHead>
                  <TableHead className="hidden w-32 md:table-cell">创建时间</TableHead>
                  <TableHead className="w-40 text-right">操作</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {artifacts.map((a) => (
                  <TableRow key={a.id}>
                    <TableCell className="max-w-64">
                      <p className="truncate font-mono text-xs">{a.name}</p>
                      <button
                        type="button"
                        onClick={() => copyCommand(a)}
                        title="复制 docker load 命令"
                        className="mt-1 flex max-w-full items-center gap-1 rounded text-xs text-muted-foreground hover:text-foreground"
                      >
                        {copied === a.id ? (
                          <Check className="h-3 w-3 shrink-0 text-primary" />
                        ) : (
                          <Copy className="h-3 w-3 shrink-0" />
                        )}
                        <span className="truncate font-mono">docker load -i {a.path}</span>
                      </button>
                    </TableCell>
                    <TableCell className="hidden font-mono text-xs sm:table-cell">
                      {a.repository}
                      {a.tag ? `:${a.tag}` : ""}
                    </TableCell>
                    <TableCell className="hidden font-mono text-xs text-muted-foreground md:table-cell">
                      {a.platform || "—"}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">{formatBytes(a.size)}</TableCell>
                    <TableCell className="hidden text-xs text-muted-foreground md:table-cell">
                      {formatTime(a.createdAt)}
                    </TableCell>
                    <TableCell className="text-right">
                      <div className="flex justify-end gap-0.5">
                        <Button
                          variant="ghost"
                          size="icon-sm"
                          title="下载"
                          aria-label="下载"
                          render={
                            <a href={artifactURL(a.id)} download={a.name} />
                          }
                        >
                          <Download className="h-4 w-4" />
                        </Button>
                        <Button
                          variant="ghost"
                          size="icon-sm"
                          title="打开所在文件夹"
                          aria-label="打开所在文件夹"
                          disabled={revealing === a.id}
                          onClick={() => void reveal(a.id)}
                        >
                          {revealing === a.id ? (
                            <Loader2 className="h-4 w-4 animate-spin" />
                          ) : (
                            <FolderOpen className="h-4 w-4" />
                          )}
                        </Button>
                        <Button
                          variant="ghost"
                          size="icon-sm"
                          title="删除"
                          aria-label="删除"
                          onClick={() => setRemoving(a)}
                        >
                          <Trash2 className="h-4 w-4 text-destructive" />
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <AlertDialog open={!!removing} onOpenChange={(open) => !open && setRemoving(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除镜像包？</AlertDialogTitle>
            <AlertDialogDescription>
              「{removing?.name}」将从磁盘删除，任务记录会保留。此操作不可撤销。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <Button
              variant="destructive"
              onClick={async () => {
                const target = removing;
                setRemoving(null);
                if (!target) return;
                const res = await deleteArtifact(target.id);
                if (!res.ok) setError(res.error || "删除失败");
                reload();
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
