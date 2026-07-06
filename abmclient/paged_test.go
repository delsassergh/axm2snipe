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

// newPagedTestServer serves two orgDevices pages: DEV1 on the first request,
// DEV2 (with an empty "next" link) on every request after that.
func newPagedTestServer(onRequest func()) *httptest.Server {
	var server *httptest.Server
	requests := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/orgDevices", func(w http.ResponseWriter, r *http.Request) {
		requests++
		if onRequest != nil {
			onRequest()
		}
		w.Header().Set("Content-Type", "application/json")
		if requests == 1 {
			fmt.Fprintf(w, `{"data":[{"id":"DEV1","type":"orgDevices"}],"links":{"self":%q,"next":%q}}`,
				r.URL.String(), server.URL+"/v1/orgDevices?page=2")
			return
		}
		fmt.Fprintf(w, `{"data":[{"id":"DEV2","type":"orgDevices"}],"links":{"self":%q,"next":""}}`, r.URL.String())
	})
	server = httptest.NewServer(mux)
	return server
}

// TestBuildOrgDevicesURL_IncludesFields is a regression test: axm2snipe used
// to call FetchDevicesPaged with no Fields at all, and Apple's /v1/orgDevices
// silently excluded released devices from the response entirely (not just
// the releasedFromOrgDateTime attribute) unless fields[orgDevices] was
// explicitly set. Every caller building this URL must pass an explicit
// field list -- see sync.orgDeviceFields.
func TestBuildOrgDevicesURL_IncludesFields(t *testing.T) {
	u, err := buildOrgDevicesURL([]string{"serialNumber", "releasedFromOrgDateTime"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	if got := q.Get("fields[orgDevices]"); got != "serialNumber,releasedFromOrgDateTime" {
		t.Errorf("fields[orgDevices] = %q, want %q", got, "serialNumber,releasedFromOrgDateTime")
	}
	if got := q.Get("limit"); got != "100" {
		t.Errorf("limit = %q, want \"100\"", got)
	}
}

func TestBuildOrgDevicesURL_NoFieldsOmitsParam(t *testing.T) {
	u, err := buildOrgDevicesURL(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Query().Has("fields[orgDevices]") {
		t.Error("fields[orgDevices] should be omitted when no fields are given")
	}
}

// Note: FetchDevicesPaged's fresh-start (non-Resume) path builds its URL
// against abm.DefaultAPIBaseURL, which isn't overridable for a local test
// server -- so the fields[orgDevices] wiring is covered by
// TestBuildOrgDevicesURL_IncludesFields (the URL builder itself) plus
// sync.orgDeviceFields being passed into PagedFetchOptions.Fields at the one
// production call site (sync.fetchOrgDevicesPaced).

func TestFetchDevicesPaged_MultiplePages(t *testing.T) {
	server := newPagedTestServer(nil)
	defer server.Close()

	c := &Client{httpClient: server.Client()}

	var collected []string
	failedURL, err := c.FetchDevicesPaged(context.Background(), PagedFetchOptions{
		Resume: server.URL + "/v1/orgDevices?limit=1",
	}, func(res PagedDevicesResult) error {
		for _, d := range res.Devices {
			collected = append(collected, d.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("FetchDevicesPaged returned error: %v", err)
	}
	if failedURL != "" {
		t.Errorf("failedURL = %q, want empty on success", failedURL)
	}
	if len(collected) != 2 || collected[0] != "DEV1" || collected[1] != "DEV2" {
		t.Errorf("collected = %v, want [DEV1 DEV2]", collected)
	}
}

func TestFetchDevicesPaged_StopsAndReturnsFailedURLOn429(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"detail":"You have exceeded the allowable number of requests for this time period","code":"RATE_LIMIT_EXCEEDED","status":429}`)
	}))
	defer server.Close()

	c := &Client{httpClient: server.Client()}

	startURL := server.URL + "/v1/orgDevices?limit=1"
	var onPageCalled bool
	failedURL, err := c.FetchDevicesPaged(context.Background(), PagedFetchOptions{
		Resume: startURL,
	}, func(res PagedDevicesResult) error {
		onPageCalled = true
		return nil
	})
	if err == nil {
		t.Fatal("expected error on 429, got nil")
	}
	if onPageCalled {
		t.Error("onPage should not be called when the request itself fails")
	}
	if failedURL != startURL {
		t.Errorf("failedURL = %q, want %q (so a retry re-fetches the same page)", failedURL, startURL)
	}
}

func TestFetchDevicesPaged_PacesBetweenPages(t *testing.T) {
	server := newPagedTestServer(nil)
	defer server.Close()

	c := &Client{httpClient: server.Client()}

	delay := 50 * time.Millisecond
	start := time.Now()
	_, err := c.FetchDevicesPaged(context.Background(), PagedFetchOptions{
		Resume: server.URL + "/v1/orgDevices?limit=1",
		Delay:  delay,
	}, func(res PagedDevicesResult) error { return nil })
	if err != nil {
		t.Fatalf("FetchDevicesPaged returned error: %v", err)
	}
	if elapsed := time.Since(start); elapsed < delay {
		t.Errorf("elapsed = %v, want at least %v (delay should apply before the second page)", elapsed, delay)
	}
}

func TestFetchDevicesPaged_OnPageErrorReturnsCurrentPageURL(t *testing.T) {
	server := newPagedTestServer(nil)
	defer server.Close()

	c := &Client{httpClient: server.Client()}

	startURL := server.URL + "/v1/orgDevices?limit=1"
	wantErr := fmt.Errorf("disk full")
	failedURL, err := c.FetchDevicesPaged(context.Background(), PagedFetchOptions{
		Resume: startURL,
	}, func(res PagedDevicesResult) error {
		return wantErr
	})
	if err != wantErr {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if failedURL != startURL {
		t.Errorf("failedURL = %q, want %q (the page that was fetched but not persisted)", failedURL, startURL)
	}
}
