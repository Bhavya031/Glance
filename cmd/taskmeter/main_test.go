package main

import (
	"context"
	"io"
	"net/http"
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
