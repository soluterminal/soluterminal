package main

import "net/http"

// Diagnostic handlers run on every transport, including those which do not
// pass through authGate. A loopback address alone grants no diagnostic access.
func (a *apiServer) diagnosticAccess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requestViaRelay(r) || requestHasForwardingMetadata(r) {
			http.Error(w, "diagnostics require a local connection", http.StatusForbidden)
			return
		}
		if !browserRequestOriginMatches(r) {
			rejectCrossOrigin(w, r)
			return
		}
		if !a.gate.hasOwnerCredential(r) && !a.requireHuman(w, r, "read diagnostics") {
			return
		}
		next.ServeHTTP(w, r)
	})
}
