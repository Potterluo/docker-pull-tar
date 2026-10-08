import type { NextConfig } from "next";

// The frontend is a pure static export (output: "export"): `next build`
// emits plain HTML/JS/CSS into web/out, which the Go binary embeds and
// serves same-origin. Consequences worth knowing:
//
//   - No server-side Next features: no SSR, no API routes, no next
//     middleware, no rewrites. All pages are client components or
//     statically prerendered shells.
//   - Same-origin with the API: apiFetch uses relative URLs with no CORS
//     setup and no base URL.
//   - Development: run the Go server with APP_DEV_PROXY=http://localhost:3000
//     and `pnpm dev` — the Go process proxies non-API traffic to the dev
//     server, so HMR works against the real backend.
const nextConfig: NextConfig = {
  output: "export",
  trailingSlash: true,
  images: {
    unoptimized: true,
  },
};

export default nextConfig;
