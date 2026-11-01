package providers

var muse = Provider{
	Slug:        "muse",
	Vendor:      "meta",
	DisplayName: "Muse",
	DocsURL:     "https://dev.meta.ai/docs/muse-code",
	Runtimes: map[string]RuntimeFacts{
		"headless": {
			Billing: "subscription",
			// Muse resolves through the login-shell fallback for parity with
			// Claude and Codex, so an nvm/asdf-managed install is found even when
			// the daemon's PATH omits it. It still declares no home glob.
			Binary: &BinarySpec{
				Name:               "muse",
				HomeSubdirs:        []string{".local/bin", ".asdf/shims", ".volta/bin"},
				LoginShellFallback: true,
				NotFoundMsg:        "muse not found (PATH, ~/.local/bin, asdf, volta, login shell)",
				NoHomeMsg:          "muse not on PATH and no home dir",
			},
		},
	},
	// Needles audited against muse 0.2.1 on 2026-08-19, including the
	// wft_99ab4fcc billing incident signature, and the subscription-window
	// exhaustion observed on 2026-09-05.
	Errors: ErrorSignatures{
		Quota: []string{"subscription quota exhausted. your usage window resets at"},
		Auth:  []string{"failed to authenticate", "not logged in"},
		Billing: []string{
			"billing_error", "billing verification failed", "check your payment method",
			"payment method on your account", "payment required to finish setting up",
		},
	},
	Models: ModelCatalog{Selectable: []string{"muse-spark-1.3-contributor", "muse-spark-1.2-contributor"}, Rows: []ModelRow{
		// Meta's catalog lists both contributor versions (2026-09-22).
		// Keep 1.2 selectable for saved lineups and frozen task routes; listing
		// 1.3 does not silently migrate an owner's opted-in model pool.
		{Match: "spark", Tier: TierStandard},
	}},
	Creds: &CredFacts{
		Store: &StoreFacts{UsesAuthLockFile: true, LiveFileName: "auth.json", RootDirName: "muse", ManagedFileName: "managed.json", SourcesDirName: "sources", TeamDirName: "team"},
	},
}
