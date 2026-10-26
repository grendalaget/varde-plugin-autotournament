package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/grendalaget/varde-plugin-autotournament/internal/state"
	"github.com/grendalaget/varde-plugin-autotournament/internal/varde"
)

func TestServerPublicAddressUsesMatchingUDPPortOrFallback(t *testing.T) {
	tests := []struct {
		name    string
		address string
		ports   []varde.GamePort
		want    string
	}{
		{
			name:    "matching UDP port",
			address: "127.77.0.5:27020",
			ports: []varde.GamePort{
				{Port: 27015, Protocol: "udp"},
				{Port: 27020, Protocol: "UDP"},
			},
			want: "127.77.0.5:27020",
		},
		{
			name:    "address port is TCP",
			address: "127.77.0.5:25575",
			ports: []varde.GamePort{
				{Port: 25575, Protocol: "tcp"},
				{Port: 27015, Protocol: "udp"},
			},
			want: "127.77.0.5:27015",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := varde.Server{
				Service: &varde.Service{Ports: test.ports},
			}
			server.Summary.Address = &test.address
			if got := serverPublicAddress(server); got != test.want {
				t.Fatalf("serverPublicAddress() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestReconcileReadyUpAddonSpecAndSkipUnchanged(t *testing.T) {
	var mu sync.Mutex
	putCount := 0
	var received varde.AddonSpec
	cpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/groups/group-1/servers":
			_, _ = w.Write([]byte(`{"servers":[{"id":"server-cs2","game_id":"cs2"},{"id":"server-other","game_id":"minecraft"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/servers/server-cs2":
			_, _ = w.Write([]byte(`{
				"id":"server-cs2",
				"group_id":"group-1",
				"game_id":"cs2",
				"service":{"loopback_ip":"127.77.0.5","ports":[
					{"port":27015,"protocol":"udp"},
					{"port":27016,"protocol":"tcp"}
				]},
				"summary":{"address":"127.77.0.5:27015"}
			}`))
		case r.Method == http.MethodPut && r.URL.Path == "/v1/servers/server-cs2/addons/autotournament/readyup":
			var spec varde.AddonSpec
			if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
				t.Errorf("decode addon spec: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			mu.Lock()
			putCount++
			received = spec
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpServer.Close()

	pin := &ReadyUpPin{
		URL:     "https://releases.example.test/readyup.zip",
		SHA256:  strings.Repeat("a", 64),
		License: "noncommercial",
	}
	service, store, _ := newTestService(t, cpServer.URL, pin)
	if err := store.Save("group-1", state.Group{
		ATURL:    "https://at.example.test",
		ATToken:  "at-token",
		FleetKey: "fleet-key",
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := service.ReconcileGroup(context.Background(), "group-1"); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if putCount != 1 {
		t.Fatalf("addon PUT count = %d, want 1", putCount)
	}
	if received.URL != pin.URL || received.SHA256 != pin.SHA256 ||
		received.Archive != "zip" || received.Target != "csgo" {
		t.Fatalf("addon pin = %#v", received)
	}
	if !reflectSearchPath(received.DriverOptions["gameinfo_search_paths"]) {
		t.Fatalf("driver options = %#v", received.DriverOptions)
	}
	if received.Files["cfg/ReadyUp/fleet.cfg"] !=
		"url = https://at.example.test\nenroll_key = fleet-key\npublic_addr = 127.77.0.5:27015\nenabled = 1\n" {
		t.Fatalf("fleet config = %q", received.Files["cfg/ReadyUp/fleet.cfg"])
	}
	license := received.Files["cfg/ReadyUp/license.cfg"]
	if !strings.HasPrefix(license, "readyup_license_accepted \"noncommercial\"\nreadyup_license_accepted_at \"") ||
		!strings.HasSuffix(license, "\"\n") {
		t.Fatalf("license config = %q", license)
	}
	group, ok := store.Get("group-1")
	if !ok || group.AcceptedAt["server-cs2"] == "" {
		t.Fatalf("saved acceptance timestamp = %#v, found=%v", group.AcceptedAt, ok)
	}
	if !strings.Contains(license, group.AcceptedAt["server-cs2"]) {
		t.Fatalf("license config timestamp does not match persisted state: %q", license)
	}
}

func TestReconcile400IsNonfatalAndLoggedOnce(t *testing.T) {
	var mu sync.Mutex
	putCount := 0
	cpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/groups/group-1/servers":
			_, _ = w.Write([]byte(`{"servers":[{"id":"server-cs2","game_id":"cs2"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/servers/server-cs2":
			_, _ = w.Write([]byte(`{
				"id":"server-cs2","group_id":"group-1","game_id":"cs2",
				"service":{"loopback_ip":"127.77.0.5","ports":[{"port":27015,"protocol":"udp"}]},
				"summary":{"address":"127.77.0.5:27015"}
			}`))
		case r.Method == http.MethodPut && r.URL.Path == "/v1/servers/server-cs2/addons/autotournament/readyup":
			mu.Lock()
			putCount++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"unsupported target"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpServer.Close()

	var logs bytes.Buffer
	service, store, _ := newTestService(t, cpServer.URL, &ReadyUpPin{
		URL:     "https://releases.example.test/readyup.zip",
		SHA256:  strings.Repeat("b", 64),
		License: "commercial",
	})
	service.logger = slog.New(slog.NewTextHandler(&logs, nil))
	if err := store.Save("group-1", state.Group{ATURL: "https://at.example", ATToken: "token", FleetKey: "fleet"}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := service.ReconcileGroup(context.Background(), "group-1"); err != nil {
			t.Fatalf("HTTP 400 should be nonfatal: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if putCount != 2 {
		t.Fatalf("addon PUT count = %d, want retry on next pass", putCount)
	}
	if got := strings.Count(logs.String(), "Varde rejected Ready Up addon spec; skipping server"); got != 1 {
		t.Fatalf("unsupported target warning count = %d, want 1: %s", got, logs.String())
	}
}

func TestReadyUpFleetConfigOmitsUnknownAddress(t *testing.T) {
	spec, err := buildReadyUpSpec(
		state.Group{ATURL: "https://at.example", FleetKey: "fleet"},
		varde.Server{},
		"2025-01-02T03:04:05Z",
		ReadyUpPin{URL: "https://readyup.example/file.zip", SHA256: strings.Repeat("c", 64), License: "commercial"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(spec.Files["cfg/ReadyUp/fleet.cfg"], "public_addr") {
		t.Fatalf("fleet config unexpectedly contains unknown address: %q", spec.Files["cfg/ReadyUp/fleet.cfg"])
	}
}

func reflectSearchPath(value any) bool {
	paths, ok := value.([]any)
	return ok && len(paths) == 1 && paths[0] == "csgo/readyup"
}
