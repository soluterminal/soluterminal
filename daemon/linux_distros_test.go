package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
)

func TestLinuxDistrosRoute(t *testing.T) {
	get := func(a *apiServer) (out struct {
		Enabled bool          `json:"enabled"`
		Distros []linuxDistro `json:"distros"`
		Error   string        `json:"error"`
	}) {
		rec := httptest.NewRecorder()
		a.linuxDistros(rec, httptest.NewRequest("GET", "/api/linux/distros", nil))
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("body %q: %v", rec.Body.String(), err)
		}
		return out
	}
	old := listWSLDistros
	defer func() { listWSLDistros = old }()
	listWSLDistros = func(context.Context) ([]linuxDistro, error) {
		return []linuxDistro{{Name: "Ubuntu", Default: true}, {Name: "Debian"}}, nil
	}
	// Off: no list, whatever is installed.
	off := get(&apiServer{mgr: NewManager(t.TempDir())})
	if off.Enabled || len(off.Distros) != 0 {
		t.Fatalf("Linux panes off: %+v", off)
	}
	m, _ := newLinuxPaneManager(t)
	on := get(&apiServer{mgr: m})
	if !on.Enabled || len(on.Distros) != 2 || !on.Distros[0].Default {
		t.Fatalf("Linux panes on: %+v", on)
	}
	listWSLDistros = func(context.Context) ([]linuxDistro, error) { return nil, errors.New("wsl.exe failed") }
	if bad := get(&apiServer{mgr: m}); !bad.Enabled || bad.Error == "" || bad.Distros == nil {
		t.Fatalf("a failed listing: %+v", bad)
	}
}
