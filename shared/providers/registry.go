package providers

import (
	"fmt"
	"sort"
	"strings"
)

var registry = []Provider{anthropic, claude, codex, deepseek, gemini, glm, grok, localOpenAI, muse, openai, openrouter, pi, piGlm, typesafe}

// All returns a copy of the provider registry ordered by slug.
func All() []Provider {
	out := append([]Provider(nil), registry...)
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

func Get(slug string) (Provider, bool) {
	for _, p := range registry {
		if p.Slug == slug {
			return p, true
		}
	}
	return Provider{}, false
}

func ByVendor(vendor string) (Provider, bool) {
	vendor = strings.TrimSpace(vendor)
	for _, p := range registry {
		if strings.EqualFold(p.Vendor, vendor) {
			return p, true
		}
	}
	return Provider{}, false
}

// ByRuntimeID splits at the last dash so provider slugs may themselves contain
// dashes without changing the runtime-id contract.
func ByRuntimeID(id string) (p Provider, family string, facts RuntimeFacts, ok bool) {
	i := strings.LastIndex(id, "-")
	if i <= 0 || i == len(id)-1 {
		return Provider{}, "", RuntimeFacts{}, false
	}
	p, ok = Get(id[:i])
	if !ok {
		return Provider{}, "", RuntimeFacts{}, false
	}
	family = id[i+1:]
	facts, ok = p.Runtimes[family]
	if !ok {
		return Provider{}, "", RuntimeFacts{}, false
	}
	return p, family, facts, true
}

// IsHarness reports whether slug names a managed native harness provider.
func IsHarness(slug string) bool {
	p, ok := Get(slug)
	return ok && p.Harness != nil
}

// HarnessSlugs returns the slugs of managed native harness providers, ordered
// by slug for deterministic iteration.
func HarnessSlugs() []string {
	var out []string
	for _, p := range All() {
		if p.Harness != nil {
			out = append(out, p.Slug)
		}
	}
	return out
}

// Validate checks the shipped registry without imposing product policy.
func Validate() error {
	return validateProviders(registry)
}

// validateProviders is split from Validate so package tests can exercise fake
// descriptors without registering a production provider.
func validateProviders(entries []Provider) error {
	seen := make(map[string]struct{}, len(registry))
	routes := make(map[string]string)
	tables := make(map[string]string)
	validTiers := map[string]bool{
		TierFast: true, TierStandard: true, TierPremium: true, TierFrontier: true,
	}
	for _, p := range entries {
		if p.Slug == "" {
			return fmt.Errorf("provider slug is empty")
		}
		if _, exists := seen[p.Slug]; exists {
			return fmt.Errorf("duplicate provider slug %q", p.Slug)
		}
		seen[p.Slug] = struct{}{}
		if p.KeyAPI != nil {
			if (p.KeyAPI.Protocol != "anthropic" && p.KeyAPI.Protocol != "openai") ||
				strings.TrimSpace(p.KeyAPI.Env) == "" || !strings.HasPrefix(p.KeyAPI.BaseURL, "https://") {
				return fmt.Errorf("provider %q has incomplete API key facts", p.Slug)
			}
		}
		if p.Vendor == "" {
			return fmt.Errorf("provider %q vendor is empty", p.Slug)
		}
		for family, facts := range p.Runtimes {
			switch facts.Billing {
			case "subscription", "api", "unknown":
			default:
				return fmt.Errorf("provider %q runtime %q has invalid billing %q", p.Slug, family, facts.Billing)
			}
			if facts.Binary != nil && (facts.Binary.Name == "" || facts.Binary.NotFoundMsg == "") {
				return fmt.Errorf("provider %q runtime %q has incomplete binary facts", p.Slug, family)
			}
		}
		for _, set := range []struct {
			name    string
			needles []string
		}{
			{name: "quota", needles: p.Errors.Quota},
			{name: "auth", needles: p.Errors.Auth},
			{name: "billing", needles: p.Errors.Billing},
			{name: "infra", needles: p.Errors.Infra},
		} {
			for _, needle := range set.needles {
				if strings.TrimSpace(needle) == "" {
					return fmt.Errorf("provider %q error signature set %q has empty needle %q", p.Slug, set.name, needle)
				}
				if needle != strings.ToLower(needle) {
					return fmt.Errorf("provider %q error signature set %q has non-lowercase needle %q", p.Slug, set.name, needle)
				}
			}
		}
		for _, row := range p.Models.Rows {
			if !validTiers[row.Tier] {
				return fmt.Errorf("provider %q model %q has invalid tier %q", p.Slug, row.Match, row.Tier)
			}
		}
		selectable := make(map[string]bool, len(p.Models.Selectable))
		for _, model := range p.Models.Selectable {
			model = strings.TrimSpace(model)
			if model == "" {
				return fmt.Errorf("provider %q has an empty selectable model id", p.Slug)
			}
			if selectable[model] {
				return fmt.Errorf("provider %q repeats selectable model %q", p.Slug, model)
			}
			selectable[model] = true
			if _, known := p.Models.Tier(model); !known {
				return fmt.Errorf("provider %q selectable model %q has no tier matcher", p.Slug, model)
			}
		}
		for model, levels := range p.Models.EffortByModel {
			if !selectable[model] {
				return fmt.Errorf("provider %q model effort metadata names non-selectable model %q", p.Slug, model)
			}
			seenLevels := make(map[string]bool, len(levels))
			for _, level := range levels {
				trimmed := strings.TrimSpace(level)
				if trimmed == "" {
					return fmt.Errorf("provider %q model %q has blank effort level %q", p.Slug, model, level)
				}
				if level != trimmed || level != strings.ToLower(level) {
					return fmt.Errorf("provider %q model %q has non-lowercase effort level %q", p.Slug, model, level)
				}
				if seenLevels[level] {
					return fmt.Errorf("provider %q model %q repeats effort level %q", p.Slug, model, level)
				}
				seenLevels[level] = true
			}
		}
		if p.Creds != nil && p.Creds.Mailbox != nil {
			m := p.Creds.Mailbox
			if m.RouteSegment == "" || m.SetTable == "" || m.SyncTable == "" || m.PolicyTable == "" || m.EnvelopeKey == "" {
				return fmt.Errorf("provider %q credential mailbox facts are incomplete", p.Slug)
			}
			if m.Validation != "oauth-object" && m.Validation != "auth-kind" && m.Validation != "envelope-only" {
				return fmt.Errorf("provider %q has invalid credential validation %q", p.Slug, m.Validation)
			}
			if m.PolicyKind != "rules" && m.PolicyKind != "policy" {
				return fmt.Errorf("provider %q has invalid credential policy kind %q", p.Slug, m.PolicyKind)
			}
			if m.MaxBodyBytes <= 0 {
				return fmt.Errorf("provider %q credential max body bytes must be positive", p.Slug)
			}
			if m.Validation == "envelope-only" && m.MaxEnvelopeBytes <= 0 {
				return fmt.Errorf("provider %q envelope-only credentials require positive max envelope bytes", p.Slug)
			}
			if m.ReservedAccountLabel != "" && !m.RestrictAccountLabels {
				return fmt.Errorf("provider %q reserved credential account label requires restricted labels", p.Slug)
			}
			if other, ok := routes[m.RouteSegment]; ok {
				return fmt.Errorf("providers %q and %q share credential route segment %q", other, p.Slug, m.RouteSegment)
			}
			routes[m.RouteSegment] = p.Slug
			for _, table := range []string{m.SetTable, m.SyncTable, m.PolicyTable} {
				if other, ok := tables[table]; ok {
					return fmt.Errorf("providers %q and %q share credential table %q", other, p.Slug, table)
				}
				tables[table] = p.Slug
			}
			if m.ActiveTable != "" {
				if other, ok := tables[m.ActiveTable]; ok {
					return fmt.Errorf("providers %q and %q share credential table %q", other, p.Slug, m.ActiveTable)
				}
				tables[m.ActiveTable] = p.Slug
			}
		}
		if p.Creds != nil && p.Creds.TeamState != nil {
			if p.Creds.TeamState.StateFileName == "" || p.Creds.Store == nil {
				return fmt.Errorf("provider %q credential daemon facts are incomplete", p.Slug)
			}
		}
		if h := p.Harness; h != nil {
			if len(h.Billings) == 0 {
				return fmt.Errorf("provider %q harness has no billing modes", p.Slug)
			}
			seenBillings := make(map[string]bool, len(h.Billings))
			for _, billing := range h.Billings {
				if strings.TrimSpace(billing) == "" {
					return fmt.Errorf("provider %q harness has an empty billing mode", p.Slug)
				}
				if seenBillings[billing] {
					return fmt.Errorf("provider %q harness repeats billing mode %q", p.Slug, billing)
				}
				seenBillings[billing] = true
				if strings.TrimSpace(h.Endpoints[billing]) == "" {
					return fmt.Errorf("provider %q harness billing %q has no endpoint", p.Slug, billing)
				}
			}
			for billing := range h.Endpoints {
				if !seenBillings[billing] {
					return fmt.Errorf("provider %q harness endpoint names unlisted billing %q", p.Slug, billing)
				}
			}
			if strings.TrimSpace(h.InstallDir) == "" || len(h.RuntimePath) == 0 {
				return fmt.Errorf("provider %q harness install facts are incomplete", p.Slug)
			}
			for _, part := range h.RuntimePath {
				if strings.TrimSpace(part) == "" {
					return fmt.Errorf("provider %q harness runtime path has an empty segment", p.Slug)
				}
			}
			if h.ImportKeyFile != "" && h.ImportKeyEnv != "" {
				return fmt.Errorf("provider %q harness import key cannot be both a file and a secret", p.Slug)
			}
		}
	}
	return nil
}
