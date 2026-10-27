package plugin

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthzDoesNotRequireAuthentication(t *testing.T) {
	cpServer := httptest.NewServer(http.NotFoundHandler())
	defer cpServer.Close()
	service, _, _ := newTestService(t, cpServer.URL, nil)

	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	recorder := httptest.NewRecorder()
	service.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || recorder.Body.String() != "ok" {
		t.Fatalf("GET /healthz = %d %q, want 200 %q", recorder.Code, recorder.Body.String(), "ok")
	}
}

func TestDisableGroupInvalidatesAddonCache(t *testing.T) {
	cpServer := httptest.NewServer(http.NotFoundHandler())
	defer cpServer.Close()
	service, _, _ := newTestService(t, cpServer.URL, nil)
	service.lastSpecHashes["group-1/server-1"] = "hash-1"
	service.lastSpecHashes["group-2/server-1"] = "hash-2"
	service.warned400["group-1/server-1"] = true
	service.warned400["group-2/server-1"] = true
	service.warnedLicense["group-1"] = true
	service.warnedLicense["group-2"] = true

	service.disableGroup("group-1")

	if _, ok := service.lastSpecHashes["group-1/server-1"]; ok {
		t.Fatal("disabled group's addon spec hash was retained")
	}
	if _, ok := service.warned400["group-1/server-1"]; ok {
		t.Fatal("disabled group's warning state was retained")
	}
	if service.warnedLicense["group-1"] {
		t.Fatal("disabled group's licence warning state was retained")
	}
	if service.lastSpecHashes["group-2/server-1"] != "hash-2" ||
		!service.warned400["group-2/server-1"] || !service.warnedLicense["group-2"] {
		t.Fatal("disabling one group changed another group's cache")
	}
	select {
	case change := <-service.groupChanges:
		if change.groupID != "group-1" || change.enable {
			t.Fatalf("group change = %#v", change)
		}
	default:
		t.Fatal("disable event was not queued")
	}
}
