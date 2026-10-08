import type { Metadata } from "next";
import { ThemeProvider } from "@/components/theme-provider";
import { AppShell } from "@/components/app-shell";
import "./globals.css";

export const metadata: Metadata = {
  title: "DockerPull",
  description: "Docker 镜像搜索、下载与本地镜像包管理工具",
};

export default function RootLayout({
  children,
}: Readonly<{ children: React.ReactNode }>) {
  return (
    <html lang="zh-CN" suppressHydrationWarning>
      <head>
        {/* Apply the stored theme (mode + accent preset) before first
            paint — without this, users get a white flash / accent snap on
            every hard load. Keep in sync with theme-provider.tsx
            (THEME_KEY / ACCENT_KEY / DEFAULT_ACCENT). */}
        <script
          dangerouslySetInnerHTML={{
            __html: `(function(){try{var t=localStorage.getItem('app-theme');if(t==='dark'||((!t||t==='system')&&window.matchMedia('(prefers-color-scheme: dark)').matches)){document.documentElement.classList.add('dark')}var a=localStorage.getItem('app-accent');if(a&&a!=='violet'){document.documentElement.setAttribute('data-theme',a)}}catch(e){}})()`,
          }}
        />
      </head>
      <body className="antialiased">
        <ThemeProvider>
          <AppShell>{children}</AppShell>
        </ThemeProvider>
      </body>
    </html>
  );
}
