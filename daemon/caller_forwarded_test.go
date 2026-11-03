package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// forwardedHeaders is a request another local program forwarded for a remote
// device: the /m/ marker, with the remote browser's fetch headers passed on.
func forwardedHeaders(r *http.Request) *http.Request {
	r.Header.Set("X-Forwarded-Prefix", "/m/machine1")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Origin", "http://127.0.0.1:8787")
	return r
}

// An untraced request carrying the /m/ marker is never the person on browser
// headers, in any mode; an approved device's signature still is; without the
// marker nothing changes; and the relay tunnel is unaffected.
func TestForwardedRequestIsNotThePersonOnHeaders(t *testing.T) {
	for _, mode := range []string{humanProofOff, humanProofReport, humanProofRequire} {
		t.Run(mode, func(t *testing.T) {
			a := humanProofTestServer(t, mode)
			stubPeerPID(t, 0, false)

			r, _ := throughProof(a, forwardedHeaders(httptest.NewRequest("POST", "/api/settings/agent-authority", nil)))
			if id := a.identifyCaller(r); id.Kind != callerUnverified {
				t.Fatalf("marker + browser headers = %+v", id)
			}
			w := httptest.NewRecorder()
			if a.requireHuman(w, r, "change agent authority") || w.Code != http.StatusForbidden {
				t.Fatalf("requireHuman with the marker = %d", w.Code)
			}

			// The same through a proxy the daemon traced (WSL's init, the
			// desktop router) rather than no process at all.
			prev := peerIsRelayProxy
			peerIsRelayProxy = func(int) bool { return true }
			stubPeerPID(t, 4242, true)
			r, _ = throughProof(a, forwardedHeaders(httptest.NewRequest("POST", "/api/settings/agent-authority", nil)))
			if id := a.identifyCaller(r); id.Kind != callerUnverified {
				t.Fatalf("marker through a proxy = %+v", id)
			}
			peerIsRelayProxy = prev
			stubPeerPID(t, 0, false)

			// A valid device signature still makes it the person.
			phone := newTestDevice(t)
			approveTestDevice(t, a, phone)
			signed := forwardedHeaders(phone.localRequest(t, a, "POST", "/api/settings/agent-authority", "", time.Now(), "sig-"+mode))
			signed, _ = throughProof(a, signed)
			if id := a.identifyCaller(signed); id.Kind != callerHuman || id.Device != deviceID(phone.spki) {
				t.Fatalf("signed with the marker = %+v", id)
			}

			// Without the marker: today's result.
			plain := httptest.NewRequest("POST", "/api/settings/agent-authority", nil)
			plain.Header.Set("Sec-Fetch-Site", "same-origin")
			plain, _ = throughProof(a, plain)
			id := a.identifyCaller(plain)
			switch mode {
			case humanProofRequire:
				if id.Kind != callerUnverified || !id.Unproven {
					t.Fatalf("no marker under require = %+v", id)
				}
			default:
				if id.Kind != callerHuman || !id.Unproven {
					t.Fatalf("no marker under %s = %+v", mode, id)
				}
			}

			// The relay tunnel is decided before any of this.
			relayed := forwardedHeaders(httptest.NewRequest("POST", "/api/settings/agent-authority", nil))
			relayed = relayed.WithContext(context.WithValue(relayed.Context(), relayTransportKey{}, true))
			if id := a.identifyCaller(relayed); id.Kind != callerHuman || !id.Relay {
				t.Fatalf("relay tunnel = %+v", id)
			}
		})
	}
}
