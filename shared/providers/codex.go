package providers

var codex = Provider{
	Slug:        "codex",
	Vendor:      "openai",
	DisplayName: "Codex",
	DocsURL:     "https://github.com/openai/codex#readme",
	Runtimes: map[string]RuntimeFacts{
		"headless": {
			Billing: "subscription",
			Binary: &BinarySpec{
				Name:        "codex",
				HomeSubdirs: []string{".local/bin", ".asdf/shims", ".volta/bin"},
				HomeGlobs:   []string{".nvm/versions/node/*/bin", ".asdf/installs/nodejs/*/bin"},
				// OpenAI's install.ps1 default; install.sh uses ~/.local/bin.
				LocalAppDataSubdirs: []string{"Programs/OpenAI/Codex/bin"},
				LoginShellFallback:  true,
				NotFoundMsg:         "codex not found (PATH, ~/.local/bin, nvm, asdf, volta, login shell)",
			},
		},
	},
	// Needles audited against codex 0.147.0 on 2026-08-19. The OpenAI API's
	// billing prose is printed through from its error body by the CLI.
	Errors: ErrorSignatures{
		Quota: []string{"hit your usage limit", "usage limit reached"},
		Auth:  []string{"not logged in", "token has expired"},
		Billing: []string{
			"out of credits", "credits_depleted", "check your plan and billing",
			"insufficient_quota", "spend cap set in your workspace",
		},
	},
	// gpt-6.1-sol checked 2026-10-05 against the official model page and
	// Codex models_cache.json. This catalog is not proof of account access.
	// Model facts checked 2026-09-05 against the official OpenAI model pages
	// (developers.openai.com/api/docs/models/{gpt-6-astra,gpt-5.6-sol,
	// gpt-5.6-terra,gpt-5.6-luna}) and Codex 0.153.4's locally fetched
	// models_cache.json; gpt-6-sol ("workhorse model for coding") and
	// gpt-6-luna ("fast and affordable") added 2026-09-26 from Codex
	// 0.157.1's models_cache.json. The cache is session-visible evidence only
	// and is never read by soluterminal at runtime or treated as account
	// entitlement.
	Models: ModelCatalog{Selectable: []string{
		"gpt-6.1-sol", "gpt-6-astra", "gpt-6-sol", "gpt-6-luna",
		"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna",
		"gpt-5.6-codex", "gpt-5.5", "gpt-5.4",
	}, Rows: []ModelRow{
		// Full family slugs must precede generic GPT classifiers so suffixed
		// snapshots and aliases retain their explicit capability tier.
		{Match: "gpt-6.1-sol", Tier: TierPremium},
		{Match: "gpt-6-astra", Tier: TierFrontier},
		{Match: "gpt-6-sol", Tier: TierPremium},
		{Match: "gpt-6-luna", Tier: TierFast},
		{Match: "gpt-5.6-sol", Tier: TierPremium},
		{Match: "gpt-5.6-terra", Tier: TierStandard},
		{Match: "gpt-5.6-luna", Tier: TierFast},
		{Match: "nano", Tier: TierFast},
		{Match: "mini", Tier: TierFast},
		// pro intentionally precedes gpt so gpt-5-pro is premium.
		{Match: "pro", Tier: TierPremium},
		{Match: "gpt", Tier: TierStandard},
		{Match: "codex", Tier: TierStandard},
		{Match: "o3", Tier: TierStandard},
		{Match: "o4", Tier: TierStandard},
	}, EffortByModel: map[string][]string{
		"gpt-6.1-sol":   {"low", "medium", "high", "xhigh", "max"},
		"gpt-6-astra":   {"low", "medium", "high", "xhigh", "max"},
		"gpt-6-sol":     {"low", "medium", "high", "xhigh", "max"},
		"gpt-6-luna":    {"low", "medium", "high", "xhigh", "max"},
		"gpt-5.6-sol":   {"low", "medium", "high", "xhigh", "max"},
		"gpt-5.6-terra": {"low", "medium", "high", "xhigh", "max"},
		"gpt-5.6-luna":  {"low", "medium", "high", "xhigh", "max"},
		"gpt-5.6-codex": {"low", "medium", "high", "xhigh"},
		"gpt-5.5":       {"low", "medium", "high", "xhigh"},
		"gpt-5.4":       {"low", "medium", "high", "xhigh"},
	}},
	Creds: &CredFacts{
		Store: &StoreFacts{LiveFileName: "auth.json", RootDirName: "codex", ManagedFileName: "managed.json", SourcesDirName: "sources", TeamDirName: "team"},
	},
}
