# gocache-overlay

**Keep mutation testing from filling your disk with Go build cache.**

Mutation testing builds the same code thousands of times, each time with one
small change. Every mutant's compiled packages and test results go into the Go
build cache (`GOCACHE`), and nothing ever reads them again. The cache keeps
growing until `go` trims it, five days later.

`gocache-overlay` is a [`GOCACHEPROG`](https://pkg.go.dev/cmd/go/internal/cacheprog)
helper that solves this. It:

- **reuses the cache you already have.** Packages a mutant did not change
  (the standard library, your dependencies, and the packages the mutation
  does not reach) come from the existing `GOCACHE`, as normal cache hits.
- **adds nothing to that cache.** It never writes there. A mutant's new
  outputs go to a scratch directory that is deleted when the build ends, or
  when the run ends if you give it a run id.

```sh
go install github.com/richardtkemp/gocache-overlay@latest

# One mutant: reads $GOCACHE, writes to a scratch dir that is gone afterwards.
GOCACHEPROG=gocache-overlay go test ./...
```

It is a single static Go binary with no dependencies. It needs Go 1.24 or
later, where `GOCACHEPROG` is generally available. It is tested against Go
1.26.

## How it works

The go command starts the helper and sends it `get`, `put` and `close`
requests over stdin and stdout.

- **get**: it looks in the scratch dir first, which holds this build's or
  this run's own outputs, then in the shared native cache. A hit returns
  the path of the existing file, so nothing is copied.
- **put**: the body is written to the scratch dir only, in the same on-disk
  layout as a native cache. Every file is written to a temporary name and
  renamed into place.
- **close**, or the go command exiting: a scratch dir without a run id is
  removed.

The shared cache is opened as a read-only type that has no write methods.
The helper also leaves its files' mtimes alone. (The go command updates
mtimes on use, and its trim uses them to decide what to keep.)

## Usage

### One-off builds (no run id)

```sh
GOCACHEPROG=gocache-overlay go build ./...
```

Each go command gets its own `<root>/inv-<pid>-*` scratch dir, removed when
that command exits.

### A mutation-testing run (run id)

Give every go command in the run the same id. They then share one scratch
dir, so a mutant can reuse what an earlier mutant in the same run built. At
the end of the run, remove the dir by its id:

```sh
export GOCACHEPROG=gocache-overlay
export GOCACHE_OVERLAY_ID="$WORKFLOW_ID"   # or: GOCACHEPROG="gocache-overlay -id $WORKFLOW_ID"

for m in mutants/*; do (cd "$m" && go test ./...); done   # parallel is fine too

gocache-overlay -remove-id "$WORKFLOW_ID"                 # final cleanup step
```

Several go processes can write to one run dir at the same time. Every write
is atomic, and a reader sees either a complete entry or no entry.

### The backstop sweep

A run that is cancelled or crashes never reaches its cleanup step. So at every
start, the helper removes scratch dirs under its root that nobody has touched
for longer than `-max-age` (default `24h`). A live run dir is touched when a go
command starts in it, and again at most once a minute while it writes.

### Flags and environment

You can set each flag on the `GOCACHEPROG` command line
(`GOCACHEPROG="gocache-overlay -id x"`). The go command splits that line on
spaces, so paths in it must not contain spaces. You can also set each flag
with an environment variable, which the go command passes on to the helper.

| flag | env | default | meaning |
|---|---|---|---|
| `-shared` | `GOCACHE_OVERLAY_SHARED` | `$GOCACHE` | the native cache to read |
| `-root` | `GOCACHE_OVERLAY_ROOT` | `$TMPDIR/gocache-overlay-<uid>` | where scratch dirs go; must not contain the shared dir; may be inside it, beside the `00`–`ff` dirs |
| `-id` | `GOCACHE_OVERLAY_ID` | none | run id (`[A-Za-z0-9._-]`, up to 128 characters) |
| `-max-age` | `GOCACHE_OVERLAY_MAX_AGE` | `24h` | sweep scratch dirs older than this; `0` turns the sweep off |
| `-stats` | `GOCACHE_OVERLAY_STATS` | none | append one JSON line of counters per helper process |
| `-v` | `GOCACHE_OVERLAY_VERBOSE` | off | print that line to stderr |
| | `GOCACHE_OVERLAY_DEBUG` | off | log every get and put to stderr |
| `-remove-id ID` | | | delete `<root>/run-ID`, print the bytes it held, and exit |
| `-mode` | `GOCACHE_OVERLAY_MODE` | `overlay` | `plain`: read and write the shared cache like the go command itself (no scratch) |
| `-lock FILE` | `GOCACHE_OVERLAY_LOCK` | none | flock FILE on the first cache miss and hold it until exit (see below) |

Stats line: `{"gets":…,"scratch_hits":…,"shared_hits":…,"misses":…,"puts":…,"put_bytes":…}`.

### A lazy build lock (`-lock`)

A host that serialises heavy builds with one flock (foci's `/tmp/heavy`) makes
every go command wait in that queue, even one the cache fully covers. With
`-lock FILE` the helper takes the lock at the first cache **miss**, before it
answers that miss, and holds it until it exits (the go command's end). A go
command with no miss never touches the lock; one with a miss is serialised as
before.

- The file is opened read-only and never created (a sandbox may allow only
  reading it; a missing lock file is a setup error). Failing to open or lock it
  fails the build: it never runs unlocked.
- It does not wait on a lock its own process tree holds, which would deadlock:
  - `$HEAVY_LOCK_HELD` (colon-separated paths) names the file: a caller that
    took the lock up front says so to everything it starts;
  - the holder in `/proc/locks` is an ancestor of the helper;
  - an ancestor holds it through one of its open files (a `lock:` line in
    `/proc/<pid>/fdinfo/<fd>` for an fd on the file). `/proc/locks` names the
    process that called flock, not the one keeping the lock: in
    `( flock 9; go build 9<&- ) 9<lockfile` the flock(1) child exits and the
    lock stays with the subshell's fd 9;
  - the holder is another process of this program (same `/proc/<pid>/comm`)
    whose parent is an ancestor: a go command run by a test (a nested
    `go build`) while the test's own go command's helper holds the lock.
- It cannot tell a `go test -count=N` run (whose tests always run, so a fully
  cached compile means no miss and no lock) from a cached one. Such a caller
  must take the lock up front and set `$HEAVY_LOCK_HELD`.
- The stats line's `lock` field says what happened: `not needed`, `taken`,
  `taken after waiting <d>`, or `held by ...`.
- When it takes the lock it prints the lines a build script around `flock`
  prints, to stderr, which the go command passes through as its own:
  `>>> waiting for heavy lock ... [HH:MM:SS] (...)` before it blocks and
  `>>> acquired heavy lock [HH:MM:SS] (...)` once it holds the lock (local
  time; both at once when nobody else held it). It prints nothing when it does
  not take the lock (no miss, or one of the cases above). A caller that sends
  the go command's output to a file finds them there.

`-mode plain` is the same helper without the overlay: gets and puts go to the
shared cache, as with no helper at all, and a hit refreshes the entry's mtimes
the way the go command does (so mtime-based trimming still sees it in use).
Files are written with the go command's mode (0666 less umask).

### Rules it enforces

- **`GOCACHE` must be the shared dir, or a prefix of it.** The go command
  copies files that are under `GOCACHE` when it installs them. A file
  outside `GOCACHE` it may instead `chmod` and `rename`, which would move a
  shared file out of the cache. (See `moveOrCopyFile` in
  `cmd/go/internal/work`.) Normally you leave `GOCACHE` alone and the
  default `-shared` follows it. The go command itself writes nothing new
  there: the `00`–`ff` subdirs and the `README` already exist, and while
  `GOCACHEPROG` is in use it neither trims nor updates mtimes.
- **The scratch root must not contain the shared dir.** It may sit INSIDE
  the shared dir, beside the entry dirs (for example `$GOCACHE/overlay`), but
  not be the shared dir itself or lie under one of its `00`–`ff` entry dirs.
  The helper reads only those, and go's trim and `go clean -cache` touch only
  those, so a root beside them is invisible to all three. Inside `GOCACHE` is
  the better place for a long-lived run dir: the go command copies scratch
  files it installs instead of renaming them out of the scratch dir, and a
  sandbox that lets go write `GOCACHE` (foci's Landlock seal) already lets the
  helper write there. A cron that deletes old files under `GOCACHE` by age
  also reaches the scratch files; the run dirs themselves go by `-remove-id`
  and the sweep.

## A Go upgrade that changes the cache format

The helper reads the native cache's file layout directly. That layout is an
internal detail of `cmd/go`, and no document promises it will stay the same.
It has been stable for many releases, through at least Go 1.26:

```
<GOCACHE>/<xx>/<actionID>-a   "v1 <actionID> <outputID> <size %20d> <unixnano %20d>\n"
<GOCACHE>/<xx>/<outputID>-d   the output (or a dir holding one cached executable)
```

If the format changes, the helper fails loudly; it does not quietly miss:

- **At startup** it reads up to 16 existing entries. If any of them is not in
  the format above, it exits with `native Go build cache format not
  recognised`, and the go command fails.
- **On every get**, an entry in an unknown format is a fatal error, not a
  miss. An empty or partly written `v1` entry is a miss, because a native
  writer can be part-way through writing it.
- **The test suite** seeds a cache with the Go toolchain you have
  installed. It then checks that every entry parses and that an unchanged
  build gets 100% shared hits. After a Go upgrade, run `make test` before
  you rely on the helper.

## Measured

`go test -c ./internal/agent` on a real Go project (about 3,400 cache
lookups), with Go 1.26.9 and a 26 GB shared `/var/cache/go-build`:

| run | shared hits | misses | written to scratch | added to shared cache | wall time |
|---|---|---|---|---|---|
| one-line mutant, native cache | n/a | n/a | n/a | ~19 MB (the same outputs the overlay puts; not measured separately) | 11.9 s |
| one-line mutant, overlay | 3,421 | 4 | 18.7 MB, deleted at exit | **0 bytes** | 12.4 s |
| mutant built from a new directory, native | n/a | n/a | n/a | 224.5 MB in 2,169 files | 82 s |
| mutant built from a new directory, overlay | 2,368 | 459 | 224.3 MB, deleted at exit | **0 bytes** | ~120 s (under strace) |

To show that "0 bytes" was not just a quiet machine, the overlay runs were
traced with `strace -f`, following every process the go command started. They
made **no** write, create, rename, unlink, chmod, mkdir or utimes syscalls on
the shared dir. As a control, the same detector counted 270 such calls for a
tiny native build. Other builds on the same machine kept writing to the
shared cache during the measurement, so the trace is the only clean evidence
here; the before/after sizes are not.

## Limits

- Cache keys include each package's absolute directory. So does the
  test-result cache, which also uses file mtimes. A mutant built in a
  different directory from the one that filled the shared cache gets hits
  for the standard library and dependencies, and misses for its own module.
  Build mutants in the same directory as normal builds (or a fixed path)
  to get the most hits.
- The go command relinks every main and test binary on each build, so a
  link always misses. This happens with the native cache too.
- Shared entries that only mutants use do not get their mtimes refreshed,
  so the native trim can remove them after 5 days if no normal build uses
  them.
- If the helper is killed with SIGKILL, its scratch dir stays behind until
  the next sweep. The helper cleans up on SIGINT, SIGTERM and SIGHUP.

## Development

```sh
make lint    # gofmt + go vet
make test    # unit tests and integration tests with real go builds (~30 s)
make build
```

The integration tests (`main_test.go`) build the helper, seed a throwaway
shared cache natively, and drive real `go build` / `go test` runs through it.
They check:

- shared hits for unchanged packages;
- that the shared dir is unchanged after a mutant build: every file's
  path, mode, size, mtime and SHA-256 is compared;
- that a run id shares one dir across go commands, and `-remove-id` deletes it;
- concurrent go processes writing to one run dir;
- the startup sweep;
- the format checks;
- the `GOCACHE` prefix rule.

Each of these was seen to fail with a fault planted on purpose: writing
through to the shared dir, non-atomic writes, no shared reads, no scratch
reads, no sweep, and treating a bad entry header as a miss.

## Prior art

- [bradfitz/go-tool-cache](https://github.com/bradfitz/go-tool-cache): the
  reference `GOCACHEPROG` implementation.
- [vearutop/gocacheprog](https://github.com/vearutop/gocacheprog): a remote
  CI cache with a disk-size cap.
- [yonesko/gocacheprog](https://github.com/yonesko/gocacheprog): a local
  cache that writes through to Redis.

None of these reads an existing native cache without also writing to it.
