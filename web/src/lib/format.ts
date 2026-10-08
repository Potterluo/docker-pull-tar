// format.ts — tiny presentation helpers shared by the dashboard, the task
// list and the artifact list. Kept dependency-free on purpose: these are
// the only three places that need to humanise bytes/speed/statuses.

import type { Platform } from "./api";

/** 1024-based human bytes: "0 B", "1.5 MB", "2.3 GB". */
export function formatBytes(n: number | undefined | null): string {
  if (!n || n <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i += 1;
  }
  const text = i === 0 || v >= 100 ? String(Math.round(v)) : v.toFixed(1);
  return `${text} ${units[i]}`;
}

/** Byte/sec → "1.5 MB/s"; empty when the sampler has nothing yet. */
export function formatSpeed(bytesPerSecond: number | undefined | null): string {
  if (!bytesPerSecond || bytesPerSecond <= 0) return "";
  return `${formatBytes(bytesPerSecond)}/s`;
}

/** 0–100, clamped; 0 when the total is unknown. */
export function percent(done: number | undefined, total: number | undefined): number {
  if (!total || total <= 0) return 0;
  const p = ((done ?? 0) / total) * 100;
  return Math.max(0, Math.min(100, p));
}

/** 2026/01/02 15:04 — Go's zero time renders as "—". */
export function formatTime(iso: string | undefined): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime()) || d.getFullYear() < 1970) return "—";
  return d.toLocaleString("zh-CN", {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
}

/** "linux/amd64", "linux/arm/v7" — the wire form accepted by createTask. */
export function platformString(p: Platform): string {
  return `${p.os}/${p.architecture}${p.variant ? `/${p.variant}` : ""}`;
}

const TASK_STATUS: Record<string, string> = {
  pending: "等待中",
  running: "下载中",
  paused: "已暂停",
  succeeded: "已完成",
  failed: "失败",
  canceled: "已取消",
};

export function taskStatusLabel(status: string): string {
  return TASK_STATUS[status] ?? (status || "未知");
}

export type BadgeVariant = "default" | "secondary" | "destructive" | "outline";

export function taskStatusVariant(status: string): BadgeVariant {
  switch (status) {
    case "running":
      return "default";
    case "succeeded":
      return "secondary";
    case "failed":
      return "destructive";
    default:
      return "outline";
  }
}

const LAYER_STATUS: Record<string, string> = {
  pending: "等待",
  downloading: "下载中",
  completed: "已完成",
  failed: "失败",
  skipped: "已跳过",
};

export function layerStatusLabel(status: string): string {
  return LAYER_STATUS[status] ?? (status || "未知");
}

/** Task statuses that should keep the live poller / SSE patch loop hot. */
export function isActiveStatus(status: string): boolean {
  return status === "running" || status === "pending";
}
