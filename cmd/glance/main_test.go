package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
