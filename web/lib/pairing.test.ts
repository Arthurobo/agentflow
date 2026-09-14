import { describe, expect, it } from "vitest";
import {
  deviceLabel,
  isExpired,
  parsePairFragment,
  readPairLocation,
  strippedPairUrl,
  tokenFromScan,
} from "./pairing";

const TOKEN = "3f9a1c0b7d2e4f6a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e";

describe("parsePairFragment", () => {
  it("reads the token and expiry from the link fragment", () => {
    expect(parsePairFragment(`#token=${TOKEN}&expires=1757000000000`)).toEqual({
      token: TOKEN,
      expiresAt: 1757000000000,
    });
  });

  it("accepts a fragment without the leading #", () => {
    expect(parsePairFragment(`token=${TOKEN}`)).toEqual({ token: TOKEN, expiresAt: null });
  });

  it("treats a missing or garbled expiry as unknown, not expired", () => {
    expect(parsePairFragment(`#token=${TOKEN}&expires=soon`)?.expiresAt).toBeNull();
    expect(parsePairFragment(`#token=${TOKEN}&expires=`)?.expiresAt).toBeNull();
  });

  it("finds nothing in an unrelated or empty fragment", () => {
    expect(parsePairFragment("")).toBeNull();
    expect(parsePairFragment("#section-2")).toBeNull();
    expect(parsePairFragment("#token=")).toBeNull();
  });
});

describe("readPairLocation", () => {
  it("prefers the fragment over the query string", () => {
    expect(
      readPairLocation({ hash: "#token=from-hash", search: "?token=from-query" })?.token,
    ).toBe("from-hash");
  });

  it("falls back to ?token=", () => {
    expect(readPairLocation({ hash: "", search: "?token=from-query" })?.token).toBe(
      "from-query",
    );
  });
});

describe("strippedPairUrl", () => {
  it("drops the token and expiry but keeps returnTo", () => {
    expect(
      strippedPairUrl({
        pathname: "/pair/",
        search: "?token=abc&expires=1&returnTo=%2Frun%2F",
      }),
    ).toBe("/pair/?returnTo=%2Frun%2F");
    expect(strippedPairUrl({ pathname: "/pair/", search: "" })).toBe("/pair/");
  });
});

describe("isExpired", () => {
  it("compares the expiry with now", () => {
    expect(isExpired({ token: "t", expiresAt: 1000 }, 2000)).toBe(true);
    expect(isExpired({ token: "t", expiresAt: 3000 }, 2000)).toBe(false);
    expect(isExpired({ token: "t", expiresAt: null }, 2000)).toBe(false);
  });
});

describe("tokenFromScan", () => {
  it("reads a pairing link from a remote address", () => {
    expect(
      tokenFromScan(
        `https://agentflow-3fa9c1.tail1234.ts.net/pair/#token=${TOKEN}&expires=1757000000000`,
      ),
    ).toEqual({ token: TOKEN, expiresAt: 1757000000000 });
  });

  it("reads a local pairing link", () => {
    expect(tokenFromScan(`http://127.0.0.1:4344/pair/#token=${TOKEN}`)?.token).toBe(TOKEN);
  });

  it("reads an older link with the token in the query string", () => {
    expect(tokenFromScan(`http://127.0.0.1:4344/pair/?token=${TOKEN}`)?.token).toBe(TOKEN);
  });

  it("accepts a bare token", () => {
    expect(tokenFromScan(`  ${TOKEN}\n`)).toEqual({ token: TOKEN, expiresAt: null });
  });

  it("ignores QR codes that are not pairing codes", () => {
    expect(tokenFromScan("")).toBeNull();
    expect(tokenFromScan("https://example.com/menu")).toBeNull();
    expect(tokenFromScan("hello world")).toBeNull();
    expect(tokenFromScan("WIFI:S:home;T:WPA;P:secret;;")).toBeNull();
    expect(tokenFromScan('{"token":"abc","machine":"nuc"}')).toBeNull();
  });
});

describe("deviceLabel", () => {
  it.each([
    [
      "Mozilla/5.0 (iPhone; CPU iPhone OS 18_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.5 Mobile/15E148 Safari/604.1",
      "iPhone · Safari",
    ],
    [
      "Mozilla/5.0 (iPhone; CPU iPhone OS 18_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/138.0 Mobile/15E148 Safari/604.1",
      "iPhone · Chrome",
    ],
    [
      "Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0 Mobile Safari/537.36",
      "Android phone · Chrome",
    ],
    [
      "Mozilla/5.0 (Linux; Android 14; SM-S928B) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/27.0 Chrome/125.0 Mobile Safari/537.36",
      "Android phone · Samsung Internet",
    ],
    [
      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.5 Safari/605.1.15",
      "Mac · Safari",
    ],
    [
      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0 Safari/537.36 Edg/138.0",
      "Windows PC · Edge",
    ],
    ["Mozilla/5.0 (X11; Linux x86_64; rv:141.0) Gecko/20100101 Firefox/141.0", "Linux PC · Firefox"],
  ])("names %s", (ua, want) => {
    expect(deviceLabel(ua)).toBe(want);
  });

  it("falls back to a plain label for an unknown agent", () => {
    expect(deviceLabel("")).toBe("Browser");
    expect(deviceLabel("curl/8.0")).toBe("Browser");
  });
});
