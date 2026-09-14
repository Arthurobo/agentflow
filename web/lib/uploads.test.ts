// uploads.test.ts — the phone's side of an attachment.
//
// jsdom has no real canvas encoder, so normalization is asserted elsewhere by
// its decisions. The interesting half here is the transfer: the image is
// POSTed to the daemon over the same authenticated channel as every other
// request, so these tests drive a fake XMLHttpRequest and assert the request
// shape, the id it resolves with, the errors it surfaces, and cancel.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { MAX_EDGE, reasonText, uploadImage, type NormalizedImage } from "./uploads";

// --- a fake XMLHttpRequest ----------------------------------------------

class FakeXHR {
  static last: FakeXHR | null = null;
  method = "";
  url = "";
  headers: Record<string, string> = {};
  body: FormData | null = null;
  status = 0;
  responseText = "";
  aborted = false;
  upload: {
    onprogress:
      | ((ev: { lengthComputable: boolean; loaded: number; total: number }) => void)
      | null;
  } = { onprogress: null };
  onload: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onabort: (() => void) | null = null;

  constructor() {
    FakeXHR.last = this;
  }
  open(method: string, url: string) {
    this.method = method;
    this.url = url;
  }
  setRequestHeader(k: string, v: string) {
    this.headers[k] = v;
  }
  send(body: FormData) {
    this.body = body;
  }
  abort() {
    this.aborted = true;
    this.onabort?.();
  }
  // test helpers
  succeed(attachmentId: string) {
    this.status = 200;
    this.responseText = JSON.stringify({ attachmentId });
    this.onload?.();
  }
  fail(status: number, code: string) {
    this.status = status;
    this.responseText = JSON.stringify({ error: { code } });
    this.onload?.();
  }
  network() {
    this.onerror?.();
  }
  progress(loaded: number, total: number) {
    this.upload.onprogress?.({ lengthComputable: true, loaded, total });
  }
}

function fakeImage(bytes: number): NormalizedImage {
  const blob = new Blob([new Uint8Array(bytes)], { type: "image/png" });
  return {
    blob,
    mime: "image/png",
    name: "shot.png",
    width: 800,
    height: 600,
    sha256: "abc123",
    previewUrl: "blob:preview",
  };
}

const settle = async (n = 4) => {
  for (let i = 0; i < n; i++) await Promise.resolve();
};

beforeEach(() => {
  vi.stubGlobal("XMLHttpRequest", FakeXHR as unknown as typeof XMLHttpRequest);
  // getDeviceToken() reads the token from localStorage; a stub keeps the
  // Authorization header assertion honest without a real browser store.
  vi.stubGlobal("localStorage", {
    getItem: () => "test-token",
    setItem: () => {},
    removeItem: () => {},
  });
});
afterEach(() => vi.unstubAllGlobals());

describe("the HTTP upload", () => {
  it("POSTs to the run's upload endpoint with name + sha256 and the bearer token", async () => {
    uploadImage("run-1", fakeImage(40 * 1024));
    await settle();
    const xhr = FakeXHR.last!;
    expect(xhr.method).toBe("POST");
    expect(xhr.url).toContain("/api/v1/agentd/sessions/run-1/uploads");
    expect(xhr.headers.Authorization).toBe("Bearer test-token");
    const form = xhr.body!;
    expect(form.get("name")).toBe("shot.png");
    expect(form.get("sha256")).toBe("abc123");
    expect(form.get("image")).toBeTruthy();
  });

  it("resolves with the server's attachment id on success", async () => {
    const handle = uploadImage("run-1", fakeImage(1024));
    await settle();
    FakeXHR.last!.succeed("att-9");
    await expect(handle.done).resolves.toBe("att-9");
  });

  it("reports progress as it uploads", async () => {
    const seen: number[] = [];
    uploadImage("run-1", fakeImage(1000), (sent, total) => seen.push(sent / total));
    await settle();
    FakeXHR.last!.progress(500, 1000);
    FakeXHR.last!.progress(1000, 1000);
    expect(seen).toEqual([0.5, 1]);
  });

  it("turns a server refusal into a message a person can act on", async () => {
    const handle = uploadImage("run-1", fakeImage(1024));
    await settle();
    FakeXHR.last!.fail(413, "too_large");
    await expect(handle.done).rejects.toThrow(/too large/i);
  });

  it("surfaces a network failure as a reachability message", async () => {
    const handle = uploadImage("run-1", fakeImage(1024));
    await settle();
    FakeXHR.last!.network();
    await expect(handle.done).rejects.toThrow(/unreachable/i);
  });

  it("cancels cleanly, aborting the request", async () => {
    const handle = uploadImage("run-1", fakeImage(1024));
    await settle();
    handle.cancel();
    expect(FakeXHR.last!.aborted).toBe(true);
    await expect(handle.done).rejects.toThrow(/cancelled/i);
  });
});

describe("server reasons", () => {
  it("are rendered as something a person can act on", () => {
    expect(reasonText("quota")).toMatch(/limit is full/i);
    expect(reasonText("rate")).toMatch(/try again/i);
    expect(reasonText("type")).toMatch(/not supported/i);
    expect(reasonText("hash_mismatch")).toMatch(/intact/i);
    expect(reasonText("no_session")).toMatch(/still starting/i);
    // An unknown reason still says something rather than showing a code.
    expect(reasonText("something-new")).toBe("The upload failed");
    expect(reasonText(undefined)).toBe("The upload failed");
  });
});

describe("normalization limits", () => {
  it("targets a long edge small enough to send and large enough to read", () => {
    expect(MAX_EDGE).toBe(1600);
  });
});
