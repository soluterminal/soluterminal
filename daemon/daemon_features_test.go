package main

import (
	"encoding/json"

	"net/http/httptest"
	"slices"
	"soluterminal-shared/daemonfeatures"
	"testing"
	"time"
)

func TestHealthAdvertisesRegisteredFeatures(t *testing.T) {
	a := &apiServer{mgr: NewManager(""), startT: time.Now(), ver: "dev"}
	response := httptest.NewRecorder()
	a.health(response, httptest.NewRequest("GET", "/api/health", nil))
	var health struct {
		Features []string `json:"features"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(health.Features, daemonFeatures) {
		t.Fatalf("health features %v differ from registrations %v", health.Features, daemonFeatures)
	}
}

func TestDaemonFeatureRegistration(t *testing.T) {
	saved := daemonFeatures
	t.Cleanup(func() { daemonFeatures = saved })
	daemonFeatures = []string{}
	registerDaemonFeature("example.v1")
	registerDaemonFeature("example.v1")
	if !slices.Equal(daemonFeatures, []string{"example.v1"}) {
		t.Fatalf("registration = %v", daemonFeatures)
	}
}

// ST-315: the UI shows pane recording only when the daemon says it serves it.
func TestHealthAdvertisesPaneRecording(t *testing.T) {
	if !slices.Contains(daemonFeatures, daemonfeatures.PaneRecording) {
		t.Fatalf("daemonFeatures %v lacks %s", daemonFeatures, daemonfeatures.PaneRecording)
	}
}
