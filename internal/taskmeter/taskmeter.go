package taskmeter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	SchemaVersion        = "1"
	DefaultCharsPerToken = 4.0
	DefaultMaxFiles      = 10_000
	DefaultMaxBytes      = int64(32 << 20)
	DefaultMaxFileBytes  = int64(1 << 20)
	DefaultLikelyFiles   = 20
	PlanningLabel        = "user-selected planning assumption, not official/verified"
)

type Range struct {
	Low  int64 `json:"low"`
	High int64 `json:"high"`
}
type Profile struct {
	UncachedInput [2]float64 `json:"uncached_input"`
	CachedInput   [2]float64 `json:"cached_input"`
	Output        [2]float64 `json:"output_including_reasoning"`
}
type Prices struct {
	Currency                string   `json:"currency"`
	UncachedInputPerMillion *float64 `json:"uncached_input_per_million,omitempty"`
	CachedInputPerMillion   *float64 `json:"cached_input_per_million,omitempty"`
	OutputPerMillion        *float64 `json:"output_per_million,omitempty"`
}
type Config struct {
	CharsPerToken      float64            `json:"chars_per_token"`
	MaxFiles           int                `json:"max_files"`
	MaxBytes           int64              `json:"max_bytes"`
	MaxFileBytes       int64              `json:"max_file_bytes"`
	LikelyFiles        int                `json:"likely_files"`
	Calibration        float64            `json:"calibration"`
	Profiles           map[string]Profile `json:"profiles"`
	Prices             Prices             `json:"prices"`
	SubscriptionTokens *int64             `json:"subscription_tokens,omitempty"`
	SubscriptionLabel  string             `json:"subscription_label"`
}

func DefaultConfig() Config {
	capacity := int64(200_000_000)
	return Config{
		CharsPerToken: DefaultCharsPerToken, MaxFiles: DefaultMaxFiles, MaxBytes: DefaultMaxBytes,
		MaxFileBytes: DefaultMaxFileBytes, LikelyFiles: DefaultLikelyFiles, Calibration: 1,
		Profiles: map[string]Profile{"standard": {UncachedInput: [2]float64{1, 3}, CachedInput: [2]float64{0, 1}, Output: [2]float64{0.15, 0.75}}},
		Prices:   Prices{Currency: "USD"}, SubscriptionTokens: &capacity, SubscriptionLabel: PlanningLabel,
	}
}
func (c Config) Validate() error {
	if c.CharsPerToken < 1 || c.CharsPerToken > 20 {
		return errors.New("chars_per_token must be between 1 and 20")
	}
	if c.MaxFiles < 1 || c.MaxFiles > 1_000_000 {
		return errors.New("max_files must be between 1 and 1000000")
	}
	if c.MaxBytes < 1 || c.MaxBytes > 1<<40 {
		return errors.New("max_bytes must be between 1 and 1099511627776")
	}
	if c.MaxFileBytes < 1 || c.MaxFileBytes > c.MaxBytes {
		return errors.New("max_file_bytes must be positive and no larger than max_bytes")
	}
	if c.LikelyFiles < 1 || c.LikelyFiles > 1000 {
		return errors.New("likely_files must be between 1 and 1000")
	}
	if c.Calibration <= 0 || c.Calibration > 100 {
		return errors.New("calibration must be greater than 0 and at most 100")
	}
	if len(c.Profiles) == 0 {
		return errors.New("at least one profile is required")
	}
	for name, p := range c.Profiles {
		for _, pair := range [][2]float64{p.UncachedInput, p.CachedInput, p.Output} {
			if pair[0] < 0 || pair[1] < pair[0] {
				return fmt.Errorf("profile %q has an invalid range", name)
			}
		}
	}
	if c.SubscriptionTokens != nil && *c.SubscriptionTokens < 0 {
		return errors.New("subscription_tokens must not be negative")
	}
	for name, price := range map[string]*float64{"uncached_input": c.Prices.UncachedInputPerMillion, "cached_input": c.Prices.CachedInputPerMillion, "output": c.Prices.OutputPerMillion} {
		if price != nil && (*price < 0 || math.IsNaN(*price) || math.IsInf(*price, 0)) {
			return fmt.Errorf("%s price must be a finite non-negative number", name)
		}
	}
	return nil
}
func LoadConfig(path string) (Config, error) {
	c := DefaultConfig()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("decode config: %w", err)
	}
	return c, c.Validate()
}
func SaveConfig(path string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

type FileEvidence struct {
	Path   string `json:"path"`
	Tokens int64  `json:"tokens"`
	Score  int    `json:"score"`
}
type Context struct {
	Approximation    string         `json:"approximation"`
	Tokens           int64          `json:"tokens"`
	IssueTokens      int64          `json:"issue_tokens"`
	LikelyFiles      []FileEvidence `json:"likely_files"`
	LikelyFileTokens int64          `json:"likely_file_tokens"`
}
type RunTokens struct {
	UncachedInput            Range `json:"uncached_input"`
	CachedInput              Range `json:"cached_input"`
	OutputIncludingReasoning Range `json:"output_including_reasoning"`
	Total                    Range `json:"total"`
}
type CostRange struct {
	Low  float64 `json:"low"`
	High float64 `json:"high"`
}
type Costs struct {
	Currency      string                `json:"currency"`
	APIEquivalent map[string]*CostRange `json:"api_equivalent"`
	ActualCash    *CostRange            `json:"actual_cash"`
	Subscription  struct {
		CapacityTokens *int64     `json:"capacity_tokens"`
		Label          string     `json:"label"`
		EstimatedShare *CostRange `json:"estimated_share"`
	} `json:"subscription_quota"`
}
type ChangeSize struct {
	Files Range `json:"files"`
	Lines Range `json:"lines"`
}
type Estimate struct {
	SchemaVersion   string     `json:"schema_version"`
	Product         string     `json:"product"`
	Mode            string     `json:"mode"`
	Repository      string     `json:"repository"`
	Issue           int        `json:"issue,omitempty"`
	Model           string     `json:"model,omitempty"`
	Profile         string     `json:"profile"`
	SelectedContext Context    `json:"selected_context"`
	WholeRun        RunTokens  `json:"whole_run"`
	Costs           Costs      `json:"costs"`
	LikelyInspect   []string   `json:"likely_files_to_inspect"`
	LikelyModify    []string   `json:"likely_files_to_modify"`
	ChangeSize      ChangeSize `json:"change_size"`
	Confidence      string     `json:"confidence"`
	Evidence        []string   `json:"evidence"`
	Assumptions     []string   `json:"assumptions"`
	MissingInfo     []string   `json:"missing_info"`
	Warnings        []string   `json:"warnings"`
	ScannedFiles    int        `json:"scanned_files"`
	ScannedBytes    int64      `json:"scanned_bytes"`
	Truncated       bool       `json:"truncated"`
	SkippedBinary   int        `json:"skipped_binary"`
	SkippedLarge    int        `json:"skipped_large_files"`
}
type candidate struct {
	FileEvidence
}

func EstimatePath(path string, issue []byte, repo string, issueNumber int, model, profileName string, c Config) (Estimate, error) {
	if err := c.Validate(); err != nil {
		return Estimate{}, err
	}
	profile, ok := c.Profiles[profileName]
	if !ok {
		return Estimate{}, fmt.Errorf("unknown profile %q", profileName)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return Estimate{}, err
	}
	terms := searchTerms(string(issue))
	e := Estimate{SchemaVersion: SchemaVersion, Product: "Glance", Repository: repo, Issue: issueNumber, Model: model, Profile: profileName, Confidence: "low", Assumptions: []string{"offline character approximation; repository content is untrusted and was not executed"}, MissingInfo: []string{"tool output, conversation history, generated context, and final response size are unknown"}}
	var candidates []candidate
	err = filepath.WalkDir(abs, func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(abs, name)
		if err != nil {
			return err
		}
		if name != abs && d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if name != abs && d.IsDir() && excludedDir(d.Name()) {
			return filepath.SkipDir
		}
		if d.IsDir() || d.Type()&os.ModeSymlink != 0 || sensitivePath(rel) {
			return nil
		}
		if e.ScannedFiles >= c.MaxFiles || e.ScannedBytes >= c.MaxBytes {
			e.Truncated = true
			return fs.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if info.Size() > c.MaxFileBytes {
			e.SkippedLarge++
			return nil
		}
		if info.Size() > c.MaxBytes-e.ScannedBytes {
			e.Truncated = true
			return fs.SkipAll
		}
		b, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		if bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b) {
			e.SkippedBinary++
			return nil
		}
		e.ScannedFiles++
		e.ScannedBytes += int64(len(b))
		candidates = append(candidates, candidate{FileEvidence: FileEvidence{Path: filepath.ToSlash(rel), Tokens: Tokens(b, c.CharsPerToken), Score: scoreFile(filepath.ToSlash(rel), b, terms)}})
		return nil
	})
	if err != nil {
		return Estimate{}, err
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Score == candidates[j].Score {
			return candidates[i].Path < candidates[j].Path
		}
		return candidates[i].Score > candidates[j].Score
	})
	for _, file := range candidates {
		if len(e.SelectedContext.LikelyFiles) >= c.LikelyFiles || (file.Score == 0 && len(e.SelectedContext.LikelyFiles) > 0) {
			break
		}
		e.SelectedContext.LikelyFiles = append(e.SelectedContext.LikelyFiles, file.FileEvidence)
		e.SelectedContext.LikelyFileTokens += file.Tokens
		e.LikelyInspect = append(e.LikelyInspect, file.Path)
	}
	for i := 0; i < len(e.LikelyInspect) && i < 5; i++ {
		e.LikelyModify = append(e.LikelyModify, e.LikelyInspect[i])
	}
	e.SelectedContext.IssueTokens = Tokens(issue, c.CharsPerToken)
	e.SelectedContext.Tokens = e.SelectedContext.IssueTokens + e.SelectedContext.LikelyFileTokens
	e.SelectedContext.Approximation = fmt.Sprintf("ceil(UTF-8 bytes / %.2f chars_per_token)", c.CharsPerToken)
	base := float64(e.SelectedContext.Tokens) * c.Calibration
	e.WholeRun.UncachedInput = scaledRange(base, profile.UncachedInput)
	e.WholeRun.CachedInput = scaledRange(base, profile.CachedInput)
	e.WholeRun.OutputIncludingReasoning = scaledRange(base, profile.Output)
	e.WholeRun.Total = Range{Low: e.WholeRun.UncachedInput.Low + e.WholeRun.CachedInput.Low + e.WholeRun.OutputIncludingReasoning.Low, High: e.WholeRun.UncachedInput.High + e.WholeRun.CachedInput.High + e.WholeRun.OutputIncludingReasoning.High}
	e.ChangeSize = ChangeSize{Files: Range{Low: 1, High: int64(max(1, min(5, len(e.LikelyModify))))}, Lines: Range{Low: 5, High: int64(max(20, min(500, len(issue)*2)))}}
	e.Costs = CalculateCosts(e.WholeRun, c.Prices, c.SubscriptionTokens, c.SubscriptionLabel)
	if c.Calibration == 1 {
		e.Mode = "offline_heuristic_uncalibrated"
		e.Warnings = append(e.Warnings, "uncalibrated estimate")
	} else {
		e.Mode = "offline_heuristic_calibrated"
	}
	if e.Truncated {
		e.Warnings = append(e.Warnings, "repository scan stopped at a configured safety limit")
	}
	if len(terms) > 0 && len(e.SelectedContext.LikelyFiles) > 0 {
		e.Confidence = "medium"
		e.Evidence = append(e.Evidence, fmt.Sprintf("%d bounded filename/text matches selected", len(e.SelectedContext.LikelyFiles)))
	}
	return e, nil
}
func Tokens(b []byte, charsPerToken float64) int64 {
	if len(b) == 0 {
		return 0
	}
	return int64(math.Ceil(float64(len(b)) / charsPerToken))
}
func scaledRange(base float64, factor [2]float64) Range {
	return Range{int64(math.Ceil(base * factor[0])), int64(math.Ceil(base * factor[1]))}
}
func CalculateCosts(tokens RunTokens, prices Prices, capacity *int64, label string) Costs {
	c := Costs{Currency: prices.Currency, ActualCash: nil, APIEquivalent: map[string]*CostRange{"uncached_input": nil, "cached_input": nil, "output_including_reasoning": nil}}
	c.Subscription.CapacityTokens, c.Subscription.Label = capacity, label
	put := func(name string, r Range, price *float64) {
		if price != nil {
			c.APIEquivalent[name] = &CostRange{float64(r.Low) * *price / 1e6, float64(r.High) * *price / 1e6}
		}
	}
	put("uncached_input", tokens.UncachedInput, prices.UncachedInputPerMillion)
	put("cached_input", tokens.CachedInput, prices.CachedInputPerMillion)
	put("output_including_reasoning", tokens.OutputIncludingReasoning, prices.OutputPerMillion)
	if capacity != nil && *capacity > 0 {
		c.Subscription.EstimatedShare = &CostRange{float64(tokens.Total.Low) / float64(*capacity), float64(tokens.Total.High) / float64(*capacity)}
	}
	return c
}
func excludedDir(name string) bool {
	switch strings.ToLower(name) {
	case ".git", ".hg", ".svn", "vendor", "node_modules", "dist", "build", "coverage", "target", ".next":
		return true
	}
	return false
}
func sensitivePath(path string) bool {
	p := strings.ToLower(strings.ReplaceAll(path, "\\", "/"))
	base := filepath.Base(p)
	if base == ".env" || strings.HasPrefix(base, ".env.") || base == "credentials" || base == "credentials.json" || base == "id_rsa" || base == "id_ed25519" {
		return true
	}
	switch strings.ToLower(filepath.Ext(base)) {
	case ".pem", ".key", ".p12", ".pfx", ".keystore":
		return true
	}
	return false
}
func searchTerms(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, word := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' }) {
		if len(word) >= 4 && !seen[word] {
			seen[word] = true
			out = append(out, word)
		}
	}
	if len(out) > 50 {
		out = out[:50]
	}
	return out
}
func scoreFile(path string, b []byte, terms []string) int {
	p, body, score := strings.ToLower(path), strings.ToLower(string(b)), 0
	for _, term := range terms {
		if strings.Contains(p, term) {
			score += 5
		}
		if strings.Contains(body, term) {
			score++
		}
	}
	return score
}

type ActualTokens struct {
	UncachedInput            int64 `json:"uncached_input"`
	CachedInput              int64 `json:"cached_input"`
	OutputIncludingReasoning int64 `json:"output_including_reasoning"`
}
type Predicted struct {
	Low  int64 `json:"low"`
	High int64 `json:"high"`
}
type Record struct {
	SchemaVersion   string       `json:"schema_version"`
	ID              string       `json:"id"`
	Timestamp       time.Time    `json:"timestamp"`
	Repo            string       `json:"repo"`
	Category        string       `json:"category"`
	Profile         string       `json:"profile"`
	Config          string       `json:"config"`
	Version         string       `json:"version"`
	Predicted       Predicted    `json:"predicted"`
	Actual          ActualTokens `json:"actual"`
	Spend           *float64     `json:"spend"`
	DurationSeconds *float64     `json:"duration_seconds"`
	VerifiedOutcome string       `json:"verified_outcome"`
	CensoredStatus  string       `json:"censored_status"`
	Provenance      string       `json:"provenance"`
	Warnings        []string     `json:"warnings"`
	HeldOut         bool         `json:"held_out"`
}

func (r Record) ActualTotal() int64 {
	return r.Actual.UncachedInput + r.Actual.CachedInput + r.Actual.OutputIncludingReasoning
}
func AppendRecord(path string, r Record) error {
	if r.ID == "" || r.Predicted.Low < 0 || r.Predicted.High < r.Predicted.Low {
		return errors.New("record requires id and a valid predicted range")
	}
	if r.Actual.UncachedInput < 0 || r.Actual.CachedInput < 0 || r.Actual.OutputIncludingReasoning < 0 {
		return errors.New("actual token values must not be negative")
	}
	if r.SchemaVersion == "" {
		r.SchemaVersion = SchemaVersion
	}
	if r.Timestamp.IsZero() {
		r.Timestamp = time.Now().UTC()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(r)
}

type Metrics struct {
	Observations             int      `json:"observations"`
	Eligible                 int      `json:"eligible"`
	Failed                   int      `json:"failed"`
	BudgetStopped            int      `json:"budget_stopped"`
	Coverage                 *float64 `json:"coverage"`
	MedianCentralError       *float64 `json:"median_central_estimate_error"`
	UnderestimationFrequency *float64 `json:"underestimation_frequency"`
	HistoricalMedianBaseline *float64 `json:"historical_median_baseline"`
	AverageRangeWidth        *float64 `json:"average_range_width"`
}
type CalibrationReport struct {
	SchemaVersion string   `json:"schema_version"`
	Tuning        Metrics  `json:"tuning"`
	HeldOut       Metrics  `json:"held_out"`
	Factor        *float64 `json:"factor"`
}

func Calibrate(r io.Reader) (CalibrationReport, error) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), 1<<20)
	var tuning, held []Record
	for line := 1; s.Scan(); line++ {
		var rec Record
		if err := json.Unmarshal(s.Bytes(), &rec); err != nil {
			return CalibrationReport{}, fmt.Errorf("record %d: %w", line, err)
		}
		if rec.HeldOut {
			held = append(held, rec)
		} else {
			tuning = append(tuning, rec)
		}
	}
	if err := s.Err(); err != nil {
		return CalibrationReport{}, err
	}
	report := CalibrationReport{SchemaVersion: SchemaVersion, Tuning: metrics(tuning), HeldOut: metrics(held)}
	var predicted, actual int64
	for _, rec := range tuning {
		if eligible(rec) {
			predicted += (rec.Predicted.Low + rec.Predicted.High) / 2
			actual += rec.ActualTotal()
		}
	}
	if predicted > 0 {
		factor := float64(actual) / float64(predicted)
		report.Factor = &factor
	}
	return report, nil
}
func eligible(r Record) bool {
	return r.ActualTotal() > 0 && r.CensoredStatus != "budget-stopped" && r.CensoredStatus != "failed"
}
func metrics(records []Record) Metrics {
	m := Metrics{Observations: len(records)}
	var errors, baselines, widths []float64
	covered, under := 0, 0
	for _, r := range records {
		if r.CensoredStatus == "failed" {
			m.Failed++
		}
		if r.CensoredStatus == "budget-stopped" {
			m.BudgetStopped++
		}
		if !eligible(r) {
			continue
		}
		m.Eligible++
		actual, center := float64(r.ActualTotal()), float64(r.Predicted.Low+r.Predicted.High)/2
		errors = append(errors, math.Abs(center-actual)/actual)
		baselines = append(baselines, actual)
		widths = append(widths, float64(r.Predicted.High-r.Predicted.Low))
		if actual >= float64(r.Predicted.Low) && actual <= float64(r.Predicted.High) {
			covered++
		}
		if actual > float64(r.Predicted.High) {
			under++
		}
	}
	if m.Eligible > 0 {
		coverage, underFreq, width := float64(covered)/float64(m.Eligible), float64(under)/float64(m.Eligible), mean(widths)
		m.Coverage, m.UnderestimationFrequency, m.AverageRangeWidth = &coverage, &underFreq, &width
		medianErr, baseline := median(errors), median(baselines)
		m.MedianCentralError, m.HistoricalMedianBaseline = &medianErr, &baseline
	}
	return m
}
func mean(v []float64) float64 {
	var n float64
	for _, x := range v {
		n += x
	}
	return n / float64(len(v))
}
func median(v []float64) float64 {
	sort.Float64s(v)
	n := len(v)
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}
func ParseConfigSet(c *Config, key, value string) error {
	switch key {
	case "chars_per_token":
		if _, err := fmt.Sscan(value, &c.CharsPerToken); err != nil {
			return err
		}
	case "max_files":
		if _, err := fmt.Sscan(value, &c.MaxFiles); err != nil {
			return err
		}
	case "max_bytes":
		if _, err := fmt.Sscan(value, &c.MaxBytes); err != nil {
			return err
		}
	case "max_file_bytes":
		if _, err := fmt.Sscan(value, &c.MaxFileBytes); err != nil {
			return err
		}
	case "likely_files":
		if _, err := fmt.Sscan(value, &c.LikelyFiles); err != nil {
			return err
		}
	case "subscription_tokens":
		var n int64
		if value == "unknown" {
			c.SubscriptionTokens = nil
		} else if _, err := fmt.Sscan(value, &n); err == nil {
			c.SubscriptionTokens = &n
		} else {
			return fmt.Errorf("invalid value %q", value)
		}
	default:
		return fmt.Errorf("unknown setting %q", key)
	}
	return c.Validate()
}
