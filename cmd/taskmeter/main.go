package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
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

	"github.com/Bhavya031/Glance/internal/taskmeter"
)

var version = "dev"
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "taskmeter:", err)
		os.Exit(1)
	}
}

func run(args []string, out, diagnostics io.Writer) error {
	if len(args) == 0 {
		usage(out)
		return nil
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
		fmt.Fprintln(out, "taskmeter", version)
		return nil
	case "help", "--help", "-h":
		usage(out)
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func estimate(args []string, defaultConfig string, out, diagnostics io.Writer) error {
	f := flag.NewFlagSet("estimate", flag.ContinueOnError)
	f.SetOutput(diagnostics)
	repo := f.String("repo", "", "GitHub owner/repo")
	issueNumber := f.Int("issue", 0, "GitHub issue number")
	model := f.String("model", "", "worker model label")
	profile := f.String("profile", "standard", "estimation profile")
	configPath := f.String("config", defaultConfig, "config file")
	history := f.Bool("history", false, "include up to 100 issue comments")
	jsonOutput := f.Bool("json", false, "print stable JSON")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() > 1 {
		return errors.New("estimate accepts at most one local path")
	}
	c, err := taskmeter.LoadConfig(*configPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
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
	e, err := taskmeter.EstimatePath(path, issueText, resolvedRepo, *issueNumber, *model, *profile, c)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return json.NewEncoder(out).Encode(e)
	}
	fmt.Fprintf(out, "Glance estimate (%s, confidence %s)\n", e.Mode, e.Confidence)
	fmt.Fprintf(out, "Selected context: %d approximate tokens\n", e.SelectedContext.Tokens)
	fmt.Fprintf(out, "Whole run: %d–%d tokens (uncached %d–%d, cached %d–%d, output incl. reasoning %d–%d)\n", e.WholeRun.Total.Low, e.WholeRun.Total.High, e.WholeRun.UncachedInput.Low, e.WholeRun.UncachedInput.High, e.WholeRun.CachedInput.Low, e.WholeRun.CachedInput.High, e.WholeRun.OutputIncludingReasoning.Low, e.WholeRun.OutputIncludingReasoning.High)
	fmt.Fprintf(out, "Likely inspect: %s\nChange size: %d–%d files, %d–%d lines\n", strings.Join(e.LikelyInspect, ", "), e.ChangeSize.Files.Low, e.ChangeSize.Files.High, e.ChangeSize.Lines.Low, e.ChangeSize.Lines.High)
	fmt.Fprintln(out, "Approximation:", e.SelectedContext.Approximation)
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
	r := taskmeter.Record{ID: *id, Repo: *repo, Category: *category, Profile: *profile, Config: *configPath, Version: version, Predicted: taskmeter.Predicted{Low: *low, High: *high}, Actual: taskmeter.ActualTokens{UncachedInput: *uncached, CachedInput: *cached, OutputIncludingReasoning: *outputTokens}, VerifiedOutcome: *outcome, CensoredStatus: *censored, Provenance: *provenance, HeldOut: *heldOut}
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
	if err := taskmeter.AppendRecord(*records, r); err != nil {
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
	report, err := taskmeter.Calibrate(file)
	if err != nil {
		return err
	}
	if report.Factor != nil {
		c, err := taskmeter.LoadConfig(*configPath)
		if err != nil {
			return err
		}
		c.Calibration = *report.Factor
		if err := taskmeter.SaveConfig(*configPath, c); err != nil {
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
	c, err := taskmeter.LoadConfig(*path)
	if err != nil {
		return err
	}
	rest := f.Args()
	if len(rest) == 0 {
		return json.NewEncoder(out).Encode(struct {
			Config      taskmeter.Config `json:"config"`
			ConfigPath  string           `json:"config_path"`
			RecordsPath string           `json:"records_path"`
		}{c, *path, defaultRecords})
	}
	if len(rest) != 3 || rest[0] != "set" {
		return errors.New("usage: taskmeter config [-config path] [set <setting> <value>]")
	}
	if err := taskmeter.ParseConfigSet(&c, rest[1], rest[2]); err != nil {
		return err
	}
	return taskmeter.SaveConfig(*path, c)
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
	req.Header.Set("User-Agent", "Glance-taskmeter/"+version)
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
func snapshotRepo(parent context.Context, client *http.Client, repo string, config taskmeter.Config, diagnostics io.Writer) (string, func(), error) {
	dir, err := os.MkdirTemp("", "taskmeter-")
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
	req.Header.Set("User-Agent", "Glance-taskmeter/"+version)
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
	config, records := os.Getenv("TASKMETER_CONFIG"), os.Getenv("TASKMETER_RECORDS")
	if config != "" && records != "" {
		return config, records, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", "", err
	}
	if config == "" {
		config = filepath.Join(dir, "glance", "taskmeter.json")
	}
	if records == "" {
		records = filepath.Join(dir, "glance", "records.jsonl")
	}
	return config, records, nil
}
func usage(w io.Writer) {
	fmt.Fprintln(w, "Glance taskmeter — safe repository task/token estimator")
	fmt.Fprintln(w, "usage: taskmeter <estimate|record|calibrate|config|version> [arguments]")
}
