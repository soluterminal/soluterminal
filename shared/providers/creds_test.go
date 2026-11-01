package providers

import "testing"

func TestCredentialFactsAllowAbsentExtensions(t *testing.T) {
	for _, p := range All() {
		if p.Creds == nil {
			continue
		}
		c := *p.Creds
		c.Mailbox, c.TeamState = nil, nil
		p.Creds = &c
		if err := validateProviders([]Provider{p}); err != nil {
			t.Fatalf("%s without optional descriptors: %v", p.Slug, err)
		}
	}
}

func TestCredentialStateRejectsIncompleteExtension(t *testing.T) {
	p, _ := Get("claude")
	p.Creds = &CredFacts{Store: &StoreFacts{}, TeamState: &TeamStateFacts{}}
	if err := validateProviders([]Provider{p}); err == nil {
		t.Fatal("empty registered state filename accepted")
	}
}
