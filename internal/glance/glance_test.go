package glance

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEstimateArithmeticAndLimits(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "task.go"), []byte("fix parser task"))
	mustWrite(t, filepath.Join(dir, ".git", "secret"), []byte("ignored"))
	mustWrite(t, filepath.Join(dir, "image.bin"), []byte{'a', 0, 'b'})
	mustWrite(t, filepath.Join(dir, "large.txt"), []byte("this is far too large"))
	c := DefaultConfig()
	c.MaxFileBytes = 15
	e, err := EstimatePath(dir, []byte("fix task parser"), "owner/repo", 1, "worker", "standard", c)
	if err != nil {
		t.Fatal(err)
	}
	if e.SelectedContext.IssueTokens != 4 || e.SelectedContext.LikelyFileTokens != 4 || e.SelectedContext.Tokens != 8 {
		t.Fatalf("unexpected context: %+v", e.SelectedContext)
	}
	if e.WholeRun.Total.Low != 10 || e.WholeRun.Total.High != 38 {
		t.Fatalf("unexpected total: %+v", e.WholeRun.Total)
	}
	if e.ScannedFiles != 1 || e.SkippedBinary != 1 || e.SkippedLarge != 1 {
		t.Fatalf("limits/exclusions failed: %+v", e)
	}
}

func TestPriceConversionAndUnknownCapacity(t *testing.T) {
	input, cached, output := 2.0, 0.2, 10.0
	cost := CalculateCosts(RunTokens{UncachedInput: Range{1_000_000, 2_000_000}, CachedInput: Range{500_000, 1_000_000}, OutputIncludingReasoning: Range{100_000, 200_000}, Total: Range{1_600_000, 3_200_000}}, Prices{Currency: "USD", UncachedInputPerMillion: &input, CachedInputPerMillion: &cached, OutputPerMillion: &output}, nil, PlanningLabel)
	if cost.APIEquivalent["uncached_input"].High != 4 || cost.APIEquivalent["cached_input"].Low != .1 || cost.APIEquivalent["output_including_reasoning"].High != 2 {
		t.Fatalf("bad costs: %+v", cost)
	}
	if cost.ActualCash != nil || cost.Subscription.CapacityTokens != nil || cost.Subscription.EstimatedShare != nil {
		t.Fatal("unknown values must stay unknown")
	}
}

func TestConfigOverrideAndWindowsSensitivePath(t *testing.T) {
	c := DefaultConfig()
	if err := ParseConfigSet(&c, "max_files", "7"); err != nil || c.MaxFiles != 7 {
		t.Fatalf("override failed: %v %+v", err, c)
	}
	if !sensitivePath(`C:\\work\\repo\\.env.local`) || !sensitivePath(`C:\\work\\id_ed25519`) {
		t.Fatal("Windows-style credential paths not excluded")
	}
}

func TestWorktreeGitFileIsExcluded(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, ".git"), []byte("gitdir: elsewhere"))
	mustWrite(t, filepath.Join(dir, "task.go"), []byte("task"))
	e, err := EstimatePath(dir, []byte("task"), "", 0, "", "standard", DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if e.ScannedFiles != 1 || len(e.LikelyInspect) != 1 || e.LikelyInspect[0] != "task.go" {
		t.Fatalf("worktree metadata was included: %+v", e)
	}
}

func TestBudgetStoppedExcludedFromCalibration(t *testing.T) {
	data := strings.Join([]string{
		`{"id":"ok","predicted":{"low":80,"high":120},"actual":{"uncached_input":150},"censored_status":"none"}`,
		`{"id":"stop","predicted":{"low":1,"high":1},"actual":{"uncached_input":10000},"censored_status":"budget-stopped"}`,
		`{"id":"held","predicted":{"low":100,"high":200},"actual":{"uncached_input":150},"censored_status":"none","held_out":true}`,
	}, "\n")
	report, err := Calibrate(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if report.Tuning.BudgetStopped != 1 || report.Tuning.Eligible != 1 || report.HeldOut.Eligible != 1 || report.Factor == nil || math.Abs(*report.Factor-1.5) > 1e-9 {
		t.Fatalf("bad report: %+v", report)
	}
}

func TestEmptyHistoryHasUnknownMetrics(t *testing.T) {
	report, err := Calibrate(strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if report.Factor != nil || report.Tuning.Coverage != nil || report.Tuning.HistoricalMedianBaseline != nil {
		t.Fatalf("empty history should remain unknown: %+v", report)
	}
}

func mustWrite(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
