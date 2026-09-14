// pairing.ts — reading pairing tokens out of links and QR codes.
//
// `agentflow pair` prints a link of the form
//
//   <base>/pair/#token=<token>&expires=<unix ms>
//
// The token rides in the URL fragment because browsers never send a fragment
// to the server and never put it in a Referer header, so the secret stays out
// of request logs. Older links carried it as ?token=, which is still read.

export const EXPIRED_MESSAGE =
  "This pairing link has expired. Run `agentflow pair` on your computer to get a new one.";

export interface PairLink {
  token: string;
  // expiresAt is unix milliseconds, or null when the link did not say.
  expiresAt: number | null;
}

function fromParams(params: URLSearchParams): PairLink | null {
  const token = (params.get("token") ?? "").trim();
  if (!token) return null;
  const raw = params.get("expires");
  const n = raw === null || raw.trim() === "" ? NaN : Number(raw);
  return { token, expiresAt: Number.isFinite(n) && n > 0 ? n : null };
}

// parsePairFragment reads "#token=…&expires=…" (the leading "#" is optional).
export function parsePairFragment(hash: string): PairLink | null {
  return fromParams(new URLSearchParams(hash.replace(/^#/, "")));
}

// parsePairQuery reads the older "?token=…" form.
export function parsePairQuery(search: string): PairLink | null {
  return fromParams(new URLSearchParams(search.replace(/^\?/, "")));
}

// readPairLocation finds a token in the page's own URL: the fragment first,
// then the query string.
export function readPairLocation(loc: { hash: string; search: string }): PairLink | null {
  return parsePairFragment(loc.hash) ?? parsePairQuery(loc.search);
}

// strippedPairUrl is the address to show once the token has been read: the
// fragment is dropped and a ?token=/&expires= pair is removed from the query,
// while anything else there (returnTo) is kept.
export function strippedPairUrl(loc: { pathname: string; search: string }): string {
  const params = new URLSearchParams(loc.search.replace(/^\?/, ""));
  params.delete("token");
  params.delete("expires");
  const qs = params.toString();
  return loc.pathname + (qs ? `?${qs}` : "");
}

// A bare token as printed by `agentflow pair`: URL-safe characters only, no
// spaces, and long enough that random QR codes do not look like one.
const PLAIN_TOKEN = /^[A-Za-z0-9_~.+/=-]{16,512}$/;

// tokenFromScan reads whatever a QR code contained: a pairing link (token in
// the fragment or the query string) or a bare token.
export function tokenFromScan(data: string): PairLink | null {
  const text = data.trim();
  if (!text) return null;
  if (/^https?:\/\//i.test(text)) {
    let url: URL;
    try {
      url = new URL(text);
    } catch {
      return null;
    }
    return readPairLocation({ hash: url.hash, search: url.search });
  }
  return PLAIN_TOKEN.test(text) ? { token: text, expiresAt: null } : null;
}

export function isExpired(link: PairLink, now: number = Date.now()): boolean {
  return link.expiresAt !== null && link.expiresAt <= now;
}

// deviceLabel names a device for the person approving its access request,
// from what its browser says about itself: "iPhone · Safari". Order matters
// in both lists, because every browser claims to be several others.
export function deviceLabel(userAgent: string): string {
  const ua = userAgent || "";
  const devices: [RegExp, string][] = [
    [/iPhone/, "iPhone"],
    [/iPad/, "iPad"],
    [/Android/, /Mobile/.test(ua) ? "Android phone" : "Android tablet"],
    [/CrOS/, "Chromebook"],
    [/Macintosh|Mac OS X/, "Mac"],
    [/Windows/, "Windows PC"],
    [/Linux/, "Linux PC"],
  ];
  const browsers: [RegExp, string][] = [
    [/EdgA?\/|EdgiOS/, "Edge"],
    [/OPR\/|Opera/, "Opera"],
    [/SamsungBrowser/, "Samsung Internet"],
    [/Firefox\/|FxiOS/, "Firefox"],
    [/CriOS|Chrome\//, "Chrome"],
    [/Safari\//, "Safari"],
  ];
  const pick = (list: [RegExp, string][]) => list.find(([re]) => re.test(ua))?.[1] ?? "";
  return [pick(devices), pick(browsers)].filter(Boolean).join(" · ") || "Browser";
}
