package providers

// GLM's native ZCode harness has its own credential family. It is not a
// workflow runtime and must not inherit Claude's subscription identity.
var glm = Provider{
	Slug: "glm", Vendor: "zai", DisplayName: "GLM", DocsURL: "https://zcode.z.ai/en/docs/configuration",
	Creds: apiHarnessCreds("glm"),
	Harness: &HarnessFacts{
		Billings: []string{"coding-plan", "api"},
		Endpoints: map[string]string{
			"coding-plan": "https://api.z.ai/api/coding/paas/v4",
			"api":         "https://api.z.ai/api/paas/v4",
		},
		DefaultModel:  "glm-5.3",
		InstallDir:    "zcode-terminal",
		RuntimePath:   []string{"node_modules", "zcode-app-cli", "vendor", "zcode.cjs"},
		RuntimeGuard:  []string{"node_modules", "@zcode", "tui", "package.json"},
		ImportKeyFile: "zai-env-file",
	},
}

func apiHarnessCreds(slug string) *CredFacts {
	return &CredFacts{
		Store: &StoreFacts{RootDirName: slug, LiveFileName: "api-key.json", ManagedFileName: "managed.json", SourcesDirName: "sources", TeamDirName: "team"},
	}
}
