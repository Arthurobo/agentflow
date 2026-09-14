#!/usr/bin/env python3
"""Rewrite Claude Code transcript fixtures into synthetic, shape-preserving data.

The parser tests need the *structure* of real transcripts (record types, field
names, nesting, key order, uuid/parentUuid linkage, content block kinds, tool
names, versions, system subtypes, token counts, line sizes), not what anyone
actually typed. This script keeps the former and replaces the latter:

* Enumerated / structural strings (``type``, ``role``, ``subtype``, model names,
  ``version``, permission modes, ...) and timestamps are kept verbatim.
* Identifiers (uuids, ``toolu_``/``msg_``/``req_``/``session_`` ids, long hex
  ids) are renumbered through one corpus-wide table, so every reference that
  pointed at the same record still does, inside free text too.
* Every other string is free text. Words become filler words of the same length
  (case and digit positions preserved), punctuation and whitespace stay, and
  XML-ish tags such as ``<task-notification>`` keep their tag names. Replacement
  words come from a PRNG seeded by file name, line and JSON path, never from the
  original word, so the output cannot be mapped back by dictionary lookup.
* ``cwd`` values collapse to ``/home/user/code/project-<letter>``; URLs keep only
  an allow-listed host; thinking signatures become random base64.

Numbers, booleans and nulls are untouched. Output is re-serialized exactly the
way the fixtures are stored (``json.dumps(..., ensure_ascii=False)``), and the
script verifies each rewritten line has the same skeleton as its source.

Usage (from this directory):

    python3 sanitize.py fixtures/*.jsonl    # rewrite in place

The script is deterministic: the same input always produces the same output.
It is not idempotent on its own output (words are re-rolled and ids renumbered
again), which is fine because it only ever needs to run on freshly captured
transcripts. Pass every file of a capture in one run so the identifier table is
shared across files.
"""

import argparse
import base64
import json
import random
import re
import sys

# Keys whose string values are enumerations or structure, not prose.
KEEP_KEYS = {
    "type", "role", "subtype", "operation", "model", "version", "stop_reason",
    "stop_sequence", "userType", "entrypoint", "effort", "level",
    "permissionMode", "mode", "trigger", "direction", "scope",
    "toolDenialKind", "cronKind", "choice", "promptSource", "inference_geo",
    "service_tier", "speed", "fallbackModel", "originalModel",
    "apiRefusalCategory", "kind", "fromMode", "stopReason",
}

# Keys holding RFC 3339 timestamps; ordering matters to the parser.
TIMESTAMP_KEYS = {"timestamp", "backupTime"}

# Attachment lists of built-in tool / agent / skill identifiers. Entries that are
# bare identifiers are kept; anything longer (descriptions) is free text.
IDENT_LIST_KEYS = {"addedNames", "addedTypes", "names", "addedLines", "removedNames", "readdedNames"}
IDENT_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_-]*$")

GIT_BRANCH_KEEP = {"HEAD", "main", "master"}
URL_HOST_KEEP = {"example.com", "claude.ai", "github.com"}

UUID_RE = r"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}"
PREFIXED_ID_RE = r"(?:toolu|srvtoolu|msg|req|cse|session)_[A-Za-z0-9]{8,}"
HEX_ID_RE = r"(?<![A-Za-z0-9])[0-9a-f]{16,}(?![A-Za-z0-9])"
ID_RE = re.compile("(%s)|(%s)|(%s)" % (UUID_RE, PREFIXED_ID_RE, HEX_ID_RE))
WHOLE_ID_RE = re.compile("^(?:%s|%s|%s)$" % (UUID_RE, PREFIXED_ID_RE, HEX_ID_RE))

URL_RE = re.compile(r"https?://[^\s\"'<>)\]]+")
# Only lowercase kebab/snake tag names directly followed by ">", "/" or
# whitespace count as tags; "<Foo" inside code stays free text.
TAG_RE = re.compile(r"</?[a-z][a-z0-9_-]*(?=[\s>/])")
# Letters of any script (so accented names are replaced too) or ASCII digits.
WORD_RE = re.compile(r"[^\W\d_]+|[0-9]+")

LOREM = (
    "lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor "
    "incididunt ut labore et dolore magna aliqua enim ad minim veniam quis nostrud "
    "exercitation ullamco laboris nisi aliquip ex ea commodo consequat duis aute irure "
    "in reprehenderit voluptate velit esse cillum fugiat nulla pariatur excepteur sint "
    "occaecat cupidatat non proident sunt culpa qui officia deserunt mollit anim id est "
    "laborum"
).split()
LOREM_BY_LEN = {}
for _w in LOREM:
    LOREM_BY_LEN.setdefault(len(_w), []).append(_w)


class IDTable:
    """Corpus-wide renumbering of identifiers, keyed by the original value."""

    def __init__(self):
        self.map = {}
        self.counter = 0

    def remap(self, value):
        if value in self.map:
            return self.map[value]
        self.counter += 1
        n = self.counter
        if re.fullmatch(UUID_RE, value):
            new = "%08x-0000-4000-8000-%012x" % (n, n)
        elif "_" in value and re.fullmatch(PREFIXED_ID_RE, value):
            prefix, rest = value.split("_", 1)
            digits = str(n)
            new = prefix + "_" + "0" * max(0, len(rest) - len(digits)) + digits
        else:
            new = ("%x" % n).rjust(len(value), "0")
        self.map[value] = new
        return new


class Sanitizer:
    def __init__(self):
        self.ids = IDTable()
        self.cwds = {}

    # --- free text ---------------------------------------------------------

    @staticmethod
    def filler_word(rng, length):
        pool = LOREM_BY_LEN.get(length)
        if pool and length > 1:
            return rng.choice(pool)
        out, size = [], 0
        while size < length:
            w = rng.choice(LOREM)
            out.append(w)
            size += len(w)
        return "".join(out)[:length]

    def word(self, rng, original):
        if original.isdigit():
            return "".join(rng.choice("0123456789") for _ in original)
        base = self.filler_word(rng, len(original))
        if original.isupper() and len(original) > 1:
            return base.upper()
        if original[0].isupper():
            return base[0].upper() + base[1:]
        return base

    def words(self, rng, text):
        return WORD_RE.sub(lambda m: self.word(rng, m.group(0)), text)

    def url(self, rng, url):
        scheme, rest = url.split("://", 1)
        host, _, path = rest.partition("/")
        if host.lower() not in URL_HOST_KEEP:
            host = "example.com"
        return scheme + "://" + host + ("/" + self.plain(rng, path) if path else "")

    def plain(self, rng, text):
        """Scramble words while renumbering embedded identifiers."""
        out, pos = [], 0
        for m in ID_RE.finditer(text):
            out.append(self.words(rng, text[pos:m.start()]))
            out.append(self.ids.remap(m.group(0)))
            pos = m.end()
        out.append(self.words(rng, text[pos:]))
        return "".join(out)

    def free_text(self, rng, text):
        out, pos = [], 0
        tokens = sorted(
            [(m.start(), m.end(), "url") for m in URL_RE.finditer(text)]
            + [(m.start(), m.end(), "tag") for m in TAG_RE.finditer(text)],
        )
        for start, end, kind in tokens:
            if start < pos:
                continue  # a tag-looking run inside a URL, already handled
            out.append(self.plain(rng, text[pos:start]))
            chunk = text[start:end]
            out.append(self.url(rng, chunk) if kind == "url" else chunk)
            pos = end
        out.append(self.plain(rng, text[pos:]))
        return "".join(out)

    # --- per-field rules ---------------------------------------------------

    def string(self, rng, key, parent_key, parent, value):
        if value == "":
            return value
        if key in KEEP_KEYS or key in TIMESTAMP_KEYS:
            return value
        if key == "name" and isinstance(parent, dict) and parent.get("type") == "tool_use":
            return value  # tool names (Bash, Read, ...) are structure
        if parent_key in IDENT_LIST_KEYS and IDENT_RE.match(value):
            return value
        if key == "gitBranch" and value in GIT_BRANCH_KEEP:
            return value
        if WHOLE_ID_RE.match(value):
            return self.ids.remap(value)
        if key == "signature":
            raw = bytes(rng.getrandbits(8) for _ in range(len(value) * 3 // 4 + 3))
            return base64.b64encode(raw).decode()[: len(value)]
        if key == "cwd":
            if value not in self.cwds:
                self.cwds[value] = "/home/user/code/project-" + chr(ord("a") + len(self.cwds))
            return self.cwds[value]
        return self.free_text(rng, value)

    def walk(self, seed, node, key=None, parent_key=None, parent=None):
        if isinstance(node, dict):
            return {
                k: self.walk(seed + "." + k, v, k, key, node) for k, v in node.items()
            }
        if isinstance(node, list):
            return [
                self.walk(seed + "[%d]" % i, v, None, key, parent) for i, v in enumerate(node)
            ]
        if isinstance(node, str):
            rng = random.Random(seed)
            # list items take their list's key for the keep rules
            eff_key = key if key is not None else parent_key
            return self.string(rng, eff_key, parent_key, parent, node)
        return node


def skeleton(node):
    """Structure used to verify a rewrite: keys, list lengths, value types."""
    if isinstance(node, dict):
        return ("dict", tuple((k, skeleton(v)) for k, v in node.items()))
    if isinstance(node, list):
        return ("list", tuple(skeleton(v) for v in node))
    if isinstance(node, str):
        return ("str", node == "")
    return (type(node).__name__, node)


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("files", nargs="+")
    args = ap.parse_args()

    s = Sanitizer()
    for path in sorted(args.files):
        with open(path, encoding="utf-8") as f:
            data = f.read()
        name = path.rsplit("/", 1)[-1]
        out_lines = []
        for i, line in enumerate(data.split("\n")):
            if not line.strip():
                out_lines.append(line)
                continue
            rec = json.loads(line)
            new = s.walk("%s:%d" % (name, i), rec)
            if skeleton(new) != skeleton(rec):
                sys.exit("%s:%d: rewrite changed the record skeleton" % (path, i + 1))
            out_lines.append(json.dumps(new, ensure_ascii=False))
        out = "\n".join(out_lines)
        with open(path, "w", encoding="utf-8") as f:
            f.write(out)
        print("sanitized", path)


if __name__ == "__main__":
    main()
