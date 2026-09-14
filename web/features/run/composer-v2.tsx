"use client";

// ComposerV2 — the terminal-run composer as its own component,
// with slash-command autocomplete (OpenCode Commands), @-mention file
// picker (best-effort, FS-based), multi-line editing
// with proper caret handling, and send-vs-newline that respects mobile
// keyboards.
//
// Preserves the existing terminal-first guarantees:
//   - safe-area insets
//   - visualViewport sizing (caller passes vpHeight)
//   - debounced refit: every keystroke must NOT resize the terminal
//     (each SIGWINCH repaints the TUI)
//
// The composer injects text into the live PTY (via TerminalPane.sendInput);
// it never calls sendRunMessage — the TUI renders the reply itself.
import {
  forwardRef,
  useCallback,
  useEffect,
  useImperativeHandle,
  useMemo,
  useRef,
  useState,
} from "react";
import {
  ArrowUp,
  Check,
  ChevronDown,
  ImagePlus,
  Loader2,
  X,
} from "lucide-react";
import { cn } from "@/lib/utils";
import { TTY_KEYS, type TtyKey, type TtyKeyAction } from "@/lib/tty";
import { normalizeImage, uploadImage, type UploadHandle } from "@/lib/uploads";
import { Picker, type PickerItem } from "@/components/ui/picker";
import type { EngineCommand } from "@/lib/agentd";

// ComposerHandle is how the parent focuses the textarea from inside a
// touch handler. It has to be imperative: iOS opens the on-screen keyboard
// only for a focus() call made synchronously in a user gesture, so a state
// change that mounts the composer and focuses in an effect pops nothing.
export interface ComposerHandle {
  focus: () => void;
}

export interface ComposerV2Props {
  // sendInput is the PTY injector the parent already owns (TerminalPane
  // ref.sendInput). We pass a function so the composer has no direct
  // coupling to the terminal pane.
  sendInput: (seq: string, opts?: { focusTerminal?: boolean }) => void;
  // submitInput sends one composed line as a submit (text, pause, then a
  // separate CR on the server side). The composer never sends text+"\r" as
  // one write: a TUI reads that as a paste and the CR lands in its prompt
  // box instead of submitting.
  submitInput: (text: string) => void;
  runId: string;
  // commands are the engine's slash-command catalog (OpenCode: /command,
  // Claude: empty). When commands is empty the / autocomplete is skipped.
  commands: EngineCommand[];
  // commands-loaded flips when commands have arrived; used to gate the
  // / autocomplete popover rendering.
  commandsLoading?: boolean;
  // onSendWithAttachments delivers a turn that carries attachment ids. The
  // composer only ever holds ids: the bytes were POSTed to the daemon over
  // the same authenticated channel as everything else.
  onSendWithAttachments?: (
    text: string,
    attachmentIds: string[],
  ) => Promise<void>;
  onHide?: () => void;
  // visible toggles the composer without unmounting it. On touch devices it
  // stays mounted so its textarea exists to be focused the instant the user
  // taps the TUI's prompt; hidden keeps it out of the layout, so a closed
  // composer never shrinks the terminal.
  visible?: boolean;
  // isTouch keeps focus in the composer after a key-row tap. On a fine
  // pointer the terminal is a real typing surface and the key row goes on
  // handing focus back to it.
  isTouch?: boolean;
}

// HISTORY_CAP bounds one run's remembered drafts.
const HISTORY_CAP = 50;

function historyKey(runId: string): string {
  return `af:composer-history:${runId}`;
}

function loadHistory(runId: string): string[] {
  try {
    const raw = localStorage.getItem(historyKey(runId));
    if (!raw) return [];
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    return parsed
      .filter((v): v is string => typeof v === "string")
      .slice(-HISTORY_CAP);
  } catch {
    // private mode / corrupt entry — an in-memory history still works
    return [];
  }
}

function saveHistory(runId: string, entries: string[]): void {
  try {
    localStorage.setItem(historyKey(runId), JSON.stringify(entries));
  } catch {
    // nothing to persist with; the session keeps its own copy
  }
}

// caretOnFirstLine / caretOnLastLine decide whether an arrow key belongs to
// history or to the draft the user is editing. A multi-line draft must still
// be navigable with the arrows.
function caretOnFirstLine(el: HTMLTextAreaElement): boolean {
  return !el.value.slice(0, el.selectionStart ?? 0).includes("\n");
}

function caretOnLastLine(el: HTMLTextAreaElement): boolean {
  return !el.value.slice(el.selectionEnd ?? 0).includes("\n");
}

export const ComposerV2 = forwardRef<ComposerHandle, ComposerV2Props>(
  function ComposerV2(
    {
      sendInput,
      submitInput,
      runId,
      commands,
      commandsLoading,
      onSendWithAttachments,
      onHide,
      visible = true,
      isTouch = false,
    },
    ref,
  ) {
    const [draft, setDraft] = useState("");
    const [slashIndex, setSlashIndex] = useState(0);
    const [history, setHistory] = useState<string[]>(() => loadHistory(runId));
    // histIdx is the position being recalled; null means "editing my own
    // draft", which is what Down returns to at the newest end.
    const [histIdx, setHistIdx] = useState<number | null>(null);
    const stashRef = useRef("");

    const taRef = useRef<HTMLTextAreaElement>(null);
    const rootRef = useRef<HTMLDivElement>(null);
    const fileRef = useRef<HTMLInputElement>(null);

    // --- attachments ----------------------------------------------------
    const [chips, setChips] = useState<Chip[]>([]);
    const [notice, setNotice] = useState("");
    const [sending, setSending] = useState(false);
    const [dragging, setDragging] = useState(false);
    const handles = useRef(new Map<string, UploadHandle>());
    const noticeTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

    // The notice is a one-liner that fades. It replaces the dead-button
    // pattern: the Attach control stays visible, and tapping it when the
    // link is down explains itself rather than doing nothing.
    const say = useCallback((msg: string) => {
      setNotice(msg);
      if (noticeTimer.current) clearTimeout(noticeTimer.current);
      noticeTimer.current = setTimeout(() => setNotice(""), 5000);
    }, []);

    useEffect(() => {
      const live = handles.current;
      const timer = noticeTimer;
      return () => {
        if (timer.current) clearTimeout(timer.current);
        for (const h of live.values()) h.cancel();
      };
    }, []);

    // Attaching is always available: the image is POSTed to the daemon over
    // the same authenticated connection the UI loaded over, so there is no
    // link to be "up" first.
    const startUpload = useCallback(
      async (files: File[]) => {
        const room = MAX_ATTACHMENTS - chips.length;
        if (room <= 0) {
          say(`You can attach up to ${MAX_ATTACHMENTS} images`);
          return;
        }
        for (const file of files.slice(0, room)) {
          const key = `chip-${Math.random().toString(36).slice(2, 10)}`;
          setChips((prev) => [
            ...prev,
            { key, name: file.name, state: "preparing", progress: 0, file },
          ]);
          try {
            const image = await normalizeImage(file);
            setChips((prev) =>
              prev.map((c) =>
                c.key === key
                  ? {
                      ...c,
                      name: image.name,
                      preview: image.previewUrl,
                      state: "uploading" as const,
                    }
                  : c,
              ),
            );
            const handle = uploadImage(runId, image, (sent, total) => {
              setChips((prev) =>
                prev.map((c) =>
                  c.key === key ? { ...c, progress: sent / total } : c,
                ),
              );
            });
            handles.current.set(key, handle);
            const id = await handle.done;
            handles.current.delete(key);
            setChips((prev) =>
              prev.map((c) =>
                c.key === key
                  ? { ...c, state: "done" as const, progress: 1, id }
                  : c,
              ),
            );
          } catch (err) {
            handles.current.delete(key);
            const msg =
              err instanceof Error ? err.message : "The upload failed";
            setChips((prev) =>
              prev.map((c) =>
                c.key === key
                  ? { ...c, state: "failed" as const, error: msg }
                  : c,
              ),
            );
            say(msg);
          }
        }
      },
      [chips.length, runId, say],
    );

    const removeChip = useCallback((key: string) => {
      handles.current.get(key)?.cancel();
      handles.current.delete(key);
      setChips((prev) => {
        const gone = prev.find((c) => c.key === key);
        if (gone?.preview) URL.revokeObjectURL(gone.preview);
        return prev.filter((c) => c.key !== key);
      });
    }, []);

    const retryChip = useCallback(
      (key: string) => {
        const chip = chips.find((c) => c.key === key);
        if (!chip?.file) return;
        removeChip(key);
        void startUpload([chip.file]);
      },
      [chips, removeChip, startUpload],
    );

    const imagesFromFiles = (list: FileList | null): File[] =>
      list ? Array.from(list).filter((f) => f.type.startsWith("image/")) : [];

    useImperativeHandle(ref, () => ({
      focus: () => {
        // Unhide before focusing: a display:none element cannot take focus,
        // and React's own re-render lands a tick too late for the gesture.
        const root = rootRef.current;
        if (root?.hidden) root.hidden = false;
        taRef.current?.focus();
      },
    }));

    // recall walks the history. Up from an untouched draft stashes it first,
    // so Down past the newest entry gives the user back what they were
    // typing.
    const recall = useCallback(
      (dir: TtyKeyAction) => {
        const el = taRef.current;
        if (history.length === 0) return;
        if (dir === "history-prev") {
          const next =
            histIdx === null ? history.length - 1 : Math.max(0, histIdx - 1);
          if (histIdx === null) stashRef.current = draft;
          setHistIdx(next);
          setDraft(history[next]);
        } else {
          if (histIdx === null) return;
          if (histIdx >= history.length - 1) {
            setHistIdx(null);
            setDraft(stashRef.current);
          } else {
            setHistIdx(histIdx + 1);
            setDraft(history[histIdx + 1]);
          }
        }
        // caret to the end of the recalled line, after React paints it
        requestAnimationFrame(() => {
          if (!el) return;
          el.selectionStart = el.selectionEnd = el.value.length;
        });
      },
      [draft, histIdx, history],
    );

    // auto-grow up to ~7 lines
    useEffect(() => {
      const el = taRef.current;
      if (!el) return;
      el.style.height = "auto";
      el.style.height = `${Math.min(el.scrollHeight, 168)}px`;
    }, [draft]);

    // slash autocomplete: when the cursor is at the start of a token that
    // starts with "/", open the picker and fuzzy-filter on the commands list.
    const slashState = useMemo(() => {
      if (!draft.startsWith("/")) {
        return { open: false, query: "", items: [] as PickerItem[] };
      }
      // everything after the leading "/", up to a space
      const sp = draft.indexOf(" ");
      const query = sp < 0 ? draft.slice(1) : draft.slice(1, sp);
      const items: PickerItem[] = commands.map((c) => ({
        id: c.name,
        label: "/" + c.name,
        description: c.description,
      }));
      const open = items.length > 0 && query.length > 0;
      return { open, query, items };
    }, [draft, commands]);

    // Slash index is bounded to the visible items — the picker clamps
    // bounds; clamp on read rather than resetting in an effect (which the
    // react-hooks/set-state-in-effect rule forbids).
    const clampedIndex = Math.min(
      slashIndex,
      Math.max(0, slashState.items.length - 1),
    );

    const send = () => {
      const text = draft.trim();
      const ready = chips.filter((c) => c.state === "done" && c.id);
      // A turn with images goes through the control endpoint (ids only);
      // a text-only turn keeps using the terminal's own paced submit, which
      // is the path the whole Enter fix was built on.
      if (ready.length > 0) {
        if (
          chips.some((c) => c.state === "uploading" || c.state === "preparing")
        ) {
          say("Wait for the images to finish uploading");
          return;
        }
        if (!onSendWithAttachments) return;
        setSending(true);
        void onSendWithAttachments(
          text,
          ready.map((c) => c.id!),
        )
          .then(() => {
            for (const c of chips)
              if (c.preview) URL.revokeObjectURL(c.preview);
            setChips([]);
            setDraft("");
            setHistIdx(null);
            stashRef.current = "";
            if (text) {
              setHistory((prev) => {
                if (prev[prev.length - 1] === text) return prev;
                const next = [...prev, text].slice(-HISTORY_CAP);
                saveHistory(runId, next);
                return next;
              });
            }
          })
          .catch((err: unknown) => {
            // Keep BOTH the draft and the chips: the engineer's work is not
            // ours to throw away because a request failed.
            say(err instanceof Error ? err.message : "Could not send");
          })
          .finally(() => setSending(false));
        return;
      }
      if (!text) return;
      submitInput(text);
      setDraft("");
      setHistIdx(null);
      stashRef.current = "";
      setHistory((prev) => {
        if (prev[prev.length - 1] === text) return prev;
        const next = [...prev, text].slice(-HISTORY_CAP);
        saveHistory(runId, next);
        return next;
      });
      // The composer keeps its own focus. Handing it to the terminal after a
      // send is what made the phone's Enter unreliable: with the composer
      // open, xterm's hidden textarea is keyboard-enabled, so focusing it
      // silently pointed the OS keyboard at the raw PTY and the next Enter
      // was a bare CR to the TUI instead of a submit through here.
      taRef.current?.focus();
    };

    // A turn is sendable with text OR with a finished image: "look at this"
    // is a complete message when the image IS the message.
    const canSend =
      draft.trim().length > 0 || chips.some((c) => c.state === "done" && c.id);

    const onKeyPress = (k: TtyKey) => {
      if (k.action) {
        recall(k.action);
        taRef.current?.focus();
        return;
      }
      if (!k.seq) return;
      sendInput(k.seq, { focusTerminal: !isTouch });
      if (isTouch) taRef.current?.focus();
    };

    return (
      <div className="relative z-20" ref={rootRef} hidden={!visible}>
        <div className="from-background pointer-events-none absolute -top-16 right-0 left-0 h-16 bg-gradient-to-t via-background/85 to-transparent" />
        <div
          className="px-3 pt-1 sm:px-4"
          style={{
            paddingBottom: "max(env(safe-area-inset-bottom), 0.625rem)",
          }}
        >
          <div className="mx-auto w-full max-w-3xl">
            <form
              onSubmit={(e) => {
                e.preventDefault();
                send();
              }}
              onDragOver={(e) => {
                if (!onSendWithAttachments) return;
                e.preventDefault();
                setDragging(true);
              }}
              onDragLeave={() => setDragging(false)}
              onDrop={(e) => {
                if (!onSendWithAttachments) return;
                e.preventDefault();
                setDragging(false);
                void startUpload(
                  imagesFromFiles(e.dataTransfer?.files ?? null),
                );
              }}
              className={cn(
                "focus-within:border-ring focus-within:ring-ring/25 rounded-[26px] border bg-card p-1.5 pl-4 shadow-[0_10px_38px_rgba(0,0,0,0.09)] ring-4 ring-transparent transition-all",
                // A subtle dashed edge is the whole drag affordance: enough
                // to say "drop here", not enough to move anything.
                dragging && "border-dashed border-ring",
              )}
            >
              <input
                ref={fileRef}
                type="file"
                accept="image/*"
                multiple
                hidden
                aria-hidden
                tabIndex={-1}
                onChange={(e) => {
                  void startUpload(imagesFromFiles(e.target.files));
                  e.target.value = ""; // let the same file be picked again
                }}
              />
              {/* Attachment strip. It exists ONLY when there is something in
                  it: an idle composer on a phone must not spend a row on a
                  feature nobody is using. One horizontally scrolling line,
                  never wrapping, animating in and out. */}
              <div
                className={cn(
                  "overflow-hidden transition-all duration-150 ease-out",
                  chips.length > 0
                    ? "mb-1.5 h-14 opacity-100"
                    : "h-0 opacity-0",
                )}
                aria-hidden={chips.length === 0}
              >
                <ul className="scrollbar-none flex h-14 items-center gap-2 overflow-x-auto">
                  {chips.map((chip) => (
                    <AttachmentChip
                      key={chip.key}
                      chip={chip}
                      onRemove={() => removeChip(chip.key)}
                      onRetry={() => retryChip(chip.key)}
                    />
                  ))}
                </ul>
              </div>

              {/* mobile key row: Esc/arrows/Ctrl+C/digits the TUI expects */}
              <div className="scrollbar-none mb-1.5 flex gap-1 overflow-x-auto pb-0.5">
                {TTY_KEYS.map((k) => (
                  <button
                    key={k.label}
                    type="button"
                    // onMouseDown + preventDefault prevents the click from
                    // stealing focus from the terminal pane (the TUI stops
                    // processing keystrokes once focus leaves the terminal's
                    // hidden textarea). sendInput also re-focuses the
                    // terminal after writing bytes, but doing it here too
                    // means the user's NEXT tap goes straight to the TUI
                    // rather than to whichever button they pressed last.
                    onMouseDown={(e) => e.preventDefault()}
                    onClick={() => onKeyPress(k)}
                    className="border-border/60 bg-muted/50 text-muted-foreground hover:bg-accent hover:text-foreground shrink-0 rounded-full border px-2.5 py-1 text-[11px] font-medium transition-colors active:scale-95"
                  >
                    {k.label}
                  </button>
                ))}
              </div>
              <textarea
                ref={taRef}
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                onPaste={(e) => {
                  if (!onSendWithAttachments) return;
                  const files: File[] = [];
                  for (const item of Array.from(e.clipboardData?.items ?? [])) {
                    const f = item.getAsFile();
                    if (f && f.type.startsWith("image/")) files.push(f);
                  }
                  if (files.length === 0) return; // an ordinary text paste
                  e.preventDefault();
                  void startUpload(files);
                }}
                onKeyDown={(e) => {
                  // An IME's commit key arrives as Enter: on Android the
                  // suggestion bar sends keyCode 229, and every IME sets
                  // isComposing while a candidate is open. Submitting there
                  // eats the word the user was still choosing.
                  const composing =
                    e.nativeEvent.isComposing === true ||
                    e.nativeEvent.keyCode === 229;
                  if (e.key === "Enter" && composing) return;
                  if (e.key === "Enter" && !e.shiftKey && !slashState.open) {
                    e.preventDefault();
                    send();
                    return;
                  }
                  if (
                    !slashState.open &&
                    (e.key === "ArrowUp" || e.key === "ArrowDown")
                  ) {
                    const el = e.currentTarget;
                    const atEdge =
                      draft === "" ||
                      (e.key === "ArrowUp"
                        ? caretOnFirstLine(el)
                        : caretOnLastLine(el));
                    if (atEdge) {
                      e.preventDefault();
                      recall(
                        e.key === "ArrowUp" ? "history-prev" : "history-next",
                      );
                      return;
                    }
                  }
                  if (slashState.open && slashState.items.length > 0) {
                    if (e.key === "ArrowDown") {
                      e.preventDefault();
                      setSlashIndex((i) =>
                        Math.min(i + 1, slashState.items.length - 1),
                      );
                    } else if (e.key === "ArrowUp") {
                      e.preventDefault();
                      setSlashIndex((i) => Math.max(0, i - 1));
                    } else if (e.key === "Tab" || e.key === "Enter") {
                      if (slashState.items[clampedIndex]) {
                        e.preventDefault();
                        const cmd = slashState.items[clampedIndex];
                        // replace the partial slash-command token with the
                        // full name and a trailing space — the user can keep
                        // typing the args.
                        const sp = draft.indexOf(" ");
                        const tail = sp < 0 ? "" : draft.slice(sp);
                        setDraft("/" + cmd.id + " " + tail.trimStart());
                      }
                    }
                  }
                }}
                rows={1}
                enterKeyHint="send"
                placeholder="Send to terminal…"
                // Phone keyboards float an autofill bar — the key/card/pin
                // trio — over any field they cannot rule out as a
                // credential, and it covers what is being typed. A plain
                // prose box has to say so explicitly: without a hint the OS
                // assumes it might be a password, a card or an address.
                // These are the same attributes the Picker's search input
                // carries, plus the password-manager opt-outs.
                //
                // This does not remove a third-party keyboard's overlay —
                // no web API can — it stops the keyboard and the managers
                // from OFFERING one over this field.
                autoComplete="off"
                autoCorrect="off"
                autoCapitalize="sentences"
                inputMode="text"
                spellCheck
                data-1p-ignore
                data-lpignore="true"
                data-form-type="other"
                className="placeholder:text-muted-foreground/60 max-h-42 min-h-7 w-full resize-none bg-transparent py-1.5 text-base leading-6 outline-none"
              />
              <div className="flex items-center justify-between gap-3 pt-1 pb-0.5 pl-0.5 pr-0.5">
                <div className="flex items-center gap-1">
                  <button
                    type="button"
                    onClick={onHide}
                    aria-label="Hide keyboard"
                    title="Hide keyboard — full screen terminal"
                    className="text-muted-foreground hover:bg-accent/60 hover:text-foreground inline-flex h-8 shrink-0 items-center gap-1 rounded-full px-2 text-[11px] transition-colors"
                  >
                    <ChevronDown className="size-4" /> Hide
                  </button>
                  {onSendWithAttachments && (
                    <button
                      type="button"
                      onClick={() => fileRef.current?.click()}
                      className="hover:bg-accent/60 hover:text-foreground text-muted-foreground relative grid min-h-11 min-w-11 shrink-0 place-items-center rounded-full transition-colors"
                      aria-label="Attach image"
                      title="Attach image"
                    >
                      <ImagePlus className="size-[18px]" />
                    </button>
                  )}
                  {commandsLoading && (
                    <Loader2 className="text-muted-foreground size-3.5 animate-spin" />
                  )}
                </div>
                <button
                  type="submit"
                  disabled={!canSend || sending}
                  aria-label={sending ? "Sending" : "Send to terminal"}
                  className={cn(
                    "grid size-9 shrink-0 place-items-center rounded-full transition-all active:scale-95",
                    canSend && !sending
                      ? "bg-primary text-primary-foreground shadow hover:brightness-110"
                      : "bg-muted text-muted-foreground cursor-not-allowed",
                  )}
                >
                  {sending ? (
                    <Loader2 className="size-5 animate-spin" />
                  ) : (
                    <ArrowUp className="size-5" />
                  )}
                </button>
              </div>
            </form>

            {/* One line, under the footer, that fades. It is where a muted
                Attach explains itself and where a failed send says why. */}
            {notice && (
              <p
                role="status"
                aria-live="polite"
                className="text-muted-foreground animate-in fade-in px-4 pt-1 text-[11px] duration-150"
              >
                {notice}
              </p>
            )}

            {/* slash-command picker */}
            {slashState.open && (
              <div className="mt-2">
                <Picker
                  title="Slash command"
                  items={slashState.items}
                  onPick={(it) => {
                    const sp = draft.indexOf(" ");
                    const tail = sp < 0 ? "" : draft.slice(sp);
                    setDraft("/" + it.id + " " + tail.trimStart());
                    taRef.current?.focus();
                  }}
                  onClose={() => setDraft("")}
                  open
                />
              </div>
            )}
          </div>
        </div>
      </div>
    );
  },
);

// MAX_ATTACHMENTS is what one turn can carry.
export const MAX_ATTACHMENTS = 4;

// Chip is one attachment as the composer sees it. It exists from the moment
// a file is picked, so the strip shows progress rather than appearing only
// once an upload finishes.
export interface Chip {
  key: string;
  name: string;
  state: "preparing" | "uploading" | "done" | "failed";
  progress: number;
  // id is the SERVER's attachment id, and the only thing that is ever sent
  // back with the message.
  id?: string;
  preview?: string;
  error?: string;
  // file is kept so a failed upload can be retried without re-picking.
  file?: File;
}

// AttachmentChip is one 44px thumbnail in the strip: a progress ring while
// it uploads, a check when it lands, a red badge to tap when it fails, and
// a remove control that also cancels an upload in flight.
function AttachmentChip({
  chip,
  onRemove,
  onRetry,
}: {
  chip: Chip;
  onRemove: () => void;
  onRetry: () => void;
}) {
  const pct = Math.round(chip.progress * 100);
  const busy = chip.state === "preparing" || chip.state === "uploading";
  return (
    <li className="relative shrink-0">
      <button
        type="button"
        onClick={chip.state === "failed" ? onRetry : undefined}
        disabled={chip.state !== "failed"}
        aria-label={
          chip.state === "failed"
            ? `${chip.name} failed to upload. ${chip.error ?? ""} Tap to retry.`
            : `${chip.name}${busy ? `, uploading ${pct}%` : ", ready"}`
        }
        className={cn(
          "border-border/60 relative grid size-11 place-items-center overflow-hidden rounded-lg border bg-muted/40",
          chip.state === "failed" && "border-destructive",
        )}
      >
        {chip.preview ? (
          // eslint-disable-next-line @next/next/no-img-element -- a local object URL, not a remote asset
          <img src={chip.preview} alt="" className="size-full object-cover" />
        ) : (
          <ImagePlus className="text-muted-foreground size-4" aria-hidden />
        )}
        {busy && (
          <span className="absolute inset-0 grid place-items-center bg-background/60">
            <Loader2
              className="text-foreground size-4 animate-spin"
              aria-hidden
            />
          </span>
        )}
        {chip.state === "done" && (
          <span className="bg-success absolute right-0 bottom-0 grid size-4 place-items-center rounded-tl-md">
            <Check className="text-background size-3" aria-hidden />
          </span>
        )}
        {chip.state === "failed" && (
          <span className="bg-destructive text-background absolute right-0 bottom-0 grid size-4 place-items-center rounded-tl-md text-[9px] font-bold">
            !
          </span>
        )}
      </button>
      {/* 44px target over a 16px visual: a remove control you cannot hit is
          not a remove control. */}
      <button
        type="button"
        onClick={onRemove}
        aria-label={`Remove ${chip.name}`}
        className="absolute -top-3 -right-3 grid size-11 place-items-center"
      >
        <span className="bg-foreground/80 text-background grid size-4 place-items-center rounded-full">
          <X className="size-3" aria-hidden />
        </span>
      </button>
    </li>
  );
}
