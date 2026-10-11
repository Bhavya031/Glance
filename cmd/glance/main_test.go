package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Bhavya031/Glance/internal/glance"
)

func TestCountExactBytesFromFileAndStdin(t *testing.T) {
	for name, input := range map[string][]byte{
		"empty":   {},
		"prism":   []byte("Instructions:\nFix the task\n\nFile: a.go\n---\nhello\n---\n\nFile: b.go\n---\nworld\n---\n"),
		"unicode": []byte("café 世界\r\nlast line\r\n"),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "input.txt")
			if err := os.WriteFile(path, input, 0o600); err != nil {
				t.Fatal(err)
			}
			var fileOut, stdinOut, diagnostics strings.Builder
			if err := count([]string{"--json", path}, strings.NewReader("unused"), &fileOut, &diagnostics); err != nil {
				t.Fatal(err)
			}
			if err := count([]string{"--json", "-"}, strings.NewReader(string(input)), &stdinOut, &diagnostics); err != nil {
				t.Fatal(err)
			}
			if fileOut.String() != stdinOut.String() {
				t.Fatalf("file and stdin differ: %q != %q", fileOut.String(), stdinOut.String())
			}
			var got struct {
				SchemaVersion string `json:"schema_version"`
				GlanceVersion string `json:"glance_version"`
				Encoding      string `json:"encoding"`
				ContextTokens int    `json:"context_tokens"`
				InputBytes    int    `json:"input_bytes"`
				InputSHA256   string `json:"input_sha256"`
			}
			if err := json.Unmarshal([]byte(fileOut.String()), &got); err != nil {
				t.Fatal(err)
			}
			if got.SchemaVersion != "1" || got.GlanceVersion != version || got.Encoding != "o200k_base" || got.InputBytes != len(input) || got.InputSHA256 != fmt.Sprintf("%x", sha256.Sum256(input)) {
				t.Fatalf("unexpected result: %+v", got)
			}
			want, err := glance.CountTokens(string(input), glance.DefaultEncoding)
			if err != nil || got.ContextTokens != want {
				t.Fatalf("tokens=%d, want %d: %v", got.ContextTokens, want, err)
			}
		})
	}
}

func TestCountRejectsInvalidInputsWithoutJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.txt")
	for name, input := range map[string][]byte{
		"invalid UTF-8": {0xff},
		"NUL":           {'a', 0, 'b'},
		"oversized":     []byte(strings.Repeat("x", maxCountBytes+1)),
	} {
		t.Run(name, func(t *testing.T) {
			var out, diagnostics strings.Builder
			if err := count([]string{"--json", "-"}, strings.NewReader(string(input)), &out, &diagnostics); err == nil || out.Len() != 0 {
				t.Fatalf("error=%v, output=%q", err, out.String())
			}
		})
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--json", path}, {"--json"}, {"--json", "a", "b"}, {"-"}, {"--json", "--encoding", "wrong", "-"}} {
		var out, diagnostics strings.Builder
		if err := count(args, strings.NewReader("text"), &out, &diagnostics); err == nil || out.Len() != 0 {
			t.Fatalf("args=%q, error=%v, output=%q", args, err, out.String())
		}
	}
}

func TestCountRunDoesNotUseConfigOrNetwork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.txt")
	if err := os.WriteFile(path, []byte("raw text\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GLANCE_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("GLANCE_RECORDS", filepath.Join(t.TempDir(), "missing.jsonl"))
	oldTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("count attempted network access")
		return nil, nil
	})
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	var out, diagnostics strings.Builder
	if err := run([]string{"count", "--json", "--encoding", "cl100k_base", path}, &out, &diagnostics); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"encoding":"cl100k_base"`) || diagnostics.Len() != 0 {
		t.Fatalf("output=%q, diagnostics=%q", out.String(), diagnostics.String())
	}
}

func TestGitHubErrorsAreClear(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests", Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}
	var target any
	err := githubJSON(context.Background(), client, "https://example.invalid", &target)
	if err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("unexpected error: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestCalibrateMissingHistory(t *testing.T) {
	err := calibrate([]string{"-records", filepath.Join(t.TempDir(), "missing.jsonl")}, filepath.Join(t.TempDir(), "config.json"), "unused", &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "no history found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGlanceNameAndPaths(t *testing.T) {
	var out, diagnostics strings.Builder
	if err := run([]string{"help"}, &out, &diagnostics); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Glance") || !strings.Contains(out.String(), "usage: glance ") {
		t.Fatalf("stale help text: %q", out.String())
	}
	t.Setenv("GLANCE_CONFIG", filepath.Join(t.TempDir(), "glance.json"))
	t.Setenv("GLANCE_RECORDS", filepath.Join(t.TempDir(), "records.jsonl"))
	config, records, err := paths()
	if err != nil || config != os.Getenv("GLANCE_CONFIG") || records != os.Getenv("GLANCE_RECORDS") {
		t.Fatalf("paths=(%q, %q), err=%v", config, records, err)
	}
}

func TestSpecialTokenProcessingIsRejected(t *testing.T) {
	err := estimate([]string{"--special-tokens"}, filepath.Join(t.TempDir(), "config.json"), &strings.Builder{}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "special-token processing is not supported") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEstimateOptionsFailBeforeRepositoryWork(t *testing.T) {
	for name, args := range map[string][]string{
		"encoding": {"--encoding", "o200k", "--repo", "owner/repo", "--issue", "123"},
		"profile":  {"--profile", "missing", "--repo", "owner/repo", "--issue", "123"},
	} {
		t.Run(name, func(t *testing.T) {
			err := estimate(args, filepath.Join(t.TempDir(), "config.json"), &strings.Builder{}, &strings.Builder{})
			if err == nil || (!strings.Contains(err.Error(), "unsupported encoding") && !strings.Contains(err.Error(), "unknown profile")) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
