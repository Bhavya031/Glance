package glance

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

	"github.com/tiktoken-go/tokenizer"
)

const (
	SchemaVersion       = "2"
	DefaultEncoding     = "o200k_base"
	DefaultMaxFiles     = 10_000
	DefaultMaxBytes     = int64(32 << 20)
	DefaultMaxFileBytes = int64(1 << 20)
	DefaultLikelyFiles  = 20
	PlanningLabel       = "user-selected planning assumption, not official/verified"
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
		MaxFiles: DefaultMaxFiles, MaxBytes: DefaultMaxBytes,
		MaxFileBytes: DefaultMaxFileBytes, LikelyFiles: DefaultLikelyFiles, Calibration: 1,
		Profiles: map[string]Profile{"standard": {UncachedInput: [2]float64{1, 3}, CachedInput: [2]float64{0, 1}, Output: [2]float64{0.15, 0.75}}},
		Prices:   Prices{Currency: "USD"}, SubscriptionTokens: &capacity, SubscriptionLabel: PlanningLabel,
	}
}
func (c Config) Validate() error {
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
	Path  string `json:"path"`
	Score int    `json:"score"`
}
type Context struct {
	Encoding      string         `json:"encoding"`
	ContextTokens int            `json:"context_tokens"`
	LikelyFiles   []FileEvidence `json:"likely_files"`
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
type EstimateRange struct {
	Low     int64 `json:"low"`
	Central int64 `json:"central"`
	High    int64 `json:"high"`
}
type ExcludedFile struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}
type Estimate struct {
	SchemaVersion    string         `json:"schema_version"`
	Product          string         `json:"product"`
	Mode             string         `json:"mode"`
	Repository       string         `json:"repository"`
	Issue            int            `json:"issue,omitempty"`
	Model            string         `json:"model,omitempty"`
	Profile          string         `json:"profile"`
	Encoding         string         `json:"encoding"`
	ContextTokens    int            `json:"context_tokens"`
	SelectedFiles    []FileEvidence `json:"selected_files"`
	ExcludedFiles    []ExcludedFile `json:"excluded_files"`
	EstimatedInput   EstimateRange  `json:"estimated_input_tokens"`
	EstimatedOutput  EstimateRange  `json:"estimated_output_tokens"`
	EstimationMethod string         `json:"estimation_method"`
	SelectedContext  Context        `json:"selected_context"`
	WholeRun         RunTokens      `json:"whole_run"`
	Costs            Costs          `json:"costs"`
	LikelyInspect    []string       `json:"likely_files_to_inspect"`
	LikelyModify     []string       `json:"likely_files_to_modify"`
	ChangeSize       ChangeSize     `json:"change_size"`
	Confidence       string         `json:"confidence"`
	Evidence         []string       `json:"evidence"`
	Assumptions      []string       `json:"assumptions"`
	MissingInfo      []string       `json:"missing_info"`
	Warnings         []string       `json:"warnings"`
	ScannedFiles     int            `json:"scanned_files"`
	ScannedBytes     int64          `json:"scanned_bytes"`
	Truncated        bool           `json:"truncated"`
	SkippedBinary    int            `json:"skipped_binary"`
	SkippedLarge     int            `json:"skipped_large_files"`
}
type candidate struct {
	FileEvidence
	content []byte
}

func EstimatePath(path string, issue []byte, repo string, issueNumber int, model, profileName, encoding string, c Config) (Estimate, error) {
	if err := c.Validate(); err != nil {
		return Estimate{}, err
	}
	profile, ok := c.Profiles[profileName]
	if !ok {
		return Estimate{}, fmt.Errorf("unknown profile %q", profileName)
	}
	if _, err := CountTokens("", encoding); err != nil {
		return Estimate{}, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return Estimate{}, err
	}
	terms := searchTerms(string(issue))
	e := Estimate{
		SchemaVersion: SchemaVersion, Product: "Glance", Repository: repo, Issue: issueNumber, Model: model, Profile: profileName,
		Encoding: encoding, Confidence: "low", EstimationMethod: "profile ranges applied to measured assembled-context tokens",
		SelectedFiles: make([]FileEvidence, 0), ExcludedFiles: make([]ExcludedFile, 0),
		Assumptions: []string{
			"context_tokens measures the assembled instructions, selected file paths, separators, and file contents with the named encoding",
			"whole-run input and output ranges are heuristic and are not exact for any model",
			"repository content is untrusted and was not executed",
		},
		MissingInfo: []string{"tool output, conversation history, generated context, reasoning behavior, and final response size are unknown"},
	}
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
		if d.IsDir() {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.Type()&os.ModeSymlink != 0 {
			e.ExcludedFiles = append(e.ExcludedFiles, ExcludedFile{Path: rel, Reason: "symlink"})
			return nil
		}
		if sensitivePath(rel) {
			e.ExcludedFiles = append(e.ExcludedFiles, ExcludedFile{Path: rel, Reason: "sensitive filename"})
			return nil
		}
		if e.ScannedFiles >= c.MaxFiles || e.ScannedBytes >= c.MaxBytes {
			e.Truncated = true
			e.ExcludedFiles = append(e.ExcludedFiles, ExcludedFile{Path: rel, Reason: "scan file or byte limit reached"})
			return fs.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			e.ExcludedFiles = append(e.ExcludedFiles, ExcludedFile{Path: rel, Reason: "non-regular file"})
			return nil
		}
		if info.Size() > c.MaxFileBytes {
			e.SkippedLarge++
			e.ExcludedFiles = append(e.ExcludedFiles, ExcludedFile{Path: rel, Reason: "exceeds max_file_bytes"})
			return nil
		}
		if info.Size() > c.MaxBytes-e.ScannedBytes {
			e.Truncated = true
			e.ExcludedFiles = append(e.ExcludedFiles, ExcludedFile{Path: rel, Reason: "scan byte limit reached"})
			return fs.SkipAll
		}
		b, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		if bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b) {
			e.SkippedBinary++
			e.ExcludedFiles = append(e.ExcludedFiles, ExcludedFile{Path: rel, Reason: "binary or non-UTF-8"})
			return nil
		}
		e.ScannedFiles++
		e.ScannedBytes += int64(len(b))
		candidates = append(candidates, candidate{FileEvidence: FileEvidence{Path: rel, Score: scoreFile(rel, b, terms)}, content: b})
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
	selected := make([]candidate, 0, c.LikelyFiles)
	contextBytes := len(assembleContext(issue, nil))
	if int64(contextBytes) > c.MaxBytes {
		return Estimate{}, errors.New("instructions exceed max_bytes assembled-context limit")
	}
	for _, file := range candidates {
		if len(selected) >= c.LikelyFiles || (file.Score == 0 && len(selected) > 0) {
			break
		}
		addition := len(assembledFile(file))
		if int64(contextBytes+addition) > c.MaxBytes {
			e.ExcludedFiles = append(e.ExcludedFiles, ExcludedFile{Path: file.Path, Reason: "exceeds max_bytes assembled-context limit"})
			continue
		}
		selected = append(selected, file)
		contextBytes += addition
		e.SelectedFiles = append(e.SelectedFiles, file.FileEvidence)
		e.LikelyInspect = append(e.LikelyInspect, file.Path)
	}
	e.SelectedContext = Context{Encoding: encoding, LikelyFiles: e.SelectedFiles}
	for i := 0; i < len(e.LikelyInspect) && i < 5; i++ {
		e.LikelyModify = append(e.LikelyModify, e.LikelyInspect[i])
	}
	contextTokens, err := CountTokens(string(assembleContext(issue, selected)), encoding)
	if err != nil {
		return Estimate{}, fmt.Errorf("count assembled context: %w", err)
	}
	e.ContextTokens = contextTokens
	e.SelectedContext.ContextTokens = contextTokens
	base := float64(contextTokens) * c.Calibration
	e.WholeRun.UncachedInput = scaledRange(base, profile.UncachedInput)
	e.WholeRun.CachedInput = scaledRange(base, profile.CachedInput)
	e.WholeRun.OutputIncludingReasoning = scaledRange(base, profile.Output)
	e.WholeRun.Total = Range{Low: e.WholeRun.UncachedInput.Low + e.WholeRun.CachedInput.Low + e.WholeRun.OutputIncludingReasoning.Low, High: e.WholeRun.UncachedInput.High + e.WholeRun.CachedInput.High + e.WholeRun.OutputIncludingReasoning.High}
	e.EstimatedInput = centralRange(Range{Low: e.WholeRun.UncachedInput.Low + e.WholeRun.CachedInput.Low, High: e.WholeRun.UncachedInput.High + e.WholeRun.CachedInput.High})
	e.EstimatedOutput = centralRange(e.WholeRun.OutputIncludingReasoning)
	e.ChangeSize = ChangeSize{Files: Range{Low: 1, High: int64(max(1, min(5, len(e.LikelyModify))))}, Lines: Range{Low: 5, High: int64(max(20, min(500, len(issue)*2)))}}
	e.Costs = CalculateCosts(e.WholeRun, c.Prices, c.SubscriptionTokens, c.SubscriptionLabel)
	if c.Calibration == 1 {
		e.Mode = "offline_tokenizer_uncalibrated"
		e.Warnings = append(e.Warnings, "uncalibrated estimate")
	} else {
		e.Mode = "offline_tokenizer_calibrated"
	}
	if e.Truncated {
		e.Warnings = append(e.Warnings, "repository scan stopped at a configured safety limit")
	}
	if len(terms) > 0 && len(e.SelectedContext.LikelyFiles) > 0 {
		e.Evidence = append(e.Evidence, fmt.Sprintf("%d bounded filename/text matches selected", len(e.SelectedContext.LikelyFiles)))
	}
	return e, nil
}
func CountTokens(text, encoding string) (int, error) {
	var name tokenizer.Encoding
	switch encoding {
	case "o200k_base":
		name = tokenizer.O200kBase
	case "cl100k_base":
		name = tokenizer.Cl100kBase
	default:
		return 0, fmt.Errorf("unsupported encoding %q; supported encodings are o200k_base and cl100k_base", encoding)
	}
	codec, err := tokenizer.Get(name)
	if err != nil {
		return 0, fmt.Errorf("initialize %s tokenizer: %w", encoding, err)
	}
	ids, _, err := codec.Encode(text)
	if err != nil {
		return 0, fmt.Errorf("encode with %s: %w", encoding, err)
	}
	return len(ids), nil
}
func assembleContext(instructions []byte, files []candidate) []byte {
	var context bytes.Buffer
	context.WriteString("Instructions:\n")
	context.Write(instructions)
	context.WriteString("\n\n")
	for _, file := range files {
		context.Write(assembledFile(file))
	}
	return context.Bytes()
}
func assembledFile(file candidate) []byte {
	var section bytes.Buffer
	section.WriteString("File: ")
	section.WriteString(file.Path)
	section.WriteString("\n---\n")
	section.Write(file.content)
	section.WriteString("\n---\n\n")
	return section.Bytes()
}
func centralRange(r Range) EstimateRange {
	return EstimateRange{Low: r.Low, Central: r.Low + (r.High-r.Low)/2, High: r.High}
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
