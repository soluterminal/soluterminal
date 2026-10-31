package main

import "soluterminal-shared/daemonfeatures"

// daemonFeatures is what THIS build supports, by name (shared/daemonfeatures).
// It rides the relay handshake (`f`) and /api/health (`features`) so the cloud
// and web can gate on capability instead of comparing versions. Add a name
// here in the same change that adds the behaviour; never list something a
// build cannot actually do.
var daemonFeatures = []string{
	daemonfeatures.UIPrefs,
	daemonfeatures.TeamBrokerCodex,
	daemonfeatures.TeamBrokerClaude,
	daemonfeatures.WorkflowOwnerDirect,
}
