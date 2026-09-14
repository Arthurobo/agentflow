import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // The daemon embeds and serves the build as a static export. A static
  // export can't build unknown [id] segments, so pages that need an id take
  // it as a query parameter (/session/?id=…). images:unoptimized is required
  // because there is no image optimization server.
  output: "export",
  trailingSlash: true,
  images: { unoptimized: true },
};

export default nextConfig;