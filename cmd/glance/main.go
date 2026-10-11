package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Bhavya031/Glance/internal/glance"
)

var version = "dev"
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "glance:", err)
		os.Exit(1)
	}
}

func run(args []string, out, diagnostics io.Writer) error {
	if len(args) == 0 {
		usage(out)
		return nil
	}
	if args[0] == "count" {
		return count(args[1:], os.Stdin, out, diagnostics)
	}
	defaultConfig, defaultRecords, err := paths()
	if err != nil {
		return err
	}
	switch args[0] {
	case "estimate":
		return estimate(args[1:], defaultConfig, out, diagnostics)
	case "record":
		return record(args[1:], defaultConfig, defaultRecords, out)
	case "calibrate":
		return calibrate(args[1:], defaultConfig, defaultRecords, out)
	case "config":
		return configCommand(args[1:], defaultConfig, defaultRecords, out)
	case "version", "--version", "-version":
		fmt.Fprintln(out, "glance", version)
		return nil
	case "help", "--help", "-h":
		usage(out)
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

const maxCountBytes = 8 << 20

func count(args []string, in io.Reader, out, diagnostics io.Writer) error {
	f := flag.NewFlagSet("count", flag.ContinueOnError)
	f.SetOutput(diagnostics)
	jsonOutput := f.Bool("json", false, "print JSON")
	encoding := f.String("encoding", glance.DefaultEncoding, "tokenizer encoding: o200k_base or cl100k_base")
	if err := f.Parse(args); err != nil {
		return err
	}
	if !*jsonOutput || f.NArg() != 1 {
		return errors.New("usage: glance count --json [--encoding o200k_base|cl100k_base] <file|->")
	}
	if _, err := glance.CountTokens("", *encoding); err != nil {
		return err
	}
	if path := f.Arg(0); path != "-" {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("count requires a regular file or stdin (-)")
		}
		in = file
	}
	data, err := io.ReadAll(io.LimitReader(in, maxCountBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxCountBytes {
		return fmt.Errorf("input exceeds %d-byte limit", maxCountBytes)
	}
	if !utf8.Valid(data) {
		return errors.New("input must be valid UTF-8")
	}
	tokens, err := glance.CountTokens(string(data), *encoding)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(struct {
		SchemaVersion string `json:"schema_version"`
		GlanceVersion string `json:"glance_version"`
		Encoding      string `json:"encoding"`
		ContextTokens int    `json:"context_tokens"`
		InputBytes    int    `json:"input_bytes"`
		InputSHA256   string `json:"input_sha256"`
	}{"1", version, *encoding, tokens, len(data), fmt.Sprintf("%x", sha256.Sum256(data))})
}

func estimate(args []string, defaultConfig string, out, diagnostics io.Writer) error {
	f := flag.NewFlagSet("estimate", flag.ContinueOnError)
	f.SetOutput(diagnostics)
	repo := f.String("repo", "", "GitHub owner/repo")
	issueNumber := f.Int("issue", 0, "GitHub issue number")
	model := f.String("model", "", "worker model label")
	profile := f.String("profile", "standard", "estimation profile")
	encoding := f.String("encoding", glance.DefaultEncoding, "tokenizer encoding: o200k_base or cl100k_base")
	specialTokens := f.Bool("special-tokens", false, "request special-token processing (unsupported)")
	configPath := f.String("config", defaultConfig, "config file")
	history := f.Bool("history", false, "include up to 100 issue comments")
	jsonOutput := f.Bool("json", false, "print stable JSON")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() > 1 {
		return errors.New("estimate accepts at most one local path")
	}
	if *specialTokens {
		return errors.New("special-token processing is not supported; omit --special-tokens")
	}
	c, err := glance.LoadConfig(*configPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if _, ok := c.Profiles[*profile]; !ok {
		return fmt.Errorf("unknown profile %q", *profile)
	}
	if _, err := glance.CountTokens("", *encoding); err != nil {
		return err
	}
	path := "."
	explicitPath := f.NArg() == 1
	if explicitPath {
		path = f.Arg(0)
	}
	resolvedRepo := *repo
	if *issueNumber < 0 {
		return errors.New("issue must be positive")
	}
	if *issueNumber > 0 && resolvedRepo == "" {
		resolvedRepo, err = localRepo(path)
		if err != nil {
			return fmt.Errorf("cannot infer GitHub repo for issue: %w", err)
		}
	}
	if resolvedRepo != "" && !repoPattern.MatchString(resolvedRepo) {
		return errors.New("repo must be owner/name")
	}
	var issueText []byte
	if *issueNumber > 0 {
		issueText, err = fetchIssue(context.Background(), http.DefaultClient, resolvedRepo, *issueNumber, *history)
		if err != nil {
			return err
		}
	}
	var cleanup func()
	if resolvedRepo != "" && !explicitPath {
		if local, localErr := localRepo("."); localErr != nil || !strings.EqualFold(local, resolvedRepo) {
			path, cleanup, err = snapshotRepo(context.Background(), http.DefaultClient, resolvedRepo, c, diagnostics)
			if err != nil {
				return err
			}
			defer cleanup()
		}
	}
	e, err := glance.EstimatePath(path, issueText, resolvedRepo, *issueNumber, *model, *profile, *encoding, c)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return json.NewEncoder(out).Encode(e)
	}
	fmt.Fprintf(out, "Glance estimate (%s, confidence %s)\n", e.Mode, e.Confidence)
	fmt.Fprintf(out, "Selected context: %d tokens measured with %s\n", e.ContextTokens, e.Encoding)
	fmt.Fprintf(out, "Estimated input: %d–%d tokens (central %d)\n", e.EstimatedInput.Low, e.EstimatedInput.High, e.EstimatedInput.Central)
	fmt.Fprintf(out, "Estimated output: %d–%d tokens (central %d)\n", e.EstimatedOutput.Low, e.EstimatedOutput.High, e.EstimatedOutput.Central)
	fmt.Fprintf(out, "Whole run: %d–%d tokens (uncached %d–%d, cached %d–%d, output incl. reasoning %d–%d)\n", e.WholeRun.Total.Low, e.WholeRun.Total.High, e.WholeRun.UncachedInput.Low, e.WholeRun.UncachedInput.High, e.WholeRun.CachedInput.Low, e.WholeRun.CachedInput.High, e.WholeRun.OutputIncludingReasoning.Low, e.WholeRun.OutputIncludingReasoning.High)
	fmt.Fprintf(out, "Likely inspect: %s\nChange size: %d–%d files, %d–%d lines\n", strings.Join(e.LikelyInspect, ", "), e.ChangeSize.Files.Low, e.ChangeSize.Files.High, e.ChangeSize.Lines.Low, e.ChangeSize.Lines.High)
	fmt.Fprintln(out, "Estimation method:", e.EstimationMethod)
	for _, excluded := range e.ExcludedFiles {
		fmt.Fprintf(out, "Excluded: %s (%s)\n", excluded.Path, excluded.Reason)
	}
	for _, evidence := range e.Evidence {
		fmt.Fprintln(out, "Evidence:", evidence)
	}
	for _, assumption := range e.Assumptions {
		fmt.Fprintln(out, "Assumption:", assumption)
	}
	for _, missing := range e.MissingInfo {
		fmt.Fprintln(out, "Missing:", missing)
	}
	for _, warning := range e.Warnings {
		fmt.Fprintln(diagnostics, "warning:", warning)
	}
	return nil
}

func record(args []string, defaultConfig, defaultRecords string, out io.Writer) error {
	f := flag.NewFlagSet("record", flag.ContinueOnError)
	id := f.String("id", "", "record id")
	repo := f.String("repo", "", "owner/repo")
	category := f.String("category", "", "task category")
	profile := f.String("profile", "standard", "profile")
	configPath := f.String("config", defaultConfig, "config provenance")
	records := f.String("records", defaultRecords, "records JSONL")
	low := f.Int64("predicted-low", 0, "predicted total low")
	high := f.Int64("predicted-high", 0, "predicted total high")
	uncached := f.Int64("actual-uncached", 0, "actual uncached input")
	cached := f.Int64("actual-cached", 0, "actual cached input")
	outputTokens := f.Int64("actual-output", 0, "actual output including reasoning")
	outcome := f.String("outcome", "unknown", "verified outcome")
	censored := f.String("censored", "none", "none, failed, or budget-stopped")
	provenance := f.String("provenance", "user", "measurement provenance")
	heldOut := f.Bool("held-out", false, "reserve for held-out metrics")
	warning := f.String("warning", "", "record warning")
	spend := f.Float64("spend", -1, "actual cash spend")
	duration := f.Float64("duration", -1, "duration seconds")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("record accepts flags only")
	}
	r := glance.Record{ID: *id, Repo: *repo, Category: *category, Profile: *profile, Config: *configPath, Version: version, Predicted: glance.Predicted{Low: *low, High: *high}, Actual: glance.ActualTokens{UncachedInput: *uncached, CachedInput: *cached, OutputIncludingReasoning: *outputTokens}, VerifiedOutcome: *outcome, CensoredStatus: *censored, Provenance: *provenance, HeldOut: *heldOut}
	if *warning != "" {
		r.Warnings = []string{*warning}
	}
	if *spend >= 0 {
		r.Spend = spend
	}
	if *duration >= 0 {
		r.DurationSeconds = duration
	}
	if *censored != "none" && *censored != "failed" && *censored != "budget-stopped" {
		return errors.New("censored must be none, failed, or budget-stopped")
	}
	if err := glance.AppendRecord(*records, r); err != nil {
		return err
	}
	fmt.Fprintln(out, "recorded", *id)
	return nil
}

func calibrate(args []string, defaultConfig, defaultRecords string, out io.Writer) error {
	f := flag.NewFlagSet("calibrate", flag.ContinueOnError)
	configPath := f.String("config", defaultConfig, "config file")
	records := f.String("records", defaultRecords, "records JSONL")
	if err := f.Parse(args); err != nil {
		return err
	}
	file, err := os.Open(*records)
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("no history found; record completed tasks first")
	}
	if err != nil {
		return err
	}
	defer file.Close()
	report, err := glance.Calibrate(file)
	if err != nil {
		return err
	}
	if report.Factor != nil {
		c, err := glance.LoadConfig(*configPath)
		if err != nil {
			return err
		}
		c.Calibration = *report.Factor
		if err := glance.SaveConfig(*configPath, c); err != nil {
			return err
		}
	}
	return json.NewEncoder(out).Encode(report)
}

func configCommand(args []string, defaultConfig, defaultRecords string, out io.Writer) error {
	f := flag.NewFlagSet("config", flag.ContinueOnError)
	path := f.String("config", defaultConfig, "config file")
	if err := f.Parse(args); err != nil {
		return err
	}
	c, err := glance.LoadConfig(*path)
	if err != nil {
		return err
	}
	rest := f.Args()
	if len(rest) == 0 {
		return json.NewEncoder(out).Encode(struct {
			Config      glance.Config `json:"config"`
			ConfigPath  string        `json:"config_path"`
			RecordsPath string        `json:"records_path"`
		}{c, *path, defaultRecords})
	}
	if len(rest) != 3 || rest[0] != "set" {
		return errors.New("usage: glance config [-config path] [set <setting> <value>]")
	}
	if err := glance.ParseConfigSet(&c, rest[1], rest[2]); err != nil {
		return err
	}
	return glance.SaveConfig(*path, c)
}

func fetchIssue(parent context.Context, client *http.Client, repo string, number int, history bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	endpoint := "https://api.github.com/repos/" + repo + "/issues/" + strconv.Itoa(number)
	type issueResponse struct {
		Title   string `json:"title"`
		Body    string `json:"body"`
		Message string `json:"message"`
	}
	var issue issueResponse
	if err := githubJSON(ctx, client, endpoint, &issue); err != nil {
		return nil, err
	}
	text := issue.Title + "\n\n" + issue.Body
	if history {
		var comments []struct {
			Body string `json:"body"`
		}
		if err := githubJSON(ctx, client, endpoint+"/comments?per_page=100", &comments); err != nil {
			return nil, fmt.Errorf("issue history: %w", err)
		}
		for _, comment := range comments {
			text += "\n\n" + comment.Body
		}
	}
	if len(text) > 1<<20 {
		return nil, errors.New("issue and history exceed 1048576-byte limit")
	}
	return []byte(text), nil
}
func githubJSON(ctx context.Context, client *http.Client, endpoint string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Glance/"+version)
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub network error: %w", err)
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, (1<<20)+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return err
	}
	if len(body) > 1<<20 {
		return errors.New("GitHub response exceeds 1048576-byte limit")
	}
	if resp.StatusCode != http.StatusOK {
		switch resp.StatusCode {
		case http.StatusNotFound:
			return errors.New("GitHub repo or issue is missing or private (set GITHUB_TOKEN if authorized)")
		case http.StatusForbidden, http.StatusTooManyRequests:
			return errors.New("GitHub API rate limit or access denied")
		}
		return fmt.Errorf("GitHub API returned %s", resp.Status)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("invalid GitHub response: %w", err)
	}
	return nil
}
func localRepo(path string) (string, error) {
	cmd := exec.Command("git", "-C", path, "config", "--get", "remote.origin.url")
	b, err := cmd.Output()
	if err != nil {
		return "", errors.New("Git is unavailable or checkout has no origin")
	}
	raw := strings.TrimSpace(string(b))
	raw = strings.TrimSuffix(raw, ".git")
	if strings.HasPrefix(raw, "git@github.com:") {
		raw = strings.TrimPrefix(raw, "git@github.com:")
	} else {
		u, err := url.Parse(raw)
		if err != nil || !strings.EqualFold(u.Host, "github.com") {
			return "", errors.New("origin is not a GitHub repository")
		}
		raw = strings.TrimPrefix(u.Path, "/")
	}
	if !repoPattern.MatchString(raw) {
		return "", errors.New("could not parse owner/repo from origin")
	}
	return raw, nil
}
func snapshotRepo(parent context.Context, client *http.Client, repo string, config glance.Config, diagnostics io.Writer) (string, func(), error) {
	dir, err := os.MkdirTemp("", "glance-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	fmt.Fprintln(diagnostics, "info: extracting a read-only snapshot into isolated temporary directory", dir)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+repo+"/tarball", nil)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Glance/"+version)
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("GitHub snapshot network error: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		cleanup()
		if resp.StatusCode == http.StatusNotFound {
			return "", nil, errors.New("GitHub repository is missing or private (set GITHUB_TOKEN if authorized)")
		}
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			return "", nil, errors.New("GitHub API rate limit or access denied")
		}
		return "", nil, fmt.Errorf("GitHub snapshot returned %s", resp.Status)
	}
	gz, err := gzip.NewReader(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("invalid GitHub snapshot: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var files int
	var total int64
	entries := 0
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanup()
			return "", nil, fmt.Errorf("read GitHub snapshot: %w", err)
		}
		entries++
		if entries > config.MaxFiles*5 {
			cleanup()
			return "", nil, errors.New("GitHub snapshot exceeds entry limit")
		}
		name := filepath.ToSlash(filepath.Clean(header.Name))
		parts := strings.SplitN(name, "/", 2)
		if len(parts) != 2 || parts[1] == "" {
			continue
		}
		rel := filepath.FromSlash(parts[1])
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			cleanup()
			return "", nil, errors.New("GitHub snapshot contains an unsafe path")
		}
		target := filepath.Join(dir, rel)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				cleanup()
				return "", nil, err
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || header.Size > config.MaxFileBytes {
				continue
			}
			if files >= config.MaxFiles || header.Size > config.MaxBytes-total {
				cleanup()
				return "", nil, errors.New("GitHub snapshot exceeds configured file or byte limit; use a local checkout or raise the limit")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				cleanup()
				return "", nil, err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				cleanup()
				return "", nil, err
			}
			_, copyErr := io.CopyN(file, tr, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				cleanup()
				return "", nil, copyErr
			}
			if closeErr != nil {
				cleanup()
				return "", nil, closeErr
			}
			files++
			total += header.Size
		}
	}
	return dir, cleanup, nil
}
func paths() (string, string, error) {
	config, records := os.Getenv("GLANCE_CONFIG"), os.Getenv("GLANCE_RECORDS")
	if config != "" && records != "" {
		return config, records, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", "", err
	}
	if config == "" {
		config = filepath.Join(dir, "glance", "glance.json")
	}
	if records == "" {
		records = filepath.Join(dir, "glance", "records.jsonl")
	}
	return config, records, nil
}
func usage(w io.Writer) {
	fmt.Fprintln(w, "Glance — safe repository task/token estimator")
	fmt.Fprintln(w, "usage: glance <count|estimate|record|calibrate|config|version> [arguments]")
	fmt.Fprintln(w, "       glance count --json [--encoding o200k_base|cl100k_base] <file|->")
}
