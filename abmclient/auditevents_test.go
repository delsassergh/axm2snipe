package abmclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestBuildAuditEventsURL(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	u, err := buildAuditEventsURL(start, end)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	if got := q.Get("filter[type]"); got != "DEVICE_REMOVED_FROM_ORG" {
		t.Errorf("filter[type] = %q, want DEVICE_REMOVED_FROM_ORG", got)
	}
	if got := q.Get("filter[startTimestamp]"); got != "2026-01-01T00:00:00Z" {
		t.Errorf("filter[startTimestamp] = %q, want 2026-01-01T00:00:00Z", got)
	}
	if got := q.Get("filter[endTimestamp]"); got != "2026-07-01T00:00:00Z" {
		t.Errorf("filter[endTimestamp] = %q, want 2026-07-01T00:00:00Z", got)
	}
	if got := q.Get("fields[auditEvents]"); got != "eventDateTime,eventDataDeviceRemovedFromOrg" {
		t.Errorf("fields[auditEvents] = %q, want eventDateTime,eventDataDeviceRemovedFromOrg", got)
	}
	if got := q.Get("limit"); got != "1000" {
		t.Errorf("limit = %q, want 1000", got)
	}
}

// newAuditEventsTestServer serves a DEVICE_REMOVED_FROM_ORG event for serial
// SN001 on the first request, and one for SN002 (with an empty "next" link)
// on every request after that -- mirrors newPagedTestServer in paged_test.go.
func newAuditEventsTestServer() *httptest.Server {
	var server *httptest.Server
	requests := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auditEvents", func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if requests == 1 {
			fmt.Fprintf(w, `{"data":[{"id":"EVT1","attributes":{"eventDateTime":"2026-01-16T00:00:00Z","eventDataDeviceRemovedFromOrg":{"serialNumber":"SN001"}}}],"links":{"self":%q,"next":%q}}`,
				r.URL.String(), server.URL+"/v1/auditEvents?cursor=page2")
			return
		}
		fmt.Fprintf(w, `{"data":[{"id":"EVT2","attributes":{"eventDateTime":"2026-02-01T00:00:00Z","eventDataDeviceRemovedFromOrg":{"serialNumber":"SN002"}}}],"links":{"self":%q,"next":""}}`, r.URL.String())
	})
	server = httptest.NewServer(mux)
	return server
}

func TestFetchAuditEventsFrom_MultiplePages(t *testing.T) {
	server := newAuditEventsTestServer()
	defer server.Close()

	c := &Client{httpClient: server.Client()}
	released, err := c.fetchAuditEventsFrom(context.Background(), server.URL+"/v1/auditEvents?limit=1")
	if err != nil {
		t.Fatalf("fetchAuditEventsFrom returned error: %v", err)
	}
	if len(released) != 2 {
		t.Fatalf("expected 2 released devices, got %d: %+v", len(released), released)
	}
	if released["SN001"].EventDateTime.IsZero() {
		t.Error("SN001 missing or has zero EventDateTime")
	}
	if released["SN002"].EventDateTime.IsZero() {
		t.Error("SN002 missing or has zero EventDateTime")
	}
}

func TestFetchAuditEventsFrom_KeepsLatestEventPerSerial(t *testing.T) {
	// A device released, re-added, and released again should end up with the
	// later of the two release timestamps, not whichever page happened to be
	// processed last by coincidence.
	var server *httptest.Server
	requests := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auditEvents", func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if requests == 1 {
			fmt.Fprintf(w, `{"data":[{"id":"EVT1","attributes":{"eventDateTime":"2026-02-01T00:00:00Z","eventDataDeviceRemovedFromOrg":{"serialNumber":"SN001"}}}],"links":{"self":%q,"next":%q}}`,
				r.URL.String(), server.URL+"/v1/auditEvents?cursor=page2")
			return
		}
		// Earlier release event for the same serial, arriving on a later page.
		fmt.Fprintf(w, `{"data":[{"id":"EVT0","attributes":{"eventDateTime":"2026-01-01T00:00:00Z","eventDataDeviceRemovedFromOrg":{"serialNumber":"SN001"}}}],"links":{"self":%q,"next":""}}`, r.URL.String())
	})
	server = httptest.NewServer(mux)
	defer server.Close()

	c := &Client{httpClient: server.Client()}
	released, err := c.fetchAuditEventsFrom(context.Background(), server.URL+"/v1/auditEvents?limit=1")
	if err != nil {
		t.Fatalf("fetchAuditEventsFrom returned error: %v", err)
	}
	want := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	if !released["SN001"].EventDateTime.Equal(want) {
		t.Errorf("EventDateTime = %v, want the later timestamp %v", released["SN001"].EventDateTime, want)
	}
}

func TestFetchAuditEventsFrom_SkipsEventsWithoutSerial(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// One event missing eventDataDeviceRemovedFromOrg entirely (e.g. a
		// polymorphic type mismatch), one with an empty serial.
		fmt.Fprint(w, `{"data":[
			{"id":"EVT1","attributes":{"eventDateTime":"2026-01-01T00:00:00Z"}},
			{"id":"EVT2","attributes":{"eventDateTime":"2026-01-02T00:00:00Z","eventDataDeviceRemovedFromOrg":{"serialNumber":""}}}
		],"links":{"next":""}}`)
	}))
	defer server.Close()

	c := &Client{httpClient: server.Client()}
	released, err := c.fetchAuditEventsFrom(context.Background(), server.URL+"/v1/auditEvents")
	if err != nil {
		t.Fatalf("fetchAuditEventsFrom returned error: %v", err)
	}
	if len(released) != 0 {
		t.Errorf("expected no released devices, got %+v", released)
	}
}

func TestFetchAuditEventsFrom_ErrorOnNon200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"detail":"rate limited"}`)
	}))
	defer server.Close()

	c := &Client{httpClient: server.Client()}
	_, err := c.fetchAuditEventsFrom(context.Background(), server.URL+"/v1/auditEvents")
	if err == nil {
		t.Fatal("expected error on non-200 response, got nil")
	}
}
