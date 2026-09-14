"use client";

// /pair/ — the device registration page. Four ways in:
//
//   1. The link from `agentflow pair` (opened directly, or by the phone's own
//      camera app from the QR): the token is in the URL fragment. It is read,
//      stripped from the address bar at once, and submitted.
//   2. Request access: no token at all; the computer approves the request.
//   3. The in-page camera button: a scanned link or bare token fills the field.
//   4. A token pasted by hand.
//
// On success the single-use pairing token is consumed, a revocable device
// token is stored, and the visitor goes back to where they were headed.
import { useCallback, useEffect, useRef, useState } from "react";
import { useRouter } from "next/navigation";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Camera,
  KeyRound,
  RefreshCcw,
  ShieldCheck,
  Smartphone,
  Trash2,
} from "lucide-react";
import QRCode from "jsqr";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import {
  EmptyState,
  Field,
  PageHeader,
  PageShell,
  SectionHeader,
} from "@/components/ui/surface";
import {
  completePair as pairComplete,
  listDevices,
  refreshDeviceCookie,
  revokeDevice,
  setDeviceToken,
  agentdReady,
  AgentdError,
} from "@/lib/agentd";
import { formatTs } from "@/lib/format";
import { AccessRequests } from "@/features/pair/access-requests";
import { RequestAccess } from "@/features/pair/request-access";
import { HOME_PATH, isRoute, PAIR_PATH, safeReturnTo } from "@/lib/paths";
import {
  EXPIRED_MESSAGE,
  isExpired,
  readPairLocation,
  strippedPairUrl,
  tokenFromScan,
} from "@/lib/pairing";

// destination is where a freshly paired device goes: the page it was sent
// here from, when that is a safe local path, otherwise the sessions list.
function destination(): string {
  if (typeof window === "undefined") return HOME_PATH;
  const returnTo = new URLSearchParams(window.location.search).get("returnTo");
  return safeReturnTo(returnTo) ?? HOME_PATH;
}

export default function PairPage() {
  const queryClient = useQueryClient();
  const router = useRouter();
  const [token, setToken] = useState("");
  const [name, setName] = useState("");
  const [status, setStatus] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [repaired, setRepaired] = useState(agentdReady());
  const videoRef = useRef<HTMLVideoElement>(null);
  const [scanning, setScanning] = useState(false);
  // paired flips the instant the token is stored, and stays true through the
  // navigation — the button says so while Next takes over.
  const [paired, setPaired] = useState(false);
  // autoSubmitted guards the link-driven pair. React StrictMode (and any
  // re-render that re-fires the effect) would otherwise submit twice, burning
  // the single-use token on the first attempt and failing the second.
  const autoSubmitted = useRef(false);

  // Warm the destination so the redirect is a paint, not a fetch. Once on
  // mount, and again as soon as there is a token to submit (a scanned QR
  // fills the field, so that is the real "about to pair" moment).
  useEffect(() => {
    router.prefetch(destination());
  }, [router]);
  useEffect(() => {
    if (token.trim()) router.prefetch(destination());
  }, [token, router]);

  // A browser that already holds a device token makes sure it also has the
  // cookie the public address wants before it serves the app.
  useEffect(() => {
    if (agentdReady()) void refreshDeviceCookie().catch(() => {});
  }, []);

  const { data: devices, refetch } = useQuery({
    queryKey: ["devices"],
    queryFn: () => listDevices(),
    enabled: repaired,
  });

  // leave goes where a freshly paired device is headed.
  const leave = useCallback(() => {
    // Nothing on the way out waits on the devices list. It is invalidated
    // so it is fresh for anyone who comes back, but not awaited.
    void queryClient.invalidateQueries({ queryKey: ["devices"] });
    const dest = destination();
    router.replace(dest);
    // Hard-nav fallback: if router.replace is swallowed (a stale service
    // worker, a routing race) the user would otherwise be stranded here.
    setTimeout(() => {
      if (isRoute(window.location.pathname, PAIR_PATH)) {
        window.location.assign(dest);
      }
    }, 600);
  }, [queryClient, router]);

  const onAccessApproved = useCallback(() => {
    setRepaired(true);
    setPaired(true);
    leave();
  }, [leave]);

  const handlePair = async (overrideToken?: string) => {
    const tok = (overrideToken ?? token).trim();
    if (!tok || busy) return;
    setBusy(true);
    setError(null);
    setStatus(null);
    try {
      const res = await pairComplete({
        token: tok,
        name: name.trim() || undefined,
      });
      setDeviceToken(res.deviceToken);
      setRepaired(true);
      setPaired(true);
      setToken("");
      // Navigate the moment the token is in hand.
      leave();
    } catch (e) {
      setError(e instanceof AgentdError ? e.message : "pairing failed");
      setPaired(false);
      setBusy(false);
    }
  };

  // Read a token from the page's own URL, strip it from the address bar
  // before anything else can see it (history, a screenshot, a shared tab),
  // then pair — unless the link says it has already expired.
  /* eslint-disable react-hooks/set-state-in-effect */
  useEffect(() => {
    if (autoSubmitted.current) return;
    const link = readPairLocation(window.location);
    if (!link) return;
    autoSubmitted.current = true;
    window.history.replaceState(window.history.state, "", strippedPairUrl(window.location));
    if (isExpired(link)) {
      setError(EXPIRED_MESSAGE);
      return;
    }
    void handlePair(link.token);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  /* eslint-enable react-hooks/set-state-in-effect */

  const handleRevoke = async (id: string) => {
    try {
      await revokeDevice(id);
      await refetch();
    } catch (e) {
      setError(e instanceof Error ? e.message : "revoke failed");
    }
  };

  // In-page camera scan. Browsers only allow the camera on HTTPS or
  // localhost, which is where the pair page is served.
  const startScan = async () => {
    if (scanning) return;
    setError(null);
    if (!navigator.mediaDevices?.getUserMedia) {
      setError("Camera not available — paste the token manually.");
      return;
    }
    try {
      const stream = await navigator.mediaDevices.getUserMedia({
        video: { facingMode: "environment" },
      });
      const video = videoRef.current;
      if (!video) {
        stream.getTracks().forEach((t) => t.stop());
        return;
      }
      video.srcObject = stream;
      await video.play();
      setScanning(true);
      const loop = () => {
        if (!video.readyState || video.videoWidth <= 0) {
          requestAnimationFrame(loop);
          return;
        }
        const canvas = document.createElement("canvas");
        canvas.width = video.videoWidth;
        canvas.height = video.videoHeight;
        const ctx = canvas.getContext("2d");
        if (!ctx) return;
        ctx.drawImage(video, 0, 0, canvas.width, canvas.height);
        const data = ctx.getImageData(0, 0, canvas.width, canvas.height);
        const code = QRCode(data.data, data.width, data.height);
        const link = code?.data ? tokenFromScan(code.data) : null;
        if (link) {
          setScanning(false);
          stream.getTracks().forEach((t) => t.stop());
          if (isExpired(link)) {
            setError(EXPIRED_MESSAGE);
          } else {
            setToken(link.token);
            setStatus("QR code scanned — tap Pair device.");
          }
          return;
        }
        requestAnimationFrame(loop);
      };
      requestAnimationFrame(loop);
    } catch {
      setError("Camera denied or unavailable — paste the token manually.");
    }
  };

  useEffect(
    () => () => {
      const stream = videoRef.current?.srcObject as MediaStream | null;
      if (stream) stream.getTracks().forEach((t) => t.stop());
    },
    [],
  );

  return (
    <PageShell className="max-w-xl pt-8">
      <PageHeader
        eyebrow="Device trust"
        title={
          <span className="flex items-center gap-2">
            <ShieldCheck className="size-6" /> Pair this device
          </span>
        }
        description={
          <>
            Run <code className="bg-muted rounded px-1">agentflow pair</code> on
            your computer, then open the link it prints, scan its QR code, or
            paste the token here. No link? Request access and approve it on the
            computer.
          </>
        }
      />

      {!paired && (
        <div className="af-panel mt-5 p-4">
          <RequestAccess onPaired={onAccessApproved} />
        </div>
      )}

      <div className="af-panel mt-4 p-4">
        <div className="flex flex-col gap-2">
          <SectionHeader
            eyebrow="Pairing token"
            title="Trust this browser"
            description="A pairing token works once and expires after a few minutes. Every paired device can be revoked below."
          />
          <Field label="Token">
            <div className="flex items-center gap-2">
              <Input
                value={token}
                onChange={(e) => setToken(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") {
                    e.preventDefault();
                    void handlePair();
                  }
                }}
                placeholder="paste token from `agentflow pair`"
                className="font-mono"
              />
              <Button
                variant="outline"
                size="icon"
                className="shrink-0"
                onClick={() => void startScan()}
                title="Scan QR"
              >
                {scanning ? (
                  <RefreshCcw className="size-4" />
                ) : (
                  <Camera className="size-4" />
                )}
              </Button>
            </div>
          </Field>
          {/* Always mounted: startScan needs the element before it flips
              `scanning`, so a conditionally rendered video was never there. */}
          <video
            ref={videoRef}
            hidden={!scanning}
            className="h-40 w-full rounded-md bg-black"
            muted
            playsInline
          />
          <Field label="Device name" className="max-w-xs">
            <Input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="my-phone"
            />
          </Field>
          {/* The button IS the progress. A separate success paragraph
              invites reading, and the whole point is that there is nothing
              to read: the console is already opening. */}
          <Button
            size="lg"
            onClick={() => void handlePair()}
            disabled={busy || !token.trim()}
            data-state={paired ? "paired" : busy ? "pairing" : "idle"}
          >
            <KeyRound className="size-4" />
            {paired
              ? "Paired — opening console"
              : busy
                ? "Pairing…"
                : "Pair device"}
          </Button>
          {status && <p className="text-success text-xs">{status}</p>}
          {error && (
            <p role="alert" className="text-destructive text-xs">
              {error}
            </p>
          )}
        </div>
      </div>

      <div className="af-subtle-panel mt-4 flex gap-3 p-3 text-xs">
        <Smartphone className="text-muted-foreground mt-0.5 size-4 shrink-0" />
        <div className="flex flex-col gap-1">
          <p className="font-medium">Add to Home Screen</p>
          <p className="text-muted-foreground">
            On a phone, add Agent Flow to your home screen so it opens like an
            app. iPhone (Safari): Share → Add to Home Screen. Android (Chrome):
            menu ⋮ → Install app.
          </p>
        </div>
      </div>

      {repaired && <AccessRequests />}

      {repaired && (
        <div className="mt-6">
          <SectionHeader
            eyebrow="Paired devices"
            title="Revocable access"
            description="Every active browser or phone appears here."
          />
          {devices?.devices.length ? (
            <ul className="flex flex-col gap-1.5">
              {devices.devices.map((d) => (
                <li
                  key={d.id}
                  className="af-subtle-panel flex flex-wrap items-center gap-2 px-3 py-2 text-sm"
                >
                  <span className="min-w-0 flex-1 truncate">{d.name}</span>
                  <Badge variant={d.status === "active" ? "live" : "outline"}>
                    {d.status}
                  </Badge>
                  <span className="text-muted-foreground hidden text-xs sm:inline">
                    last seen {formatTs(d.lastSeen)}
                  </span>
                  <Button
                    variant="danger"
                    size="sm"
                    onClick={() => void handleRevoke(d.id)}
                  >
                    <Trash2 className="size-3.5" /> Revoke
                  </Button>
                </li>
              ))}
            </ul>
          ) : (
            <EmptyState title="No devices yet." />
          )}
        </div>
      )}
    </PageShell>
  );
}
