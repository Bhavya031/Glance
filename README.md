# Glance

Glance is a single cross-platform CLI that measures selected-context tokens and
estimates the whole-run token range and likely change surface of a GitHub issue. Estimation is offline
after fetching the requested issue and, only when necessary, extracting a
bounded repository snapshot into an isolated temporary directory. Repository content is treated
as untrusted data: Glance never runs repository scripts, hooks, builds, or
tests and never modifies the checkout.

## Install and build

Go 1.22 or newer is required.

```sh
go install github.com/Bhavya031/Glance/cmd/glance@latest
go build ./cmd/glance
```

## Estimate

From any directory (downloads a bounded `owner/repo` snapshot if it is not the current
checkout):

```sh
glance estimate --repo owner/repo --issue 123
glance estimate --repo owner/repo --issue 123 --json
```

Inside the issue repository, the origin supplies `owner/repo`:

```sh
glance estimate --issue 123
glance estimate --issue 123 --history --model worker-model --profile standard
glance estimate --issue 123 --encoding cl100k_base
```

An explicit final path always selects an existing checkout:

```sh
glance estimate --repo owner/repo --issue 123 ./checkout
```

Issue text comes from the GitHub API. `--history` adds at most 100 comments.
Set `GITHUB_TOKEN` for authorized private repositories or higher API limits;
the token is only sent in the Authorization header and is never printed or
stored. Diagnostics and warnings go to stderr so `--json` stdout stays clean.

The default encoding is `o200k_base`; `--encoding cl100k_base` is the only
override. Both vocabularies are compiled into the Go dependency, so normal
runtime use performs no vocabulary download and requires no Python or Node.
Explicit special-token processing is unsupported and `--special-tokens` is
rejected rather than silently changing behavior.

Glance assembles the measured context deterministically as an `Instructions:`
section followed by each selected `File: path`, separator, and exact file
contents. It tokenizes that complete string once, so paths and separators are
included and isolated file counts are not added together.

The JSON contract is versioned by `schema_version`; see
[`examples/estimate.json`](examples/estimate.json). It separates selected
context from heuristic whole-run ranges, reports selected files and actionable
scan/context exclusions with reasons, and separates uncached input, cached input, output including
reasoning, and total. Total includes cached tokens.

## Record and calibrate

Record observed categories without combining cached and uncached tokens:

```sh
glance record --id task-123 --repo owner/repo --category bug \
  --predicted-low 30000 --predicted-high 80000 \
  --actual-uncached 42000 --actual-cached 9000 --actual-output 12000 \
  --duration 640 --outcome passed --provenance provider-usage

glance calibrate
```

`record` writes JSONL with ID, timestamp, repository, category, profile,
config/version provenance, predicted range, actual token categories, optional
spend/duration, verified outcome, censor status, provenance, warnings, and the
held-out marker. Use `--censored failed` or `--censored budget-stopped`; neither
is used to tune the factor. Use `--held-out` to reserve a completed observation
for held-out reporting.

`calibrate` reports observation count, eligible coverage, average range width,
median central-estimate error, underestimation frequency, and the historical
median baseline separately for tuning and held-out records. Missing history is
an explicit error; empty/zero-actual groups remain `null`, never zero. Only
eligible tuning records update the config calibration factor.

## Configuration and costs

```sh
glance config
glance config set max_files 20000
glance config set max_bytes 67108864
glance config set subscription_tokens unknown
glance estimate --config ./glance.json --issue 123
```

The default is shown in [`examples/config.json`](examples/config.json).
Configuration normally lives in the OS user config directory at
`glance/glance.json`; records use `glance/records.jsonl`. Override the paths
with `GLANCE_CONFIG` and `GLANCE_RECORDS`, or command flags.

No provider prices are built in. Supply separate per-million prices for
uncached input, cached input, and output in the JSON config. Output reports
API-equivalent cost by category, while actual cash spend stays unknown until
recorded. Subscription quota is separate. The editable 200,000,000-token
default is labeled exactly “user-selected planning assumption, not
official/verified”; set it to `null` or use the command above when unknown.

## Safety limits and limitations

- The scan defaults to 10,000 files, 32 MiB total, 1 MiB per file, 20 likely
  files, and at most 50 issue search terms. It excludes VCS/dependency/build
  directories, symlinks, non-regular/binary/non-UTF-8 files, and common
  credential filenames and key extensions.
- GitHub responses and combined issue history are each capped at 1 MiB with a
  15-second request timeout. A required snapshot is capped at 64 MiB compressed
  and 45 seconds, enforces configured extraction limits, is disclosed on stderr,
  and is removed afterward. Git is only required to infer a local origin.
- `context_tokens` is exact for the assembled text and named tokenizer
  encoding, not a promise of cross-model usage. Whole-run input/output ranges
  remain profile heuristics with low confidence until representative history
  exists. Likely files are bounded filename/text matches, not semantic analysis.
- Whole-run and change-size ranges are planning heuristics. Tool output,
  generated context, conversation history, reasoning behavior, and final output
  size remain unknown. Calibration can mislead when workload/model changes.
- Remote planning mode is not implemented in this stage. No endpoint is called
  and no planning spend can occur.
- Files changing during a scan may make a result inconsistent; rerun in a quiet
  checkout.

## Release and verification

Tags named `v*` run GitHub Actions and GoReleaser to build `glance` archives
for Linux, macOS, and Windows on amd64 and arm64. Archives include
`THIRD_PARTY_NOTICES.txt`. Local verification:

```sh
go test ./...
go vet ./...
go build ./cmd/glance
```

Tokenizer fixtures were compared once with official Python `tiktoken` 0.14.0;
normal Go tests read the saved counts and do not invoke Python:

```sh
python3 -m venv /tmp/glance-tiktoken-venv
/tmp/glance-tiktoken-venv/bin/pip install tiktoken==0.14.0
/tmp/glance-tiktoken-venv/bin/python -c 'import json,tiktoken; p=json.load(open("internal/glance/testdata/token_counts.json")); e={n:tiktoken.get_encoding(n) for n in ("o200k_base","cl100k_base")}; assert all(len(e[n].encode(x["text"]))==x[n] for x in p for n in e); print("verified",len(p),"fixtures against official Python tiktoken")'
```

The Go runtime pins `github.com/tiktoken-go/tokenizer` v0.4.0, the newest
release supporting both required encodings without raising Glance's Go 1.22
baseline. Its full upstream MIT notice is in `THIRD_PARTY_NOTICES.txt`.
