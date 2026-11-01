package providers

var claude = Provider{
	Slug:        "claude",
	Vendor:      "anthropic",
	DisplayName: "Claude Code",
	DocsURL:     "https://docs.claude.com/en/docs/claude-code/setup",
	Runtimes: map[string]RuntimeFacts{
		"headless": {
			Billing: "subscription",
			Binary: &BinarySpec{
				Name:               "claude",
				HomeSubdirs:        []string{".local/bin", ".asdf/shims", ".volta/bin"},
				HomeGlobs:          []string{".nvm/versions/node/*/bin", ".asdf/installs/nodejs/*/bin"},
				LoginShellFallback: true,
				NotFoundMsg:        "claude not found (PATH, ~/.local/bin, nvm, asdf, volta, login shell)",
			},
		},
	},
	// Needles audited against claude 2.1.235 on 2026-08-19. The quota/auth
	// lines also retain the observed-live baseline from workflow triage.
	Errors: ErrorSignatures{
		Quota: []string{
			"hit your weekly limit", "hit your usage limit", "usage limit reached",
			"out of extended usage", "hit your session limit", "session limit reached",
			"hit your limit", "5-hour limit",
		},
		Auth: []string{
			"oauth session expired", "failed to authenticate", "please run /login",
			"authentication_error", "not logged in", "token has expired", "oauth token revoked",
		},
		Billing: []string{
			"credit balance is too low", "credit balance too low", "requires usage credits",
			"usage credits exhausted", "purchase credits", "add a payment method",
			// The Anthropic API type is deliberately duplicated with muse.
			"billing_error",
		},
	},
	Models: ModelCatalog{Selectable: []string{"haiku", "sonnet", "opus", "fable"}, Rows: []ModelRow{
		{Match: "haiku", Tier: TierFast},
		{Match: "sonnet", Tier: TierStandard},
		{Match: "opus", Tier: TierPremium},
		{Match: "fable", Tier: TierFrontier},
	}},
	Creds: &CredFacts{
		Store: &StoreFacts{LiveFileName: ".credentials.json"},
	},
}
