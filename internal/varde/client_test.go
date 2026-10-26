package varde

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestStreamEventsResumesWithLastEventID(t *testing.T) {
	var mu sync.Mutex
	requestCount := 0
	lastHeader := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestCount++
		current := requestCount
		lastHeader = r.Header.Get("Last-Event-ID")
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer service-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if current == 1 {
			_, _ = w.Write([]byte("id: 42\ndata: {\"group_id\":\"group-1\",\"type\":\"server.created\"}\n\n"))
			w.(http.Flusher).Flush()
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "service-token")
	if err != nil {
		t.Fatal(err)
	}
	var event Event
	lastID, err := client.StreamEvents(context.Background(), "group-1", "", func(got Event) {
		event = got
	})
	if !errors.Is(err, io.EOF) {
		t.Fatalf("first stream error = %v, want EOF", err)
	}
	if lastID != "42" || event.ID != 42 || event.Type != "server.created" || event.GroupID != "group-1" {
		t.Fatalf("event = %#v, last ID = %q", event, lastID)
	}
	lastID, err = client.StreamEvents(context.Background(), "group-1", lastID, nil)
	if !errors.Is(err, io.EOF) || lastID != "42" {
		t.Fatalf("resumed stream = %q, %v", lastID, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requestCount != 2 || lastHeader != "42" {
		t.Fatalf("requests = %d, Last-Event-ID = %q", requestCount, lastHeader)
	}
}
