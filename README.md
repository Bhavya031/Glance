# Glance

Glance provides `taskmeter`, a single cross-platform CLI that estimates the
token range and likely change surface of a GitHub issue. Estimation is offline
after fetching the requested issue and, only when necessary, extracting a
bounded repository snapshot into an isolated temporary directory. Repository content is treated
as untrusted data: taskmeter never runs repository scripts, hooks, builds, or
tests and never modifies the checkout.

## Install and build

Go 1.22 or newer is required.

```sh
go install github.com/Bhavya031/Glance/cmd/taskmeter@latest
go build ./cmd/taskmeter
```

## Estimate

From any directory (downloads a bounded `owner/repo` snapshot if it is not the current
checkout):

```sh
taskmeter estimate --repo owner/repo --issue 123
taskmeter estimate --repo owner/repo --issue 123 --json
```

Inside the issue repository, the origin supplies `owner/repo`:

```sh
taskmeter estimate --issue 123
taskmeter estimate --issue 123 --history --model worker-model --profile standard
```

An explicit final path always selects an existing checkout:

```sh
taskmeter estimate --repo owner/repo --issue 123 ./checkout
```

Issue text comes from the GitHub API. `--history` adds at most 100 comments.
Set `GITHUB_TOKEN` for authorized private repositories or higher API limits;
the token is only sent in the Authorization header and is never printed or
stored. Diagnostics and warnings go to stderr so `--json` stdout stays clean.

The JSON contract is versioned by `schema_version`; see
[`examples/estimate.json`](examples/estimate.json). It separates selected
context from whole-run ranges and separates uncached input, cached input,
output including reasoning, and total. Total includes cached tokens.

## Record and calibrate

Record observed categories without combining cached and uncached tokens:

```sh
taskmeter record --id task-123 --repo owner/repo --category bug \
  --predicted-low 30000 --predicted-high 80000 \
  --actual-uncached 42000 --actual-cached 9000 --actual-output 12000 \
  --duration 640 --outcome passed --provenance provider-usage

taskmeter calibrate
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
taskmeter config
taskmeter config set max_files 20000
taskmeter config set max_bytes 67108864
taskmeter config set chars_per_token 4.2
taskmeter config set subscription_tokens unknown
taskmeter estimate --config ./taskmeter.json --issue 123
```

The default is shown in [`examples/config.json`](examples/config.json).
Configuration normally lives in the OS user config directory at
`glance/taskmeter.json`; records use `glance/records.jsonl`. Override the paths
with `TASKMETER_CONFIG` and `TASKMETER_RECORDS`, or command flags.

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
- Tokens use `ceil(UTF-8 bytes / chars_per_token)`, not a model tokenizer.
  Non-ASCII text is conservatively counted. Likely files are bounded
  filename/text matches, not semantic analysis.
- Whole-run and change-size ranges are planning heuristics. Tool output,
  generated context, conversation history, reasoning behavior, and final output
  size remain unknown. Calibration can mislead when workload/model changes.
- Remote planning mode is not implemented in this stage. No endpoint is called
  and no planning spend can occur.
- Files changing during a scan may make a result inconsistent; rerun in a quiet
  checkout.

## Release and verification

Tags named `v*` run GitHub Actions and GoReleaser to build `taskmeter` archives
for Linux, macOS, and Windows on amd64 and arm64. Local verification:

```sh
go test ./...
go vet ./...
go build ./cmd/taskmeter
```
