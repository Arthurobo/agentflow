// paths.ts — route helpers shared by the gates, the shell and the pair page.

// normalizePath drops trailing slashes (keeping "/" itself). The static export
// is built with trailingSlash, so the same page is reachable as "/pair" and
// "/pair/", and a pathname check must treat them as one route.
export function normalizePath(pathname: string | null | undefined): string {
  const p = (pathname ?? "").replace(/\/+$/, "");
  return p === "" ? "/" : p;
}

// isRoute reports whether pathname is the given route, ignoring trailing
// slashes.
export function isRoute(pathname: string | null | undefined, route: string): boolean {
  return normalizePath(pathname) === normalizePath(route);
}

export const HOME_PATH = "/";
export const PAIR_PATH = "/pair/";

// sessionPath opens a session's terminal. id is a managed run id or an engine
// session id; the page works out which.
export function sessionPath(id: string): string {
  return `/session/?id=${encodeURIComponent(id)}`;
}

// sessionConfigPath is the "rules in effect" page for a run.
export function sessionConfigPath(id: string): string {
  return `/session/config/?id=${encodeURIComponent(id)}`;
}

// safeReturnTo accepts only a same-origin path. "//evil.example" and
// "/\evil.example" are both treated by browsers as protocol-relative URLs to
// another host, so a returnTo that starts with either would turn the pair page
// into an open redirect. Anything that is not a plain local path is dropped.
export function safeReturnTo(value: string | null | undefined): string | null {
  if (!value) return null;
  if (!value.startsWith("/")) return null;
  if (value.startsWith("//") || value.startsWith("/\\")) return null;
  // Control characters (tab, newline) are stripped by URL parsers, which
  // would let "/\t/evil.example" collapse into "//evil.example".
  for (let i = 0; i < value.length; i++) {
    const c = value.charCodeAt(i);
    if (c < 0x20 || c === 0x7f) return null;
  }
  // Sending someone back to the pair page after pairing is a loop.
  if (isRoute(value.split(/[?#]/)[0], PAIR_PATH)) return null;
  return value;
}

// pairUrlFor builds the pair page URL that remembers where the visitor was.
export function pairUrlFor(returnTo: string): string {
  const safe = safeReturnTo(returnTo);
  if (!safe || isRoute(safe.split(/[?#]/)[0], "/")) return PAIR_PATH;
  return `${PAIR_PATH}?returnTo=${encodeURIComponent(safe)}`;
}
