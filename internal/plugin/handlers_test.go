package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/grendalaget/varde-plugin-autotournament/internal/state"
	"github.com/grendalaget/varde-plugin-autotournament/internal/varde"
)

func TestSettingsRejectsBadAutoTournamentToken(t *testing.T) {
	atServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tournaments":
			w.WriteHeader(http.StatusOK)
		case "/api/fleet/keys":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer atServer.Close()
	cpServer := httptest.NewServer(http.NotFoundHandler())
	defer cpServer.Close()
	service, _, secret := newTestService(t, cpServer.URL, nil)
	recorder := servePluginRequest(t, service, secret, "settings", map[string]any{
		"group_id": "group-1",
		"user":     map[string]string{"id": "user-1"},
		"settings": map[string]string{
			"at_url":   atServer.URL,
			"at_token": "bad-token",
		},
	})
	if recorder.Code < 400 || recorder.Code >= 500 {
		t.Fatalf("settings status = %d, want 4xx: %s", recorder.Code, recorder.Body)
	}
}

func TestSettingsRequiresAdminOrOwner(t *testing.T) {
	for _, role := range []string{"member", "admin", "owner"} {
		t.Run(role, func(t *testing.T) {
			var atCalls atomic.Int32
			atServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atCalls.Add(1)
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/fleet/keys":
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodPost && r.URL.Path == "/api/fleet/keys":
					_, _ = w.Write([]byte(`{"value":"new-fleet-key"}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer atServer.Close()
			cpServer := httptest.NewServer(http.NotFoundHandler())
			defer cpServer.Close()
			service, store, secret := newTestService(t, cpServer.URL, nil)
			previous := state.Group{
				ATURL:      "https://previous.at.example.test",
				ATToken:    "previous-token",
				FleetKey:   "previous-fleet-key",
				AcceptedAt: map[string]string{"server-1": "2026-01-01T00:00:00Z"},
			}
			if err := store.Save("group-1", previous); err != nil {
				t.Fatal(err)
			}

			recorder := servePluginRequestWithRole(t, service, secret, "settings", role, map[string]any{
				"group_id": "group-1",
				"user":     map[string]string{"id": "user-1"},
				"settings": map[string]string{
					"at_url":   atServer.URL,
					"at_token": "new-token",
				},
			})
			if role == "member" {
				if recorder.Code != http.StatusForbidden ||
					!strings.Contains(recorder.Body.String(), "admin role required") {
					t.Fatalf("member settings response = %d %s, want 403 admin role required", recorder.Code, recorder.Body)
				}
				got, ok := store.Get("group-1")
				if !ok || !reflect.DeepEqual(got, previous) {
					t.Fatalf("stored group after member settings = %#v, found=%v; want unchanged %#v", got, ok, previous)
				}
				if got := atCalls.Load(); got != 0 {
					t.Fatalf("Auto Tournament calls for member settings = %d, want 0", got)
				}
				return
			}

			if recorder.Code != http.StatusOK {
				t.Fatalf("%s settings status = %d, want 200: %s", role, recorder.Code, recorder.Body)
			}
			got, ok := store.Get("group-1")
			if !ok || got.ATURL != atServer.URL || got.ATToken != "new-token" ||
				got.FleetKey != "new-fleet-key" {
				t.Fatalf("stored group after %s settings = %#v, found=%v", role, got, ok)
			}
			if got := atCalls.Load(); got != 2 {
				t.Fatalf("Auto Tournament calls for %s settings = %d, want 2", role, got)
			}
		})
	}
}

func TestSettingsRejectsBadTokenWithoutReplacingStoredFleetKey(t *testing.T) {
	var keyListCalls atomic.Int32
	var createCalls atomic.Int32
	atServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/tournaments":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/api/fleet/keys":
			keyListCalls.Add(1)
			if r.Header.Get("Authorization") != "Bearer bad-token" {
				t.Errorf("fleet-key list authorization = %q, want bad-token bearer", r.Header.Get("Authorization"))
			}
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/fleet/keys":
			createCalls.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"value":"unexpected-fleet-key"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer atServer.Close()
	cpServer := httptest.NewServer(http.NotFoundHandler())
	defer cpServer.Close()
	service, store, secret := newTestService(t, cpServer.URL, nil)
	previous := state.Group{
		ATURL:      "https://previous.at.example.test",
		ATToken:    "previous-token",
		FleetKey:   "stored-fleet-key",
		AcceptedAt: map[string]string{"server-1": "2026-01-01T00:00:00Z"},
	}
	if err := store.Save("group-1", previous); err != nil {
		t.Fatal(err)
	}

	recorder := servePluginRequest(t, service, secret, "settings", map[string]any{
		"group_id": "group-1",
		"user":     map[string]string{"id": "user-1"},
		"settings": map[string]string{
			"at_url":   atServer.URL,
			"at_token": "bad-token",
		},
	})
	if recorder.Code < 400 || recorder.Code >= 500 {
		t.Fatalf("settings status = %d, want 4xx: %s", recorder.Code, recorder.Body)
	}
	got, ok := store.Get("group-1")
	if !ok || !reflect.DeepEqual(got, previous) {
		t.Fatalf("stored group after rejected token = %#v, found=%v; want unchanged %#v", got, ok, previous)
	}
	if got := keyListCalls.Load(); got != 1 {
		t.Errorf("fleet-key list calls = %d, want 1", got)
	}
	if got := createCalls.Load(); got != 0 {
		t.Errorf("CreateFleetKey calls = %d, want 0", got)
	}
}

func TestSettingsCreatesFleetKeyOnce(t *testing.T) {
	var mu sync.Mutex
	createCalls := 0
	createName := ""
	atServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/fleet/keys":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/fleet/keys":
			var request struct {
				Name     string `json:"name"`
				AutoLink bool   `json:"autoLink"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode fleet-key request: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			mu.Lock()
			createCalls++
			createName = request.Name
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"value":"fleet-key-from-at"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer atServer.Close()
	cpServer := httptest.NewServer(http.NotFoundHandler())
	defer cpServer.Close()
	service, store, secret := newTestService(t, cpServer.URL, nil)
	payload := map[string]any{
		"group_id": "group-1",
		"user":     map[string]string{"id": "user-1"},
		"settings": map[string]string{
			"at_url":   atServer.URL,
			"at_token": "at-token",
		},
	}
	for range 2 {
		recorder := servePluginRequest(t, service, secret, "settings", payload)
		if recorder.Code != http.StatusOK {
			t.Fatalf("settings status = %d, want 200: %s", recorder.Code, recorder.Body)
		}
		var response struct {
			Store map[string]string `json:"store"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Store["fleet_key"] != "fleet-key-from-at" ||
			response.Store["fleet_key_url"] != atServer.URL {
			t.Fatalf("settings response store = %#v", response.Store)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if createCalls != 1 {
		t.Fatalf("CreateFleetKey calls = %d, want 1", createCalls)
	}
	if createName != "varde-group-1" {
		t.Fatalf("fleet-key name = %q", createName)
	}
	group, ok := store.Get("group-1")
	if !ok || group.FleetKey != "fleet-key-from-at" {
		t.Fatalf("saved group = %#v, found=%v", group, ok)
	}
}

func TestSettingsRecreatesFleetKeyWhenATURLChanges(t *testing.T) {
	var previousCalls atomic.Int32
	previousServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		previousCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer previousServer.Close()
	var createCalls atomic.Int32
	newServer := newFleetKeyATServer(t, "fleet-key-from-new-at", &createCalls)
	defer newServer.Close()
	cpServer := httptest.NewServer(http.NotFoundHandler())
	defer cpServer.Close()
	service, store, secret := newTestService(t, cpServer.URL, nil)
	if err := store.Save("group-1", state.Group{
		ATURL:    previousServer.URL,
		ATToken:  "old-token",
		FleetKey: "fleet-key-from-old-at",
	}); err != nil {
		t.Fatal(err)
	}

	recorder := servePluginRequest(t, service, secret, "settings", map[string]any{
		"group_id": "group-1",
		"user":     map[string]string{"id": "user-1"},
		"settings": map[string]string{
			"at_url":   newServer.URL,
			"at_token": "new-token",
		},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("settings status = %d, want 200: %s", recorder.Code, recorder.Body)
	}
	var response struct {
		Store map[string]string `json:"store"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Store["fleet_key"] != "fleet-key-from-new-at" ||
		response.Store["fleet_key_url"] != newServer.URL {
		t.Fatalf("settings response store = %#v", response.Store)
	}
	group, ok := store.Get("group-1")
	if !ok || group.ATURL != newServer.URL || group.FleetKey != "fleet-key-from-new-at" {
		t.Fatalf("saved group = %#v, found=%v", group, ok)
	}
	if got := createCalls.Load(); got != 1 {
		t.Fatalf("CreateFleetKey calls on new AT = %d, want 1", got)
	}
	if got := previousCalls.Load(); got != 0 {
		t.Fatalf("requests to previous AT = %d, want 0", got)
	}
}

func TestSettingsIgnoresFleetKeyHintForDifferentURL(t *testing.T) {
	var createCalls atomic.Int32
	atServer := newFleetKeyATServer(t, "fleet-key-from-at", &createCalls)
	defer atServer.Close()
	cpServer := httptest.NewServer(http.NotFoundHandler())
	defer cpServer.Close()
	service, _, secret := newTestService(t, cpServer.URL, nil)

	recorder := servePluginRequest(t, service, secret, "settings", map[string]any{
		"group_id": "group-1",
		"user":     map[string]string{"id": "user-1"},
		"settings": map[string]string{
			"at_url":        atServer.URL,
			"at_token":      "at-token",
			"fleet_key":     "stale-fleet-key",
			"fleet_key_url": "https://old-at.example.test",
		},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("settings status = %d, want 200: %s", recorder.Code, recorder.Body)
	}
	var response struct {
		Store map[string]string `json:"store"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Store["fleet_key"] != "fleet-key-from-at" ||
		response.Store["fleet_key_url"] != atServer.URL {
		t.Fatalf("settings response store = %#v", response.Store)
	}
	if got := createCalls.Load(); got != 1 {
		t.Fatalf("CreateFleetKey calls = %d, want 1", got)
	}
}

func TestSettingsReusesFleetKeyHintForMatchingURL(t *testing.T) {
	var createCalls atomic.Int32
	atServer := newFleetKeyATServer(t, "unexpected-fleet-key", &createCalls)
	defer atServer.Close()
	cpServer := httptest.NewServer(http.NotFoundHandler())
	defer cpServer.Close()
	service, _, secret := newTestService(t, cpServer.URL, nil)

	recorder := servePluginRequest(t, service, secret, "settings", map[string]any{
		"group_id": "group-1",
		"user":     map[string]string{"id": "user-1"},
		"settings": map[string]string{
			"at_url":        atServer.URL,
			"at_token":      "at-token",
			"fleet_key":     "matching-fleet-key",
			"fleet_key_url": atServer.URL + "/",
		},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("settings status = %d, want 200: %s", recorder.Code, recorder.Body)
	}
	var response struct {
		Store map[string]string `json:"store"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Store["fleet_key"] != "matching-fleet-key" ||
		response.Store["fleet_key_url"] != atServer.URL {
		t.Fatalf("settings response store = %#v", response.Store)
	}
	if got := createCalls.Load(); got != 0 {
		t.Fatalf("CreateFleetKey calls = %d, want 0", got)
	}
}

func TestDisableReportsStateDeleteFailureAndKeepsGroupActive(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based state deletion failure cannot be tested as root")
	}
	cpServer := httptest.NewServer(http.NotFoundHandler())
	defer cpServer.Close()
	stateDir := t.TempDir()
	service, store, secret := newTestServiceWithStateDir(t, cpServer.URL, &ReadyUpPin{
		URL:     "https://readyup.example.test/readyup.zip",
		SHA256:  strings.Repeat("a", 64),
		License: "commercial",
	}, stateDir)
	if err := store.Save("group-1", state.Group{
		ATURL:    "https://at.example.test",
		ATToken:  "secret-at-token",
		FleetKey: "fleet-key",
	}); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	service.logger = slog.New(slog.NewTextHandler(&logs, nil))
	service.lastSpecHashes["group-1/server-1"] = "spec-hash"
	service.warned400["group-1/server-1"] = true

	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chmod(stateDir, 0o700); err != nil {
			t.Errorf("restore state directory permissions: %v", err)
		}
	}()

	recorder := servePluginRequest(t, service, secret, "disable", map[string]any{
		"group_id": "group-1",
		"user":     map[string]string{"id": "user-1"},
	})
	if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), "could not disable plugin") {
		t.Fatalf("disable response = %d %s, want 500 could not disable plugin", recorder.Code, recorder.Body)
	}
	if _, ok := store.Get("group-1"); !ok {
		t.Fatal("group state was not restored after deletion failed")
	}
	if got := service.lastSpecHashes["group-1/server-1"]; got != "spec-hash" {
		t.Fatalf("active group's addon spec hash = %q, want retained spec-hash", got)
	}
	if !service.warned400["group-1/server-1"] {
		t.Fatal("active group's warning state was not retained after deletion failed")
	}
	if !strings.Contains(logs.String(), "level=ERROR") ||
		strings.Contains(logs.String(), "secret-at-token") {
		t.Fatalf("delete failure log = %q, want an error without secrets", logs.String())
	}
	select {
	case change := <-service.groupChanges:
		t.Fatalf("disable event was queued after deletion failed: %#v", change)
	default:
	}
}

func TestDisableRequiresAdminOrOwner(t *testing.T) {
	for _, role := range []string{"member", "admin", "owner"} {
		t.Run(role, func(t *testing.T) {
			var atCalls atomic.Int32
			atServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atCalls.Add(1)
				http.NotFound(w, r)
			}))
			defer atServer.Close()
			cpServer := httptest.NewServer(http.NotFoundHandler())
			defer cpServer.Close()
			service, store, secret := newTestService(t, cpServer.URL, &ReadyUpPin{
				URL:     "https://readyup.example.test/readyup.zip",
				SHA256:  strings.Repeat("a", 64),
				License: "commercial",
			})
			previous := state.Group{
				ATURL:    atServer.URL,
				ATToken:  "at-token",
				FleetKey: "fleet-key",
			}
			if err := store.Save("group-1", previous); err != nil {
				t.Fatal(err)
			}
			service.lastSpecHashes["group-1/server-1"] = "spec-hash"
			service.warned400["group-1/server-1"] = true

			recorder := servePluginRequestWithRole(t, service, secret, "disable", role, map[string]any{
				"group_id": "group-1",
				"user":     map[string]string{"id": "user-1"},
			})
			if role == "member" {
				if recorder.Code != http.StatusForbidden ||
					!strings.Contains(recorder.Body.String(), "admin role required") {
					t.Fatalf("member disable response = %d %s, want 403 admin role required", recorder.Code, recorder.Body)
				}
				got, ok := store.Get("group-1")
				if !ok || !reflect.DeepEqual(got, previous) {
					t.Fatalf("stored group after member disable = %#v, found=%v; want unchanged %#v", got, ok, previous)
				}
				if service.lastSpecHashes["group-1/server-1"] != "spec-hash" ||
					!service.warned400["group-1/server-1"] {
					t.Fatal("member disable changed active addon reconcile state")
				}
				select {
				case change := <-service.groupChanges:
					t.Fatalf("member disable queued a group change: %#v", change)
				default:
				}
			} else {
				if recorder.Code != http.StatusOK {
					t.Fatalf("%s disable status = %d, want 200: %s", role, recorder.Code, recorder.Body)
				}
				if _, ok := store.Get("group-1"); ok {
					t.Fatalf("group state remains after %s disable", role)
				}
				if _, ok := service.lastSpecHashes["group-1/server-1"]; ok ||
					service.warned400["group-1/server-1"] {
					t.Fatalf("%s disable retained addon reconcile state", role)
				}
				select {
				case change := <-service.groupChanges:
					if change.groupID != "group-1" || change.enable {
						t.Fatalf("%s disable queued group change %#v", role, change)
					}
				default:
					t.Fatalf("%s disable did not queue a group change", role)
				}
			}
			if got := atCalls.Load(); got != 0 {
				t.Fatalf("Auto Tournament calls for %s disable = %d, want 0", role, got)
			}
		})
	}
}

func newFleetKeyATServer(t *testing.T, key string, createCalls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/fleet/keys":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/fleet/keys":
			createCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"value":%q}`, key)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestListTournamentsAllowsMembers(t *testing.T) {
	var atCalls atomic.Int32
	atServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/tournaments" {
			http.NotFound(w, r)
			return
		}
		atCalls.Add(1)
		_, _ = w.Write([]byte(`{"tournaments":[]}`))
	}))
	defer atServer.Close()
	cpServer := httptest.NewServer(http.NotFoundHandler())
	defer cpServer.Close()
	service, store, secret := newTestService(t, cpServer.URL, nil)
	if err := store.Save("group-1", state.Group{
		ATURL:    atServer.URL,
		ATToken:  "at-token",
		FleetKey: "fleet-key",
	}); err != nil {
		t.Fatal(err)
	}

	recorder := servePluginRequestWithRole(t, service, secret, "list", "member", map[string]any{
		"group_id": "group-1",
		"user":     map[string]string{"id": "user-1"},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("member tournament list status = %d, want 200: %s", recorder.Code, recorder.Body)
	}
	if got := atCalls.Load(); got != 1 {
		t.Fatalf("Auto Tournament list calls = %d, want 1", got)
	}
}

func TestCreateActionSelectionAndSkippedNotes(t *testing.T) {
	members := []map[string]any{
		{"user_id": "user-1", "display_name": "One", "steam_id": "76561190000000001"},
		{"user_id": "user-2", "display_name": "Two", "steam_id": "76561190000000002"},
		{"user_id": "user-3", "display_name": "Three", "steam_id": "76561190000000003"},
		{"user_id": "user-4", "display_name": "Four", "steam_id": "76561190000000004"},
		{"user_id": "user-5", "display_name": "Five", "steam_id": "76561190000000005"},
		{"user_id": "user-6", "display_name": "NoSteam", "steam_id": nil},
	}
	cpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/groups/group-1/members" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"members": members})
			return
		}
		http.NotFound(w, r)
	}))
	defer cpServer.Close()

	var mu sync.Mutex
	imports := make([][]string, 0, 2)
	importedNames := make([][]string, 0, 2)
	createCount := 0
	startCount := 0
	atServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/players/bulk-import":
			var players []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&players); err != nil {
				t.Errorf("decode imported players: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			ids := make([]string, 0, len(players))
			names := make([]string, 0, len(players))
			for _, player := range players {
				ids = append(ids, player.ID)
				names = append(names, player.Name)
			}
			mu.Lock()
			imports = append(imports, ids)
			importedNames = append(importedNames, names)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"errors":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/tournaments":
			mu.Lock()
			createCount++
			id := fmt.Sprintf("tournament-%d", createCount)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"tournament":{"id":%q,"name":"Cup","type":"shuffle","status":"created"}}`, id)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/tournament/") &&
			strings.HasSuffix(r.URL.Path, "/register-players"):
			_, _ = w.Write([]byte(`{"errors":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/tournament/start":
			mu.Lock()
			startCount++
			mu.Unlock()
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer atServer.Close()

	service, store, secret := newTestService(t, cpServer.URL, nil)
	if err := store.Save("group-1", state.Group{
		ATURL:    atServer.URL,
		ATToken:  "at-token",
		FleetKey: "fleet-key",
	}); err != nil {
		t.Fatal(err)
	}
	for index, selected := range [][]map[string]any{
		{
			{"user_id": "user-1", "display_name": "Untrusted One", "steam_id": "99999999999999991"},
			{"user_id": "user-2", "display_name": "Untrusted Two", "steam_id": "99999999999999992"},
			{"user_id": "user-3", "display_name": "Untrusted Three", "steam_id": "99999999999999993"},
			{"user_id": "user-4", "display_name": "Untrusted Four", "steam_id": "99999999999999994"},
		},
		{},
	} {
		payload := map[string]any{
			"group_id": "group-1",
			"user":     map[string]string{"id": "user-1"},
			"values": map[string]any{
				"name":      "Cup",
				"format":    "shuffle",
				"team_size": 2,
				"maps":      []string{"de_mirage"},
				"players":   selected,
			},
		}
		recorder := servePluginRequestWithRole(t, service, secret, "action:create", "member", payload)
		if recorder.Code != http.StatusOK {
			t.Fatalf("create action status = %d, want 200: %s", recorder.Code, recorder.Body)
		}
		var response actionResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Href != atServer.URL+"/tournament/"+fmt.Sprintf("tournament-%d", index+1) {
			t.Fatalf("tournament href = %q", response.Href)
		}
		if len(selected) == 0 && !strings.Contains(strings.Join(response.Notes, " "), "NoSteam") {
			t.Fatalf("all-member response notes = %#v, want missing Steam ID", response.Notes)
		}
		if len(selected) != 0 && len(response.Notes) != 0 {
			t.Fatalf("selected-member response notes = %#v, want none", response.Notes)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	wantSelected := []string{
		"76561190000000001",
		"76561190000000002",
		"76561190000000003",
		"76561190000000004",
	}
	wantAll := append(append([]string(nil), wantSelected...), "76561190000000005")
	if len(imports) != 2 || !reflect.DeepEqual(imports[0], wantSelected) || !reflect.DeepEqual(imports[1], wantAll) {
		t.Fatalf("imported players = %#v", imports)
	}
	wantSelectedNames := []string{"One", "Two", "Three", "Four"}
	wantAllNames := []string{"One", "Two", "Three", "Four", "Five"}
	if len(importedNames) != 2 || !reflect.DeepEqual(importedNames[0], wantSelectedNames) ||
		!reflect.DeepEqual(importedNames[1], wantAllNames) {
		t.Fatalf("imported player names = %#v", importedNames)
	}
	if createCount != 2 || startCount != 2 {
		t.Fatalf("tournament creates/starts = %d/%d, want 2/2", createCount, startCount)
	}
}

func TestRawMemberIDs(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{
			name: "objects",
			raw:  `[{"user_id":"user-1","display_name":"One","steam_id":"76561190000000001"},{"user_id":"","display_name":"Empty"}]`,
			want: []string{"user-1"},
		},
		{
			name: "strings",
			raw:  `["user-1","","user-2"]`,
			want: []string{"user-1", "user-2"},
		},
		{
			name:    "missing user_id",
			raw:     `[{"display_name":"One"}]`,
			wantErr: true,
		},
		{
			name:    "non-array",
			raw:     `{"user_id":"user-1"}`,
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := rawMemberIDs(json.RawMessage(test.raw))
			if (err != nil) != test.wantErr {
				t.Fatalf("rawMemberIDs(%s) error = %v, want error presence %t", test.raw, err, test.wantErr)
			}
			if !test.wantErr && !reflect.DeepEqual(got, test.want) {
				t.Fatalf("rawMemberIDs(%s) = %#v, want %#v", test.raw, got, test.want)
			}
		})
	}
}

func newTestService(t *testing.T, cpURL string, pin *ReadyUpPin) (*Service, *state.Store, []byte) {
	return newTestServiceWithStateDir(t, cpURL, pin, t.TempDir())
}

func newTestServiceWithStateDir(
	t *testing.T,
	cpURL string,
	pin *ReadyUpPin,
	stateDir string,
) (*Service, *state.Store, []byte) {
	t.Helper()
	secret := []byte(strings.Repeat("plugin-secret-", 3))
	store, err := state.Open(stateDir, secret)
	if err != nil {
		t.Fatal(err)
	}
	vardeClient, err := varde.NewClient(cpURL, "service-token")
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(Config{
		Secret:         secret,
		AllowPrivateAT: true,
		State:          store,
		Varde:          vardeClient,
		ReadyUp:        pin,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, store, secret
}

func servePluginRequest(t *testing.T, service *Service, secret []byte, operation string, payload any) *httptest.ResponseRecorder {
	return servePluginRequestWithRole(t, service, secret, operation, "owner", payload)
}

func servePluginRequestWithRole(
	t *testing.T,
	service *Service,
	secret []byte,
	operation string,
	role string,
	payload any,
) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		GroupID: "group-1",
		Role:    role,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "varde-control-plane",
			Subject:   "user-1",
			Audience:  jwt.ClaimStrings{pluginID},
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
			ID:        "test-jti",
		},
	}).SignedString(secret)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "/varde/tournaments"
	if operation == "settings" {
		endpoint = "/varde/v1/settings"
	} else if operation == "disable" {
		endpoint = "/varde/v1/disable"
	}
	request := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Varde-Op", operation)
	recorder := httptest.NewRecorder()
	service.Handler().ServeHTTP(recorder, request)
	return recorder
}
