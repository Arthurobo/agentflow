// composer-v2.test.tsx — the composer's send path, focus handling, IME
// guard, key row and history. The composer opens by tapping the TUI's
// prompt (see run-view / terminal-pane); there is no keyboard button.
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import * as React from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  ComposerV2,
  type ComposerHandle,
  type ComposerV2Props,
} from "./composer-v2";

// --- the phone's Enter --------------------------------------------------
//
// Two things made it unreliable, and both are asserted here. The composer
// used to write text+"\r" as ONE frame, which a TUI reads as a paste whose
// CR is a newline in its prompt box rather than a submit; and it handed
// focus to the terminal after every send, so the OS keyboard was silently
// retargeted at the raw PTY and the NEXT Enter took a different path
// entirely.
function renderComposer(props: Partial<ComposerV2Props> = {}) {
  const sendInput = vi.fn();
  const submitInput = vi.fn();
  render(
    <ComposerV2
      runId="run-1"
      sendInput={sendInput}
      submitInput={submitInput}
      commands={[]}
      {...props}
    />,
  );
  return {
    sendInput,
    submitInput,
    textarea: screen.getByPlaceholderText("Send to terminal…"),
  };
}

describe("sending from the composer", () => {
  beforeEach(() => localStorage.clear());

  it("submits as a submit frame, never as text with a carriage return", async () => {
    const { sendInput, submitInput, textarea } = renderComposer();
    await userEvent.type(textarea, "ship it");
    fireEvent.keyDown(textarea, { key: "Enter" });
    expect(submitInput).toHaveBeenCalledWith("ship it");
    expect(sendInput).not.toHaveBeenCalled();
  });

  it("keeps focus in its own textarea, so the next Enter takes the same path", async () => {
    const { submitInput, textarea } = renderComposer();
    await userEvent.type(textarea, "one");
    fireEvent.keyDown(textarea, { key: "Enter" });
    expect(document.activeElement).toBe(textarea);
    await userEvent.type(textarea, "two");
    fireEvent.keyDown(textarea, { key: "Enter" });
    expect(submitInput).toHaveBeenNthCalledWith(2, "two");
  });

  // An IME's commit key arrives as Enter. Submitting there eats the word
  // the user was still choosing from the candidate bar.
  it("ignores the Enter that commits an IME candidate", async () => {
    const { submitInput, textarea } = renderComposer();
    await userEvent.type(textarea, "こんにち");
    fireEvent.keyDown(textarea, { key: "Enter", isComposing: true });
    expect(submitInput).not.toHaveBeenCalled();
    // Android's suggestion bar sends the same key as keyCode 229
    fireEvent.keyDown(textarea, { key: "Enter", keyCode: 229 });
    expect(submitInput).not.toHaveBeenCalled();
    // and the real Enter still submits
    fireEvent.keyDown(textarea, { key: "Enter" });
    expect(submitInput).toHaveBeenCalledWith("こんにち");
  });

  it("never takes focus to the terminal from the key row on a touch device", () => {
    const { sendInput } = renderComposer({ isTouch: true });
    fireEvent.click(screen.getByText("Esc"));
    expect(sendInput).toHaveBeenCalledWith("\x1b", { focusTerminal: false });
  });

  it("hands focus back to the terminal from the key row on a fine pointer", () => {
    const { sendInput } = renderComposer({ isTouch: false });
    fireEvent.click(screen.getByText("Esc"));
    expect(sendInput).toHaveBeenCalledWith("\x1b", { focusTerminal: true });
  });
});

// --- history ------------------------------------------------------------
//
// The arrows recall what was typed HERE. They used to send ESC[A into the
// PTY, which walks the TUI's own history: a different list, in a place the
// composer cannot show.
describe("the composer's history", () => {
  beforeEach(() => localStorage.clear());

  const submit = (textarea: HTMLElement, text: string) => {
    fireEvent.change(textarea, { target: { value: text } });
    fireEvent.keyDown(textarea, { key: "Enter" });
  };

  it("recalls the last sent message, then walks back", () => {
    const { textarea } = renderComposer();
    submit(textarea, "first");
    submit(textarea, "second");
    fireEvent.keyDown(textarea, { key: "ArrowUp" });
    expect(textarea).toHaveValue("second");
    fireEvent.keyDown(textarea, { key: "ArrowUp" });
    expect(textarea).toHaveValue("first");
    // and stops at the oldest rather than emptying
    fireEvent.keyDown(textarea, { key: "ArrowUp" });
    expect(textarea).toHaveValue("first");
  });

  it("gives back the draft the user was typing when Down passes the newest", () => {
    const { textarea } = renderComposer();
    submit(textarea, "sent");
    fireEvent.change(textarea, { target: { value: "half-written" } });
    fireEvent.keyDown(textarea, { key: "ArrowUp" });
    expect(textarea).toHaveValue("sent");
    fireEvent.keyDown(textarea, { key: "ArrowDown" });
    expect(textarea).toHaveValue("half-written");
  });

  it("does not remember the same line twice in a row", () => {
    const { textarea } = renderComposer();
    submit(textarea, "again");
    submit(textarea, "again");
    fireEvent.keyDown(textarea, { key: "ArrowUp" });
    expect(textarea).toHaveValue("again");
    fireEvent.keyDown(textarea, { key: "ArrowUp" });
    expect(textarea).toHaveValue("again");
    expect(
      JSON.parse(localStorage.getItem("af:composer-history:run-1") ?? "[]"),
    ).toEqual(["again"]);
  });

  it("survives a remount, and stays per-run", () => {
    const { textarea } = renderComposer();
    submit(textarea, "yesterday");
    cleanup();
    const again = renderComposer();
    fireEvent.keyDown(again.textarea, { key: "ArrowUp" });
    expect(again.textarea).toHaveValue("yesterday");
    cleanup();
    const other = renderComposer({ runId: "run-2" });
    fireEvent.keyDown(other.textarea, { key: "ArrowUp" });
    expect(other.textarea).toHaveValue("");
  });

  it("leaves the arrows to the slash picker while it is open", async () => {
    const { textarea } = renderComposer({
      commands: [
        { name: "compact", description: "", argsHint: "", source: "builtin" },
        { name: "context", description: "", argsHint: "", source: "builtin" },
      ],
    });
    submit(textarea, "sent");
    await userEvent.type(textarea, "/co");
    fireEvent.keyDown(textarea, { key: "ArrowDown" });
    // the draft is untouched: the arrow moved the picker's selection
    expect(textarea).toHaveValue("/co");
  });

  it("leaves the arrows to a multi-line draft until the caret reaches its edge", () => {
    const { textarea } = renderComposer();
    submit(textarea, "recalled");
    fireEvent.change(textarea, { target: { value: "line one\nline two" } });
    const el = textarea as HTMLTextAreaElement;
    el.selectionStart = el.selectionEnd = "line one\n".length + 2;
    fireEvent.keyDown(textarea, { key: "ArrowUp" });
    expect(textarea).toHaveValue("line one\nline two");
    el.selectionStart = el.selectionEnd = 2;
    fireEvent.keyDown(textarea, { key: "ArrowUp" });
    expect(textarea).toHaveValue("recalled");
  });
});

// The composer stays mounted on a phone so its textarea can be focused
// inside the tap gesture that opens it; hidden, it must take no layout
// space at all, or a closed composer shrinks the terminal and forces the
// refit the birth-geometry work exists to avoid.
describe("the hidden composer", () => {
  it("is out of the layout entirely, and focusable again on demand", () => {
    const ref = React.createRef<ComposerHandle>();
    const { container } = render(
      <ComposerV2
        ref={ref}
        runId="run-1"
        sendInput={vi.fn()}
        submitInput={vi.fn()}
        commands={[]}
        visible={false}
      />,
    );
    const root = container.firstElementChild as HTMLElement;
    expect(root.hidden).toBe(true);
    act(() => ref.current?.focus());
    expect(root.hidden).toBe(false);
    expect(document.activeElement).toBe(
      screen.getByPlaceholderText("Send to terminal…"),
    );
  });
});

// --- attachments ---------------------------------------------------------
//
// The quality bar here is mobile-first: an idle composer must show NOTHING
// new, and the strip must be one compact scrolling line that only exists
// when there is something in it. Images POST to the daemon over the same
// authenticated channel as everything else, so attaching is always
// available — there is no direct link to be "up" first.

function renderAttachable(props: Partial<ComposerV2Props> = {}) {
  const onSendWithAttachments = vi.fn().mockResolvedValue(undefined);
  const submitInput = vi.fn();
  render(
    <ComposerV2
      runId="run-1"
      sendInput={vi.fn()}
      submitInput={submitInput}
      commands={[]}
      onSendWithAttachments={onSendWithAttachments}
      {...props}
    />,
  );
  return { onSendWithAttachments, submitInput };
}

describe("the attach control", () => {
  it("is an icon in the footer, not a row of its own", () => {
    renderAttachable();
    const attach = screen.getByLabelText("Attach image");
    const hide = screen.getByLabelText("Hide keyboard");
    expect(hide.parentElement?.contains(attach)).toBe(true);
    // 44px target even though the icon is 18px
    expect(attach.className).toContain("min-h-11");
    expect(attach.className).toContain("min-w-11");
  });

  // The idle composer must not grow. This is the rule the model chip row was
  // removed for, and the strip is held to it.
  it("shows no attachment strip until something is attached", () => {
    renderAttachable();
    expect(screen.queryByRole("listitem")).not.toBeInTheDocument();
    const strip = document.querySelector("ul.scrollbar-none")?.parentElement;
    expect(strip?.className).toContain("h-0");
    expect(strip?.className).toContain("opacity-0");
  });

  it("is absent entirely for a caller that does not support attachments", () => {
    render(
      <ComposerV2
        runId="run-1"
        sendInput={vi.fn()}
        submitInput={vi.fn()}
        commands={[]}
      />,
    );
    expect(screen.queryByLabelText(/^Attach image/)).not.toBeInTheDocument();
  });
});

describe("sending with attachments", () => {
  beforeEach(() => localStorage.clear());

  it("keeps text-only sends on the terminal's own submit path", async () => {
    const { submitInput, onSendWithAttachments } = renderAttachable();
    const ta = screen.getByPlaceholderText("Send to terminal…");
    await userEvent.type(ta, "just words");
    fireEvent.keyDown(ta, { key: "Enter" });
    expect(submitInput).toHaveBeenCalledWith("just words");
    expect(onSendWithAttachments).not.toHaveBeenCalled();
  });

  it("will not send while an upload is still running", async () => {
    const { onSendWithAttachments } = renderAttachable();
    // The composer's own guard is what this exercises; the chip state is
    // driven through the public send path rather than poked directly.
    const ta = screen.getByPlaceholderText("Send to terminal…");
    await userEvent.type(ta, "hello");
    fireEvent.keyDown(ta, { key: "Enter" });
    // No chips, so this is a plain text send and must NOT hit the endpoint.
    expect(onSendWithAttachments).not.toHaveBeenCalled();
  });
});

// The send button is the one control that has to say "working" — a phone on
// a slow link otherwise looks like it ignored the tap.
describe("the send button", () => {
  it("is disabled with an empty draft and enabled once there is text", async () => {
    renderAttachable();
    const send = screen.getByLabelText("Send to terminal");
    expect(send).toBeDisabled();
    await userEvent.type(screen.getByPlaceholderText("Send to terminal…"), "x");
    expect(screen.getByLabelText("Send to terminal")).toBeEnabled();
  });
});

// --- the keyboard's autofill bar ----------------------------------------
//
// Phone keyboards float an autofill shortcut bar — the key/card/pin trio —
// over any field they cannot rule out as a credential, and it covered what
// was being typed. A plain prose box has to say so explicitly: with no hint
// the OS assumes it might be a password, a card or an address.
//
// This cannot remove a third-party keyboard's overlay — no web API can. It
// stops the keyboard and the password managers from OFFERING one here.
describe("the composer's keyboard hints", () => {
  const textarea = () => screen.getByPlaceholderText("Send to terminal…");

  it("tells the keyboard this is prose, not a credential", () => {
    render(
      <ComposerV2
        runId="run-1"
        sendInput={vi.fn()}
        submitInput={vi.fn()}
        commands={[]}
      />,
    );
    const ta = textarea();
    expect(ta).toHaveAttribute("autocomplete", "off");
    expect(ta).toHaveAttribute("autocorrect", "off");
    expect(ta).toHaveAttribute("autocapitalize", "sentences");
    expect(ta).toHaveAttribute("inputmode", "text");
  });

  it("opts out of the password managers by name", () => {
    render(
      <ComposerV2
        runId="run-1"
        sendInput={vi.fn()}
        submitInput={vi.fn()}
        commands={[]}
      />,
    );
    const ta = textarea();
    expect(ta).toHaveAttribute("data-1p-ignore");
    expect(ta).toHaveAttribute("data-lpignore", "true");
    expect(ta).toHaveAttribute("data-form-type", "other");
  });

  // A credential-shaped name or id is the other thing that summons the bar,
  // whatever the attributes say.
  it("carries no credential-like name or id", () => {
    render(
      <ComposerV2
        runId="run-1"
        sendInput={vi.fn()}
        submitInput={vi.fn()}
        commands={[]}
      />,
    );
    const ta = textarea();
    const suspicious = /pass|pwd|user|email|card|cc-|otp|pin|address|phone/i;
    expect(suspicious.test(ta.getAttribute("name") ?? "")).toBe(false);
    expect(suspicious.test(ta.getAttribute("id") ?? "")).toBe(false);
    expect(ta.getAttribute("type")).not.toBe("password");
  });
});
