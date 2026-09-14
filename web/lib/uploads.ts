// uploads.ts — the phone's side of an image attachment.
//
// Two jobs: make the picture smaller before it goes anywhere, and POST it to
// the daemon over the same authenticated channel every other request uses.
// There is no peer-to-peer path: with no relay in the middle, the connection
// the UI already loaded over is the private channel.
//
// Normalizing on the client is not only about bandwidth. Re-encoding through
// a canvas drops EXIF — which is where phones put GPS coordinates — and it
// turns an iPhone's HEIC into something every engine can actually read.

import { agentdBase, getDeviceToken } from "@/lib/agentd";

// The longest side after downscaling. Big enough to read UI text in a
// screenshot, small enough that a phone photo stops being a 4MB transfer.
export const MAX_EDGE = 1600;

export const ALLOWED_TYPES = ["image/png", "image/jpeg", "image/webp", "image/gif"];

export interface NormalizedImage {
  blob: Blob;
  mime: string;
  name: string;
  width: number;
  height: number;
  sha256: string;
  // previewUrl is an object URL for the chip thumbnail; revoke when done.
  previewUrl: string;
}

// hasTransparency samples the decoded image for any non-opaque pixel.
// Screenshots with rounded corners or shadows lose their edges as JPEG, so
// they stay PNG; a photograph does not need an alpha channel it never had.
function hasTransparency(ctx: CanvasRenderingContext2D, w: number, h: number): boolean {
  try {
    const data = ctx.getImageData(0, 0, w, h).data;
    // Every 40th pixel: enough to catch a transparent region, cheap enough
    // to run on a phone.
    for (let i = 3; i < data.length; i += 4 * 40) {
      if (data[i] < 255) return true;
    }
  } catch {
    // A tainted canvas cannot be sampled; assume opaque.
  }
  return false;
}

async function sha256Hex(buf: ArrayBuffer): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", buf);
  return Array.from(new Uint8Array(digest))
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");
}

function renamed(name: string, mime: string): string {
  const base = (name || "image").replace(/\.[^.]+$/, "");
  return base + (mime === "image/png" ? ".png" : ".jpg");
}

// normalizeImage decodes, downscales and re-encodes one picked file.
export async function normalizeImage(file: File): Promise<NormalizedImage> {
  const bitmap = await createImageBitmap(file);
  const scale = Math.min(1, MAX_EDGE / Math.max(bitmap.width, bitmap.height));
  const width = Math.max(1, Math.round(bitmap.width * scale));
  const height = Math.max(1, Math.round(bitmap.height * scale));

  const canvas = document.createElement("canvas");
  canvas.width = width;
  canvas.height = height;
  const ctx = canvas.getContext("2d");
  if (!ctx) throw new Error("This browser cannot process images");
  ctx.drawImage(bitmap, 0, 0, width, height);
  bitmap.close?.();

  const png = hasTransparency(ctx, width, height);
  const mime = png ? "image/png" : "image/jpeg";
  const blob = await new Promise<Blob | null>((resolve) =>
    canvas.toBlob(resolve, mime, png ? undefined : 0.85),
  );
  if (!blob) throw new Error("Could not encode the image");

  const buf = await blob.arrayBuffer();
  return {
    blob,
    mime,
    name: renamed(file.name, mime),
    width,
    height,
    sha256: await sha256Hex(buf),
    previewUrl: URL.createObjectURL(blob),
  };
}

// --- transfer ------------------------------------------------------------

// UploadHandle is one in-flight upload: a promise that resolves with the
// server-side attachment id, and a cancel that aborts the request.
export interface UploadHandle {
  done: Promise<string>;
  cancel: () => void;
}

// Reasons the server can refuse, in words a person can act on. The keys are
// the {error:{code}} values POST .../uploads returns.
const REASONS: Record<string, string> = {
  type: "That file type is not supported",
  too_large: "That image is too large",
  quota: "This session's attachment limit is full",
  rate: "Too many uploads just now — try again in a moment",
  hash_mismatch: "The image did not arrive intact",
  no_session: "The session is still starting — try again in a moment",
  bad_request: "The upload was cut off",
  internal: "Your PC could not save the image",
};

export function reasonText(reason?: string): string {
  return (reason && REASONS[reason]) || "The upload failed";
}

// uploadImage POSTs one normalized image to the run's upload endpoint and
// resolves with its attachment id, which the caller then names in a prompt
// turn. It rides the same device-token auth as every other call; the bytes
// travel over the connection the UI already loaded, so there is nothing to
// set up and nothing to be "up" first.
export function uploadImage(
  runId: string,
  image: NormalizedImage,
  onProgress?: (sent: number, total: number) => void,
): UploadHandle {
  const xhr = new XMLHttpRequest();

  const done = new Promise<string>((resolve, reject) => {
    const form = new FormData();
    // name and sha256 first so the server has them before the file part.
    form.append("name", image.name);
    form.append("sha256", image.sha256);
    form.append("image", image.blob, image.name);

    xhr.open(
      "POST",
      agentdBase + `/api/v1/agentd/sessions/${encodeURIComponent(runId)}/uploads`,
    );
    const tok = getDeviceToken();
    if (tok) xhr.setRequestHeader("Authorization", `Bearer ${tok}`);

    if (onProgress) {
      xhr.upload.onprogress = (ev) => {
        if (ev.lengthComputable) onProgress(ev.loaded, ev.total);
      };
    }

    xhr.onload = () => {
      let body: { attachmentId?: string; error?: { code?: string } } = {};
      try {
        body = JSON.parse(xhr.responseText) as typeof body;
      } catch {
        /* non-JSON body */
      }
      if (xhr.status === 200 && body.attachmentId) {
        resolve(body.attachmentId);
      } else {
        reject(new Error(reasonText(body.error?.code)));
      }
    };
    xhr.onerror = () =>
      reject(new Error("agentflow is unreachable — is the daemon running?"));
    xhr.onabort = () => reject(new Error("Upload cancelled"));

    xhr.send(form);
  });

  return {
    done,
    cancel() {
      xhr.abort();
    },
  };
}
