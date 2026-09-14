# Claude Code transcript fixtures

These JSONL files feed the parser tests in `internal/claudelog/parse`
(`fixtures_test.go`). Each file holds a handful of records of one kind:

- `type-<type>.jsonl`: one top-level record type (`assistant`, `pr-link`, ...).
- `special-<case>.jsonl`: an edge case the parser handles on purpose (system
  subtypes, queue operations, forwarded subagent content, peer messages,
  retractions, sidechains, a single line of about 2 MB).
- `version-<x.y.z>.jsonl`: records written by one Claude Code release.

`../manifest.json` lists each file with its category and record count.

## The content is synthetic

The records were captured from Claude Code transcripts and then run through
`../sanitize.py`. Nothing a person typed, and nothing a tool printed, survives:

- Prompts, answers, thinking, tool inputs (commands, file paths, patterns),
  tool results, titles, summaries, slugs and hook commands are filler text
  ("Lorem ipsum ..."). Word lengths, capitalization, punctuation, line breaks,
  and tag names such as `<task-notification>` are kept, so text has the same
  size and rough shape as before.
- Every uuid, `toolu_`/`msg_`/`req_`/`cse_`/`session_` id and long hex id is
  renumbered through one table shared by all files (for example
  `00000001-0000-4000-8000-000000000001`), so `parentUuid`, `tool_use_id`,
  `retractedMessageUuids` and similar references still point at the same
  records.
- `cwd` is always `/home/user/code/project-<letter>`. URLs keep their host only
  when it is `example.com`, `claude.ai` or `github.com`.
- Thinking signatures are random base64 of the same length.

What is kept verbatim, because the parser depends on it:

- JSON structure: key names, key order, nesting, array lengths, value types,
  empty strings, and the `json.dumps(..., ensure_ascii=False)` layout.
- Enumerated values: `type`, `role`, `subtype`, `operation`, `version`, model
  names, `stop_reason`, permission modes, `level`, `entrypoint`, `userType`,
  tool names on `tool_use` blocks, built-in tool/agent/skill identifiers in
  attachment lists, and similar.
- Timestamps, token counts and every other number, boolean and null.
- Line counts per file, and the byte size of `special-big-line.jsonl`.

`TestFixturesAreSynthetic` fails if a fixture carries a real working
directory, ids that were not renumbered, a home directory other than
`/home/user`, an email address, or a URL host outside the placeholder set.

## Adding or refreshing fixtures

1. Copy the raw records into new or existing `.jsonl` files here.
2. From `internal/claudelog/testdata`, run the sanitizer over the whole set in
   one go so ids stay consistent across files:

   ```sh
   python3 sanitize.py fixtures/*.jsonl
   ```

   The script rewrites the files in place and stops if a rewrite would change
   a record's structure. It is deterministic for a given input, but running it
   twice re-rolls the text, so only run it over freshly captured records.
3. Read through the result before committing, and run the parser tests.

If a new record kind carries an enumerated string field, add its key to
`KEEP_KEYS` in `sanitize.py`; anything not listed there is treated as free
text and replaced.

The `../golden` directory is separate: `mixed.jsonl` is hand-written and
`mixed.jsonl.golden` is regenerated with
`go test ./internal/claudelog/parse -run Golden -update`.
