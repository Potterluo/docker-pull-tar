"use client";

import { useState } from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import {
  ArrowDownToLine,
  KeyRound,
  LayoutDashboard,
  Menu,
  Moon,
  Package,
  Search,
  Settings,
  Sun,
  X,
  // --- gen:nav-icons ---
} from "lucide-react";
import { useTheme } from "./theme-provider";
import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";

// AppShell: sidebar navigation + topbar. There is no auth in this product
// (single-user local tool), so nothing here probes a session — the shell
// renders unconditionally.

interface NavItem {
  href: string;
  label: string;
  icon: React.ComponentType<{ className?: string }>;
}

const NAV: { section: string; items: NavItem[] }[] = [
  {
    section: "Main",
    items: [
      { href: "/", label: "概览", icon: LayoutDashboard },
      { href: "/search/", label: "镜像搜索", icon: Search },
      { href: "/tasks/", label: "下载任务", icon: ArrowDownToLine },
      { href: "/artifacts/", label: "本地镜像包", icon: Package },
      { href: "/credentials/", label: "凭证管理", icon: KeyRound },
      // --- gen:nav ---
    ],
  },
  {
    section: "设置",
    items: [{ href: "/settings/", label: "设置", icon: Settings }],
  },
];

// Every registered href, longest first — used to make prefix matching
// unambiguous (/settings must not also light up a longer sibling path).
const NAV_HREFS = NAV.flatMap((g) => g.items.map((i) => i.href.replace(/\/$/, ""))).sort(
  (a, b) => b.length - a.length
);

function isActive(pathname: string, href: string): boolean {
  const p = pathname.replace(/\/$/, "") || "/";
  const h = href.replace(/\/$/, "") || "/";
  if (p === h) return true;
  return p.startsWith(h + "/") && !NAV_HREFS.some((o) => o.length > h.length && p.startsWith(o));
}

export function AppShell({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  const [mobileOpen, setMobileOpen] = useState(false);

  const sidebar = (
    <nav className="flex h-full flex-col gap-6 p-4">
      <Link href="/" className="flex items-center gap-2 px-2 pt-2">
        <img src="/icon.svg" alt="" className="h-8 w-8 rounded-lg shadow-sm" />
        <span className="font-heading text-base font-semibold">DockerPull</span>
      </Link>
      {NAV.map((group) => (
        <div key={group.section}>
          <p className="mb-1 px-2 text-xs font-medium text-muted-foreground">{group.section}</p>
          <ul className="grid gap-0.5">
            {group.items.map((it) => (
              <li key={it.href}>
                <Link
                  href={it.href}
                  onClick={() => setMobileOpen(false)}
                  className={cn(
                    "flex items-center gap-2.5 rounded-md px-2 py-1.5 text-sm transition-colors",
                    isActive(pathname, it.href)
                      ? "bg-sidebar-accent font-medium text-sidebar-accent-foreground"
                      : "text-muted-foreground hover:bg-sidebar-accent/60 hover:text-sidebar-accent-foreground"
                  )}
                >
                  <it.icon className="h-4 w-4" />
                  {it.label}
                </Link>
              </li>
            ))}
          </ul>
        </div>
      ))}
    </nav>
  );

  return (
    <div className="flex min-h-screen">
      {/* Desktop sidebar */}
      <aside className="sticky top-0 hidden h-screen w-60 shrink-0 border-r bg-sidebar text-sidebar-foreground md:block">
        {sidebar}
      </aside>

      {/* Mobile sidebar */}
      {mobileOpen && (
        <div className="fixed inset-0 z-50 md:hidden">
          <div
            className="absolute inset-0 bg-black/40"
            onClick={() => setMobileOpen(false)}
          />
          <aside className="absolute left-0 top-0 h-full w-64 border-r bg-sidebar">
            <button
              className="absolute right-2 top-2 rounded p-1.5 text-muted-foreground hover:bg-accent"
              onClick={() => setMobileOpen(false)}
              aria-label="关闭菜单"
            >
              <X className="h-4 w-4" />
            </button>
            {sidebar}
          </aside>
        </div>
      )}

      <div className="flex min-w-0 flex-1 flex-col">
        <Topbar onMenu={() => setMobileOpen(true)} />
        <main className="mx-auto w-full max-w-5xl flex-1 p-4 md:p-6">{children}</main>
      </div>
    </div>
  );
}

function Topbar({ onMenu }: { onMenu: () => void }) {
  const { resolvedTheme, setTheme } = useTheme();

  return (
    <header className="sticky top-0 z-40 flex h-14 items-center gap-3 border-b bg-background/80 px-4 backdrop-blur">
      <Button variant="ghost" size="icon" className="md:hidden" onClick={onMenu} aria-label="打开菜单">
        <Menu className="h-5 w-5" />
      </Button>
      <span className="font-heading text-sm font-semibold md:hidden">DockerPull</span>
      <div className="flex-1" />
      <Button
        variant="ghost"
        size="icon"
        onClick={() => setTheme(resolvedTheme === "dark" ? "light" : "dark")}
        aria-label="切换深浅色"
        title="切换深浅色"
      >
        {resolvedTheme === "dark" ? <Sun className="h-4.5 w-4.5" /> : <Moon className="h-4.5 w-4.5" />}
      </Button>
      <Button
        variant="ghost"
        size="sm"
        className="gap-1.5"
        render={<Link href="/settings/" />}
        aria-label="设置"
      >
        <Settings className="h-4 w-4" />
        <span className="hidden sm:inline">设置</span>
      </Button>
    </header>
  );
}
