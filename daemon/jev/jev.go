// Package jev turns a SoluTerminal task into a TypeSafe System One ("Jev")
// evaluation and returns a tier-classification verdict.
//
// It exists because tier selection is a classic System One judgment: pick one
// of four ordered-ish lanes and decide one yes/no risk question, with a
// calibrated confidence you can gate on. Jev returns exactly that shape, so
// the daemon no longer has to parse JSON out of generated prose.
//
// This package is deliberately pure: it owns request construction, confidence
// gating, and response interpretation, but no engine state, no filesystem, and
// no policy about which tasks may use it. The daemon supplies the state and
// decides what to do with a low-confidence result.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"soluterminal-shared/typesafe"
)

// Environment variables. The API key itself is TYPESAFE_API_KEY, read by
// typesafe.NewFromEnv; these gate and tune the daemon's use of it.
const (
	EnvEnabled          = "SOLU_JEV_CLASSIFIER"
	EnvModel            = "SOLU_JEV_MODEL"
	EnvMinConfidence    = "SOLU_JEV_MIN_CONFIDENCE"
	EnvFileThreshold    = "SOLU_JEV_FILE_THRESHOLD"
	EnvTimeout          = "SOLU_JEV_TIMEOUT"
	EnvMaxStateBytes    = "SOLU_JEV_MAX_STATE_BYTES"
	EnvMaxCandidates    = "SOLU_JEV_MAX_FILE_CANDIDATES"
	defaultMinConf      = 0.45
	defaultFileThresh   = 0.50
	defaultTimeout      = 10 * time.Second
	defaultMaxStateSize = 120_000
	defaultCandidates   = 50
)

// ErrDisabled means the operator has not opted in.
var ErrDisabled = errors.New("jev: classifier is disabled (set " + EnvEnabled + "=1)")

// ErrLowConfidence means Jev returned a tier below the configured confidence
// floor. Callers should fall back to the existing classifier rather than act.
var ErrLowConfidence = errors.New("jev: tier confidence below threshold")

// ErrStateTooLarge means the assembled state would exceed the configured cap.
// Callers fall back; sending it would risk a 422 or a context-truncated answer.
var ErrStateTooLarge = errors.New("jev: state exceeds configured size cap")

// Config is the operator-facing knob set. The zero value is disabled.
type Config struct {
	Enabled           bool
	Model             string
	MinTierConfidence float64
	FileThreshold     float64
	Timeout           time.Duration
	MaxStateBytes     int
	MaxCandidates     int
}

// ConfigFromEnv reads the SOLU_JEV_* environment. It never fails; a malformed
// numeric override is ignored in favour of the default.
func ConfigFromEnv() Config {
	cfg := Config{
		Enabled:           parseBool(os.Getenv(EnvEnabled)),
		Model:             strings.TrimSpace(os.Getenv(EnvModel)),
		MinTierConfidence: parseFloat(os.Getenv(EnvMinConfidence), defaultMinConf),
		FileThreshold:     parseFloat(os.Getenv(EnvFileThreshold), defaultFileThresh),
		Timeout:           parseDuration(os.Getenv(EnvTimeout), defaultTimeout),
		MaxStateBytes:     parseInt(os.Getenv(EnvMaxStateBytes), defaultMaxStateSize),
		MaxCandidates:     parseInt(os.Getenv(EnvMaxCandidates), defaultCandidates),
	}
	return cfg.withDefaults()
}

func (c Config) withDefaults() Config {
	if c.Model == "" {
		c.Model = typesafe.DefaultModel
	}
	if c.MinTierConfidence <= 0 || c.MinTierConfidence > 1 {
		c.MinTierConfidence = defaultMinConf
	}
	if c.FileThreshold <= 0 || c.FileThreshold > 1 {
		c.FileThreshold = defaultFileThresh
	}
	if c.Timeout <= 0 {
		c.Timeout = defaultTimeout
	}
	if c.MaxStateBytes <= 0 {
		c.MaxStateBytes = defaultMaxStateSize
	}
	if c.MaxCandidates <= 0 {
		c.MaxCandidates = defaultCandidates
	}
	return c
}

// Input is the material the daemon already has for a classification.
type Input struct {
	// Prompt is the original task request.
	Prompt string
	// Deliverable is the requested artifact kind (change/plan/report).
	Deliverable string
	// RiskAdvisory names a risk keyword the prompt mentioned, if any.
	RiskAdvisory string
	// Survey is the shallow repository context (top-level entries + README head).
	Survey string
}

// Verdict is JSON-compatible with the daemon's TierClassification artifact.
type Verdict struct {
	Tier               string   `json:"tier"`
	Reasoning          string   `json:"reasoning"`
	FilesLikelyTouched []string `json:"filesLikelyTouched"`
	RiskBehaviorChange bool     `json:"riskBehaviorChange"`
}

// Meta is telemetry the daemon can record without depending on Jev internals.
type Meta struct {
	Model               string             `json:"model"`
	TierConfidence      float64            `json:"tierConfidence"`
	TierProbabilities   map[string]float64 `json:"tierProbabilities,omitempty"`
	RunnerUp            string             `json:"runnerUp,omitempty"`
	RunnerUpProbability float64            `json:"runnerUpProbability,omitempty"`
	RiskProbability     float64            `json:"riskProbability"`
	CandidateScores     map[string]float64 `json:"candidateScores,omitempty"`
	QuestionCount       int                `json:"questionCount"`
	Usage               typesafe.Usage     `json:"usage"`
	EstimatedCostUSD    float64            `json:"estimatedCostUsd"`
}

// Classifier evaluates Inputs with one Jev client and one Config.
type Classifier struct {
	Client *typesafe.Client
	Config Config
}

// New returns a classifier. A nil client yields ErrDisabled from calls.
func New(client *typesafe.Client, cfg Config) *Classifier {
	return &Classifier{Client: client, Config: cfg.withDefaults()}
}

// ClassifyTier sends one System One request and interprets the answers.
func (c *Classifier) ClassifyTier(ctx context.Context, in Input) (Verdict, Meta, error) {
	if c == nil || c.Client == nil || !c.Config.Enabled {
		return Verdict{}, Meta{}, ErrDisabled
	}
	state, candidates, err := c.buildState(in)
	if err != nil {
		return Verdict{}, Meta{}, err
	}
	questions := buildQuestions(candidates)

	callCtx := ctx
	if c.Config.Timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, c.Config.Timeout)
		defer cancel()
	}
	resp, err := c.Client.SystemOneModel(callCtx, c.Config.Model, state, questions)
	if err != nil {
		return Verdict{}, Meta{}, err
	}
	return interpret(resp, candidates, c.Config)
}

// buildState assembles the state object and enforces the size cap before any
// network call. It returns the top-level candidate paths parsed from Survey.
func (c *Classifier) buildState(in Input) (map[string]any, []string, error) {
	state := map[string]any{}
	if strings.TrimSpace(in.Prompt) != "" {
		state["request"] = in.Prompt
	}
	if strings.TrimSpace(in.Deliverable) != "" {
		state["deliverable"] = in.Deliverable
	}
	if strings.TrimSpace(in.RiskAdvisory) != "" {
		state["risk_advisory"] = in.RiskAdvisory
	}
	if strings.TrimSpace(in.Survey) != "" {
		state["repo_survey"] = in.Survey
	}
	if len(state) == 0 {
		return nil, nil, errors.New("jev: empty classification input")
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return nil, nil, err
	}
	if len(encoded) > c.Config.MaxStateBytes {
		return nil, nil, fmt.Errorf("%w: %d > %d bytes", ErrStateTooLarge, len(encoded), c.Config.MaxStateBytes)
	}
	candidates := ParseSurveyCandidates(in.Survey)
	if len(candidates) == 0 {
		// The workflow-runtime path sends the fully composed worker prompt,
		// which embeds the survey's "Top-level entries:" line.
		candidates = ParseSurveyCandidates(in.Prompt)
	}
	if len(candidates) > c.Config.MaxCandidates {
		candidates = candidates[:c.Config.MaxCandidates]
	}
	return state, candidates, nil
}

func buildQuestions(candidates []string) map[string]typesafe.Question {
	questions := map[string]typesafe.Question{
		"tier": tierQuestion(),
		"risk": riskQuestion(),
	}
	for i, path := range candidates {
		questions["path_"+strconv.Itoa(i)] = pathQuestion(path)
	}
	return questions
}

// tierQuestion reproduces the tier-classifier rubric from the daemon's own
// tier-classifier skill, expressed as a Choice so the answer is a typed option
// plus a calibrated confidence rather than a string we have to parse.
func tierQuestion() typesafe.Question {
	return typesafe.Choice(
		map[string]any{
			"question": "Which execution tier should this task be routed to?",
			"focus": "Classify the requested change, not incidental words in identifiers, " +
				"reference paths, background context, display copy, or read-only observability. " +
				"Choose the lowest tier that is safe: touching several files does not by itself " +
				"make a task complex. Choose complex only when the work genuinely spans multiple " +
				"subsystems or needs root-cause investigation before implementation, and critical " +
				"only for security-sensitive or hard-to-reverse changes.",
		},
		map[string]any{
			"small": "The fast lane: single-file or few-file changes with a clear spec. " +
				"Copy edits, config tweaks, small CSS fixes, tightly scoped bug fixes. " +
				"Concrete and narrow. No spec or review stage.",
			"standard": "The default for ordinary feature work and uncertain causes that need " +
				"lightweight evidence gathering: a bounded request-driven scout, spec, and review. " +
				"Use this for work confined to one area even if it touches several files in it.",
			"complex": "Work that genuinely spans multiple subsystems, or that needs root-cause " +
				"investigation across them before implementation: adds a research-agenda plan and " +
				"a full agenda-driven scout.",
			"critical": "The complex plan reserved for changes to security-sensitive behavior " +
				"(auth, credential or secret storage, token handling, payments, data migrations) " +
				"or anything hard to reverse. A plan/report that only discusses a future sensitive " +
				"change is tiered by the complexity of producing that artifact, not floored merely " +
				"for mentioning the domain.",
		})
}

func riskQuestion() typesafe.Question {
	return typesafe.NoulWithCriteria(
		"Does the requested implementation change authentication, credential or secret storage, "+
			"token handling, payments, migrations, or another security-sensitive flow? "+
			"Incidental mentions and plan/report-only discussion are not a behavior change.",
		"Implementing a change to a security-sensitive or hard-to-reverse flow",
		"No security-sensitive behavior is implemented by this task",
	)
}

func pathQuestion(path string) typesafe.Question {
	return typesafe.NoulWithCriteria(
		"Is the top-level path `"+path+"` likely to be modified by this task?",
		"The requested change very likely edits at least one file under this path",
		"The requested change is unlikely to edit anything under this path",
	)
}

// TriageActions is the fixed recommendation vocabulary the triage skill owns.
// Exported so the daemon and the shadow report agree on the exact set.
var TriageActions = []string{
	"resume", "retry", "reland", "rescue-merge", "refile", "archive-superseded", "human",
}

// TriageVerdict is a shadow recommendation for one stuck task. It is never
// applied by this package: the caller compares it to the incumbent's decision
// under the daemon's own bounded, evidence-gated policy.
type TriageVerdict struct {
	Action        string             `json:"action"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Model         string             `json:"model,omitempty"`
}

// ClassifyTriage asks Jev which of the fixed triage actions is safest for one
// incident packet. It is deliberately shadow-shaped: it returns a typed
// recommendation and does not act, because a cheap classifier must never be the
// sole authority that closes stuck work.
func (c *Classifier) ClassifyTriage(ctx context.Context, packet string) (TriageVerdict, error) {
	if c == nil || c.Client == nil || !c.Config.Enabled {
		return TriageVerdict{}, ErrDisabled
	}
	packet = strings.TrimSpace(packet)
	if packet == "" {
		return TriageVerdict{}, errors.New("jev: empty triage packet")
	}
	if c.Config.MaxStateBytes > 0 && len(packet) > c.Config.MaxStateBytes {
		return TriageVerdict{}, ErrStateTooLarge
	}
	callCtx := ctx
	if c.Config.Timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, c.Config.Timeout)
		defer cancel()
	}
	resp, err := c.Client.SystemOneModel(callCtx, c.Config.Model,
		map[string]any{"incident": packet},
		map[string]typesafe.Question{"action": triageQuestion()})
	if err != nil {
		return TriageVerdict{}, err
	}
	if resp == nil {
		return TriageVerdict{}, errors.New("jev: nil response")
	}
	ans, ok := resp.Answers["action"]
	if !ok || ans.Type != "choice" || strings.TrimSpace(ans.Choice) == "" {
		return TriageVerdict{}, errors.New("jev: missing or malformed triage action answer")
	}
	action := strings.TrimSpace(ans.Choice)
	if !validTriageAction(action) {
		return TriageVerdict{}, fmt.Errorf("jev: unknown triage action %q", action)
	}
	conf, _ := ans.ConfidenceValue()
	return TriageVerdict{
		Action: action, Confidence: conf, Probabilities: ans.Probabilities, Model: resp.Model,
	}, nil
}

func validTriageAction(action string) bool {
	for _, a := range TriageActions {
		if a == action {
			return true
		}
	}
	return false
}

// triageQuestion reproduces the triage skill's action vocabulary as a Choice,
// so the answer is a typed option with a calibrated confidence rather than
// prose. The option descriptions mirror the retired workflow triage skill.
func triageQuestion() typesafe.Question {
	return typesafe.Choice(
		map[string]any{
			"question": "Which single triage action is safest for this stuck task?",
			"focus": "Decide only from the incident packet. Prefer the least invasive action " +
				"that can actually resolve this blocker, and choose human whenever the " +
				"evidence is insufficient, the failure is structural, or the safe next step " +
				"needs a decision a person must own. Never assume the retained branch holds " +
				"the implementation when the packet says otherwise.",
		},
		map[string]any{
			"resume":             "Continue the same attempt; a bounded, engine-controlled resume is safe.",
			"retry":              "File one linked replacement after a spawn/setup fatality; the work itself was never judged wrong.",
			"reland":             "The approved guarded land transaction should simply re-run.",
			"rescue-merge":       "A trial merge is conflict-free, or the conflicts have a clear per-file resolution.",
			"refile":             "The task must be re-implemented afresh against current main with a replacement prompt.",
			"archive-superseded": "A live replacement already exists; archive this duplicate record.",
			"human":              "The next step needs a person, or the evidence is insufficient to choose safely.",
		})
}

func interpret(resp *typesafe.Response, candidates []string, cfg Config) (Verdict, Meta, error) {
	if resp == nil {
		return Verdict{}, Meta{}, errors.New("jev: nil response")
	}
	meta := Meta{
		Model:           resp.Model,
		Usage:           resp.Usage,
		QuestionCount:   len(candidates) + 2,
		CandidateScores: map[string]float64{},
	}
	meta.EstimatedCostUSD = resp.Usage.EstimatedCostUSD()

	tierAns, ok := resp.Answers["tier"]
	if !ok || tierAns.Type != "choice" || strings.TrimSpace(tierAns.Choice) == "" {
		return Verdict{}, meta, errors.New("jev: missing or malformed tier answer")
	}
	if !validTier(tierAns.Choice) {
		return Verdict{}, meta, fmt.Errorf("jev: unknown tier %q", tierAns.Choice)
	}
	conf, ok := tierAns.ConfidenceValue()
	if !ok {
		return Verdict{}, meta, errors.New("jev: tier answer has no confidence")
	}
	meta.TierConfidence = conf
	meta.TierProbabilities = tierAns.Probabilities
	meta.RunnerUp, meta.RunnerUpProbability = runnerUp(tierAns, tierAns.Choice)

	riskAns, ok := resp.Answers["risk"]
	riskP, nok := riskAns.NoulValue()
	if !ok || !nok {
		return Verdict{}, meta, errors.New("jev: missing or malformed risk answer")
	}
	meta.RiskProbability = riskP
	risk := riskP >= 0.5

	var files []string
	type scored struct {
		path string
		p    float64
	}
	var above []scored
	for i, path := range candidates {
		ans, ok := resp.Answers["path_"+strconv.Itoa(i)]
		if !ok {
			continue
		}
		p, ok := ans.NoulValue()
		if !ok {
			continue
		}
		meta.CandidateScores[path] = p
		if p >= cfg.FileThreshold {
			above = append(above, scored{path: path, p: p})
		}
	}
	sort.SliceStable(above, func(i, j int) bool { return above[i].p > above[j].p })
	for _, s := range above {
		files = append(files, s.path)
	}

	v := Verdict{
		Tier:               tierAns.Choice,
		Reasoning:          reasoning(resp.Model, tierAns.Choice, conf, meta, risk, len(files), cfg.FileThreshold),
		FilesLikelyTouched: files,
		RiskBehaviorChange: risk,
	}
	if conf < cfg.MinTierConfidence {
		// The verdict is still returned so a caller can apply a conservative
		// policy (never route below standard on an uncertain answer) instead of
		// discarding a usable probability distribution.
		return v, meta, fmt.Errorf("%w: %.2f < %.2f", ErrLowConfidence, conf, cfg.MinTierConfidence)
	}
	return v, meta, nil
}

func reasoning(model, tier string, conf float64, meta Meta, risk bool, files int, threshold float64) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s: tier=%s", model, tier)
	if p, ok := meta.TierProbabilities[tier]; ok {
		fmt.Fprintf(&b, " (p=%.2f, confidence=%.2f)", p, conf)
	} else {
		fmt.Fprintf(&b, " (confidence=%.2f)", conf)
	}
	if meta.RunnerUp != "" && meta.RunnerUpProbability > 0.15 {
		fmt.Fprintf(&b, "; runner-up %s (p=%.2f)", meta.RunnerUp, meta.RunnerUpProbability)
	}
	yes := "no"
	if risk {
		yes = "yes"
	}
	fmt.Fprintf(&b, "; riskBehaviorChange=%s (p=%.2f)", yes, meta.RiskProbability)
	fmt.Fprintf(&b, "; %d path(s) >= %.2f", files, threshold)
	return b.String()
}

func runnerUp(a typesafe.Answer, chosen string) (string, float64) {
	opt, p := "", 0.0
	for k, v := range a.Probabilities {
		if k == chosen {
			continue
		}
		if v > p {
			opt, p = k, v
		}
	}
	return opt, p
}

func validTier(t string) bool {
	switch t {
	case "small", "standard", "complex", "critical":
		return true
	}
	return false
}

// ParseSurveyCandidates extracts the top-level entry names from the daemon's
// shallow repo survey. The survey is a single "Top-level entries: a, b/, c/"
// line possibly followed by a README head. Trailing slashes are stripped so
// the paths match the names the engine uses elsewhere.
func ParseSurveyCandidates(survey string) []string {
	var line string
	for _, l := range strings.Split(survey, "\n") {
		if strings.HasPrefix(l, "Top-level entries:") {
			line = strings.TrimSpace(strings.TrimPrefix(l, "Top-level entries:"))
			break
		}
	}
	if line == "" {
		return nil
	}
	// Drop the "…(+N more)" tail.
	if i := strings.Index(line, "…"); i >= 0 {
		line = line[:i]
	}
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(line, ",") {
		p := strings.TrimSpace(part)
		p = strings.TrimSuffix(p, "/")
		p = strings.TrimSpace(p)
		if p == "" || p == "…" || strings.HasPrefix(p, "(+") {
			continue
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func parseBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func parseFloat(v string, def float64) float64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func parseInt(v string, def int) int {
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func parseDuration(v string, def time.Duration) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
