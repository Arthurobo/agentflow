// stamp-sw.mjs — runs after `next build`. Writes the build id into the
// service worker's cache name in out/sw.js, so every release gets its own
// cache and the previous one is deleted when the new worker activates.
//
// Usage: node scripts/stamp-sw.mjs [webRoot]   (defaults to this package)
import { existsSync, readdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

export const PLACEHOLDER = "__BUILD_ID__";

// findBuildId prefers .next/BUILD_ID and falls back to the one directory
// under out/_next/static that is not a shared asset folder.
export function findBuildId(root) {
  const idFile = join(root, ".next", "BUILD_ID");
  if (existsSync(idFile)) {
    const id = readFileSync(idFile, "utf8").trim();
    if (id) return id;
  }
  const staticDir = join(root, "out", "_next", "static");
  if (existsSync(staticDir)) {
    const shared = new Set(["chunks", "css", "media", "webpack"]);
    const candidates = readdirSync(staticDir).filter(
      (name) => !shared.has(name) && statSync(join(staticDir, name)).isDirectory(),
    );
    if (candidates.length === 1) return candidates[0];
  }
  return "";
}

export function stampServiceWorker(source, buildId) {
  if (!/^[A-Za-z0-9_-]+$/.test(buildId)) {
    throw new Error(`refusing to stamp an unexpected build id: ${JSON.stringify(buildId)}`);
  }
  if (!source.includes(PLACEHOLDER)) {
    throw new Error(`service worker has no ${PLACEHOLDER} placeholder to stamp`);
  }
  return source.split(PLACEHOLDER).join(buildId);
}

function main() {
  const here = dirname(fileURLToPath(import.meta.url));
  const root = resolve(process.argv[2] ?? join(here, ".."));
  const swPath = join(root, "out", "sw.js");
  if (!existsSync(swPath)) {
    throw new Error(`${swPath} not found — run next build first`);
  }
  const buildId = findBuildId(root);
  if (!buildId) throw new Error("could not determine the Next build id");
  writeFileSync(swPath, stampServiceWorker(readFileSync(swPath, "utf8"), buildId));
  console.log(`stamped out/sw.js with build ${buildId}`);
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    main();
  } catch (err) {
    console.error(`stamp-sw: ${err instanceof Error ? err.message : err}`);
    process.exit(1);
  }
}
