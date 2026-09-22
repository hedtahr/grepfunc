# grepfunc

A local [MCP](https://modelcontextprotocol.io) server that gives coding agents **grep that
returns whole function bodies** and an **edit tool that survives being given slightly wrong
text**. 22 tools, stdio, no daemon, no index, no cgo.

## Why I built this

Two things kept going wrong while working with an agent in Zed + DeepSeek:

**1. Editing.** The built-in edit tool failed on anything that was not a byte-exact match, and
when it did match it often wrote something subtly different from what was asked for. So the
first tool here is `patch_file`:

- matches `old_text` **fuzzily** — exact, whitespace-insensitive, line-fuzzy, then
  substring-fuzzy with a coverage threshold — and refuses ambiguity instead of guessing
  (`AMBIGUOUS_MATCH` lists every candidate line);
- matches **the bytes on disk**, never a formatted copy;
- applies every replacement from offsets taken against the original file, so a `replace_all`
  plus another edit cannot corrupt neighbouring text;
- reports the **nearest line** when it cannot match, so the next attempt is informed;
- previews the result: unified diff, echo block, confidence tier, and `go vet` / `python ast`
  validation after writing; `dry_run`, `fail_fast`, `terse`, and `batch_patch` over a glob.

**2. Searching.** A matching line is rarely enough to decide what to change: an LLM needs the
**whole enclosing function** — its signature, where it starts, where it ends, what it returns.
Line-oriented grep hides exactly that. `grep_func` returns brace-aware **bodies** for the
functions that match (`body=true` for full bodies), and the symbol tools answer the questions
that a plain grep turns into ten round trips: `symbol_at` (what function is this line in),
`file_symbols`, `find_symbol`, `grep_refs`, `grep_dead`, `grep_imports`, `grep_struct`.

The rest exists because agent sessions kept needing it: `memory` (project conventions that
survive sessions), `multi_read`, `file_stats`, `git`, `tool_stats`.

## Install

Requires **Go 1.25 or newer** (the only 1.25 API used is `sync.WaitGroup.Go`; `go vet`'s
`stdversion` analyser verifies that). No cgo, no runtime dependencies.

```sh
go install github.com/hedtahr/grepfunc@latest
```

or:

```sh
git clone https://github.com/hedtahr/grepfunc
cd grepfunc
go build -o grepfunc .
```

`go install` puts the binary in `$(go env GOPATH)/bin` (often `~/go/bin`) — use that full path
in the client config below.

### Zed

**Settings → AI → MCP Servers → Add Server → Add Local Server**, or edit the settings file
(`zed: open settings file`):

```json
{
  "context_servers": {
    "grepfunc": {
      "command": "/Users/you/go/bin/grepfunc",
      "args": [],
      "env": {}
    }
  }
}
```

A green dot next to the server means it is active. Zed forwards configured servers to external
agents over ACP as well.

**The profile trick that fixes the original problem.** Models reach for the built-in editor
unless it is unavailable. A profile that turns `edit_file` off and enables grepfunc's tools
makes them use `patch_file` instead (enable the rest of the 22 as you need them):

```json
{
  "agent": {
    "profiles": {
      "grepfunc": {
        "name": "grepfunc",
        "tools": { "edit_file": false, "write_file": false },
        "enable_all_context_servers": false,
        "context_servers": {
          "grepfunc": {
            "tools": {
              "patch_file": true,
              "grep_func": true,
              "grep_context": true,
              "multi_read": true,
              "file_symbols": true,
              "find_symbol": true,
              "symbol_at": true,
              "grep_refs": true
            }
          }
        }
      }
    }
  }
}
```

Tool approvals are controlled by `agent.tool_permissions.default` (`confirm` / `allow` / `deny`);
per-tool rules use the key format `mcp:<server>:<tool_name>`.

### Claude Code

```sh
claude mcp add grepfunc -- /Users/you/go/bin/grepfunc
```

### Any other MCP client

```json
{
  "mcpServers": {
    "grepfunc": {
      "command": "/Users/you/go/bin/grepfunc",
      "args": []
    }
  }
}
```

### Platforms

Linux, macOS and Windows. The runtime is standard library only — no `syscall`, no cgo, no
OS-specific paths — so the same code runs everywhere and cross-compiles to every
`linux`/`darwin`/`windows` × `amd64`/`arm64` target with a plain `GOOS=… go build`.

There is deliberately no CI. The platform claims here were verified directly: `go vet ./...` is
clean for the host OS **and** under `GOOS=windows` / `GOOS=linux`, and the test suite runs with
`go test ./...` (and `-race` where the race detector is available).

Windows specifics: symbol extraction and search behave identically (paths are slash-normalised
before glob and `.gitignore` matching, so ignore rules cannot silently stop applying), and
post-write validation is skipped when the validator (`python3`, `ruff`, `prettier`) is not on
`PATH` rather than failing the edit.

### Project root

The server resolves the project root in this order:

1. the MCP handshake — `rootPath`, then `rootUri`, then the first entry of `roots`;
2. the `PROJECT_ROOT` environment variable;
3. the process working directory (walking up from the executable's directory if the cwd is `/`);
4. the root cached from the previous session under `~/.config/grepfunc/last_root`.

`-project-root <dir>` presets the root before the handshake, and on startup the server also
auto-discovers a root by walking up from the binary looking for `.git` / `go.mod`. **Every** path
a tool touches is resolved and bounds-checked against the root that wins: symlinks escaping it
are refused, and secret-looking paths are blocked outright.

## Tools

| Tool | What it does |
|---|---|
| `patch_file` | Edit a file: replace `old_text`→`new_text` (fuzzy) or insert at a line. Matches on-disk bytes; `format=true` runs the formatter over the result. |
| `batch_patch` | The same edits across every file matching a glob, with per-file results and diffs. |
| `grep_func` | Function/method search with brace-aware bodies. `body=true` → full body; default signature + location. |
| `grep_struct` | Struct/class/interface/enum definitions. `body=true` → full body. |
| `grep_context` | Matching lines with N lines of context, deduplicated windows, scope annotation. |
| `grep_refs` | Every reference to a symbol: call sites, type usages, assignments. |
| `grep_dead` | Declared symbols with zero references outside their own file. |
| `grep_imports` | Which files import a module, or what a file imports (Go/Python/JS/Rust). |
| `grep_replace` | Regex find-and-replace across files, `$1` groups and `\n`/`\t` escapes, `dry_run` preview. |
| `file_symbols` | Func/type definitions with line numbers, no bodies — map an unfamiliar file. |
| `find_symbol` | Find a symbol by name: `file:line` + signature, with "did you mean" suggestions. |
| `symbol_at` | The function/type enclosing a line — name, kind, start/end lines. |
| `find_related` | Related files: tests, mocks, siblings, same-named files nearby. |
| `file_stats` | Project overview: file/line counts, extension breakdown per directory. |
| `multi_read` | Read files, globs, and line ranges in one call; reports total line counts. |
| `write_file` | Create or fully overwrite a file (prefer `patch_file` for surgical edits). |
| `move_symbol` / `rename_symbol` / `delete_symbol` | Move, rename project-wide, or delete a named symbol. |
| `git` | `mode=context` (branch, commits, status, `diff --stat`), `mode=diff` (staged/base/stat_only), `mode=restore`. |
| `memory` | Remember/recall project conventions across sessions. |
| `tool_stats` | Local usage telemetry: call counts, error rates, never-called tools. |

Every tool takes a `token_budget`; most take `compact`, `names_only`, or `terse` renderings, and
path-taking tools accept an `include` glob (`**/*.go`, `{go,sql}` sets supported).

## What is unusual about it

- **Brace-aware, line-based lexer, no tree-sitter and no cgo.** The scanner jumps between the
  bytes that matter, carries string/comment state across lines, and is validated against
  `go/parser` as an oracle: `TestBraceScannerMatchesGoParser` asserts **zero divergence** across
  the repository's Go files, and `TestScanLineAgreesWithDenseScan` holds the fast path to the
  byte-walk reference.
- **Exact-match tools stay exact.** Results are ordered `(file, line)`, truncated totals are
  marked `N+` with `(cap reached; totals may be incomplete)`, and edits are applied from original
  offsets — no "approximately the same" answers.
- **Sandboxed by construction.** Symlink-aware root resolution, banned-path rules, 2 MB per-file
  caps, `.gitignore`-aware walks.
- **Read cache with a stability window.** Reads are keyed by path + size + mtime taken from the
  stat the walker already did; files modified within the last 2 seconds are never cached, so a
  file an agent just edited is always read fresh.
- **Benchmarked.** `tools/*/bench_test.go` carry synthetic-tree benchmarks for the search path,
  the scanner, the diff, and the cache. They are how the current design was chosen (and several
  tempting optimisations were rejected — see below).
- **Telemetry is local and boring.** `tool_stats` reads an append-only log in the user cache dir;
  delete the file to opt out. Nothing is sent anywhere.

### Measured, on an Apple M4

| Benchmark | Result |
|---|---|
| truncated search over 120 files / 3600 funcs | 0.39 ms, 0.9 MB, 1.7k allocs |
| full-tree scan, no match | 3.9 ms (2.9 ms warm cache) |
| symbol extraction, 26k-line Go file | 4.1 ms vs `go/parser` 13.6 ms (3.1 MB vs 18.9 MB) |
| unified diff, 200 scattered edits in 2000 lines | 0.17 ms vs 9.6 ms before |
| byte scanning share of a full scan (CPU profile) | ~2% |

Run them yourself: `go test ./... -bench . -benchmem`.

### Rejected on the numbers

Keeping these out was as deliberate as the features:

- **SIMD / memchr / hand-written assembly.** CPU profiles put byte scanning at ~2% of a full
  scan, so a perfect vector kernel cannot buy more than that. The dependency-free version of the
  idea did pay: the scanner now jumps between interesting bytes with `bytes.IndexAny` /
  `bytes.IndexByte`, with a density switch back to the byte walk.
- **`go/parser` as the default symbol extractor.** It is exact, but 3.3x slower and 6x heavier
  than the brace scanner on a 26k-line file. It is kept as a **test oracle** instead: the scanner
  is asserted to agree with it on every Go file in the repo.
- **Batching files per work handoff.** Measured within noise on full walks, 2.7x worse on
  truncated queries.
- **More search workers.** 8 workers were slower than 4 on both benchmark shapes; 2 were better
  for truncated queries and 35% worse for full scans, so the cap stayed at 4.

## Caveats

- **It edits your files.** `patch_file` writes what you asked for and validates after writing;
  `git` `mode=restore` discards uncommitted changes to a file with no confirmation step yet.
- Symbol extraction is heuristic outside Go/Rust/JS/TS/Python-shaped code; when a `.go` file does
  not parse it falls back to the brace scanner rather than failing.
- By default, searches skip known non-source formats (docs, data, lock/log/map files). Pass an
  `include` glob to search them.
- Totals marked `N+` are floors: the walk stops when the page is full. Do not verify a bulk edit
  from a truncated count.

## Development

```sh
go test ./...                              # all 15 packages
go test ./tools/grepfunc -run Oracle -v    # scanner vs go/parser
go test ./... -bench . -benchmem           # the numbers above
golangci-lint run ./... && gosec ./...     # no config committed; defaults are used
```

## License

MIT — see [LICENSE](LICENSE).
