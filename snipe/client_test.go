package snipe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	snipeit "github.com/michellepellon/go-snipeit"
)

// newTestClient creates a Client backed by a test HTTP server.
func newTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c, err := NewClient(srv.URL, "test-api-key", false)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewClient_TrimTrailingSlash(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	c, err := NewClient(srv.URL+"/", "test-key", false)
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

// --- Dry-run enforcement tests ---

func TestCreateModel_DryRun(t *testing.T) {
	c := &Client{DryRun: true}
	_, err := c.CreateModel(context.Background(), snipeit.Model{})
	if !errors.Is(err, ErrDryRun) {
		t.Errorf("expected ErrDryRun, got %v", err)
	}
}

func TestUpdateModelFieldset_DryRun(t *testing.T) {
	c := &Client{DryRun: true}
	_, err := c.UpdateModelFieldset(context.Background(), snipeit.Model{}, 2)
	if !errors.Is(err, ErrDryRun) {
		t.Errorf("expected ErrDryRun, got %v", err)
	}
}

func TestCreateSupplier_DryRun(t *testing.T) {
	c := &Client{DryRun: true}
	_, err := c.CreateSupplier(context.Background(), "Test Supplier")
	if !errors.Is(err, ErrDryRun) {
		t.Errorf("expected ErrDryRun, got %v", err)
	}
}

func TestCreateAsset_DryRun(t *testing.T) {
	c := &Client{DryRun: true}
	_, err := c.CreateAsset(context.Background(), snipeit.Asset{})
	if !errors.Is(err, ErrDryRun) {
		t.Errorf("expected ErrDryRun, got %v", err)
	}
}

func TestPatchAsset_DryRun(t *testing.T) {
	c := &Client{DryRun: true}
	_, err := c.PatchAsset(context.Background(), 1, snipeit.Asset{})
	if !errors.Is(err, ErrDryRun) {
		t.Errorf("expected ErrDryRun, got %v", err)
	}
}

// --- API integration tests (with mock server) ---

func TestGetAssetBySerial(t *testing.T) {
	// The server simulates Snipe-IT's substring /byserial behaviour: it returns
	// the exact match, a case-variant, and a partial substring match.  Our
	// client must filter down to only the exact (case-insensitive) rows.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/hardware/byserial/TESTSERIAL1" {
			http.NotFound(w, r)
			return
		}
		resp := map[string]any{
			"total": 3,
			"rows": []map[string]any{
				{"id": 42, "name": "Test Asset", "serial": "TESTSERIAL1"},
				{"id": 43, "name": "Case Variant", "serial": "testserial1"},
				{"id": 44, "name": "Substring Match", "serial": "TESTSERIAL10"},
			},
		}
		json.NewEncoder(w).Encode(resp)
	})

	c := newTestClient(t, handler)
	resp, err := c.GetAssetBySerial(context.Background(), "TESTSERIAL1")
	if err != nil {
		t.Fatal(err)
	}
	// Substring row (id=44) must be excluded; exact + case-variant remain.
	if resp.Total != 2 {
		t.Fatalf("expected 2 exact matches, got %d", resp.Total)
	}
	if len(resp.Rows) != 2 {
		t.Fatalf("expected 2 filtered rows, got %d", len(resp.Rows))
	}
	for _, row := range resp.Rows {
		if row.ID != 42 && row.ID != 43 {
			t.Errorf("unexpected asset ID %d in results", row.ID)
		}
	}
}

// TestGetAssetBySerial_RetriesOn429 is a regression test for the sync run
// where GetAssetBySerial started failing outright on Snipe-IT rate limits.
// That happened because NewClient sets DisableRetries at the client level
// (to stop non-idempotent writes from being retried after a network error --
// see NewClient), which also silently disabled the upstream library's
// automatic retry-on-429 for this safe, idempotent GET. GetAssetBySerial
// must retry 429s itself instead of surfacing them as a failed lookup.
func TestGetAssetBySerial_RetriesOn429(t *testing.T) {
	var requestCount int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if requestCount == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		resp := map[string]any{
			"total": 1,
			"rows": []map[string]any{
				{"id": 1, "name": "Test Asset", "serial": "RATELIMITED1"},
			},
		}
		json.NewEncoder(w).Encode(resp)
	})

	c := newTestClient(t, handler)
	resp, err := c.GetAssetBySerial(context.Background(), "RATELIMITED1")
	if err != nil {
		t.Fatalf("GetAssetBySerial: %v", err)
	}
	if requestCount != 2 {
		t.Fatalf("expected 2 requests (1 rate-limited + 1 retry), got %d", requestCount)
	}
	if resp.Total != 1 || len(resp.Rows) != 1 || resp.Rows[0].ID != 1 {
		t.Errorf("unexpected response after retry: %+v", resp)
	}
}

// TestGetAssetBySerial_NonRateLimitErrorNotRetried verifies that only 429
// responses trigger a retry -- a plain 404/500 fails immediately, matching
// the upstream library's non-retry behavior for anything else.
func TestGetAssetBySerial_NonRateLimitErrorNotRetried(t *testing.T) {
	var requestCount int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		http.Error(w, "server error", http.StatusInternalServerError)
	})

	c := newTestClient(t, handler)
	_, err := c.GetAssetBySerial(context.Background(), "SERIAL1")
	if err == nil {
		t.Fatal("expected an error for a 500 response")
	}
	if requestCount != 1 {
		t.Errorf("expected exactly 1 request (no retry for non-429 errors), got %d", requestCount)
	}
}

func TestCreateAsset_Success(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "expected POST", http.StatusMethodNotAllowed)
			return
		}
		resp := map[string]any{
			"status":   "success",
			"messages": "Asset created",
			"payload":  map[string]any{"id": 100, "name": "New Asset"},
		}
		json.NewEncoder(w).Encode(resp)
	})

	c := newTestClient(t, handler)
	asset, err := c.CreateAsset(context.Background(), snipeit.Asset{})
	if err != nil {
		t.Fatal(err)
	}
	if asset.ID != 100 {
		t.Errorf("expected asset ID 100, got %d", asset.ID)
	}
}

func TestCreateAsset_ValidationError(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"status":   "error",
			"messages": "Validation failed",
		}
		json.NewEncoder(w).Encode(resp)
	})

	c := newTestClient(t, handler)
	_, err := c.CreateAsset(context.Background(), snipeit.Asset{})
	if err == nil {
		t.Error("expected error for validation failure")
	}
}

func TestListAllModels_Pagination(t *testing.T) {
	callCount := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var resp map[string]any
		if callCount == 1 {
			resp = map[string]any{
				"total": 3,
				"rows": []map[string]any{
					{"id": 1, "name": "Model 1"},
					{"id": 2, "name": "Model 2"},
				},
			}
		} else {
			resp = map[string]any{
				"total": 3,
				"rows": []map[string]any{
					{"id": 3, "name": "Model 3"},
				},
			}
		}
		json.NewEncoder(w).Encode(resp)
	})

	c := newTestClient(t, handler)
	models, err := c.ListAllModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 {
		t.Errorf("expected 3 models, got %d", len(models))
	}
	if callCount != 2 {
		t.Errorf("expected 2 API calls for pagination, got %d", callCount)
	}
}

func TestUpdateModelFieldset_PreservesModel(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/models/54" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["name"] != "MacBook Pro" || body["category_id"] != float64(3) || body["manufacturer_id"] != float64(1) || body["fieldset_id"] != float64(2) {
			t.Fatalf("unexpected update payload: %#v", body)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"status":  "success",
			"payload": map[string]any{"id": 54, "name": "MacBook Pro", "fieldset_id": 2},
		})
	})

	c := newTestClient(t, handler)
	model := snipeit.Model{
		CommonFields: snipeit.CommonFields{ID: 54, Name: "MacBook Pro"},
		Category:     snipeit.Category{CommonFields: snipeit.CommonFields{ID: 3}},
		Manufacturer: snipeit.Manufacturer{CommonFields: snipeit.CommonFields{ID: 1}},
	}
	updated, err := c.UpdateModelFieldset(context.Background(), model, 2)
	if err != nil {
		t.Fatal(err)
	}
	if updated.FieldsetID != 2 {
		t.Fatalf("expected fieldset 2, got %d", updated.FieldsetID)
	}
}

func TestListAllAssets_Pagination(t *testing.T) {
	callCount := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var resp map[string]any
		if callCount == 1 {
			resp = map[string]any{
				"total": 3,
				"rows": []map[string]any{
					{"id": 1, "name": "Asset 1", "serial": "SERIAL1"},
					{"id": 2, "name": "Asset 2", "serial": "SERIAL2"},
				},
			}
		} else {
			resp = map[string]any{
				"total": 3,
				"rows": []map[string]any{
					{"id": 3, "name": "Asset 3", "serial": "SERIAL3"},
				},
			}
		}
		json.NewEncoder(w).Encode(resp)
	})

	c := newTestClient(t, handler)
	assets, err := c.ListAllAssets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 3 {
		t.Errorf("expected 3 assets, got %d", len(assets))
	}
	if callCount != 2 {
		t.Errorf("expected 2 API calls for pagination, got %d", callCount)
	}
}

// TestPatchAsset_RetriesOn429 verifies that createOrUpdateAsset (used by
// both CreateAsset and PatchAsset) retries on a 429 the same way
// GetAssetBySerial does. This is safe even though it's a write: a 429 means
// Snipe-IT's rate limiter rejected the request before it reached the
// create/update logic, so nothing was written -- unlike a generic network
// error, where the original request might already have been processed (the
// scenario DisableRetries protects against; see NewClient).
func TestPatchAsset_RetriesOn429(t *testing.T) {
	var requestCount int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if requestCount == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		resp := map[string]any{
			"status":   "success",
			"messages": "Asset updated",
			"payload":  map[string]any{"id": 7},
		}
		json.NewEncoder(w).Encode(resp)
	})

	c := newTestClient(t, handler)
	asset, err := c.PatchAsset(context.Background(), 7, snipeit.Asset{})
	if err != nil {
		t.Fatalf("PatchAsset: %v", err)
	}
	if requestCount != 2 {
		t.Fatalf("expected 2 requests (1 rate-limited + 1 retry), got %d", requestCount)
	}
	if asset.ID != 7 {
		t.Errorf("asset.ID = %d, want 7", asset.ID)
	}
}

// TestPatchAsset_StringFieldsetID is a regression test for Snipe-IT returning
// the PATCH response's embedded payload.model.fieldset_id as a JSON string
// instead of a number. The upstream go-snipeit library declares
// Model.FieldsetID as a plain int, so decoding straight into its
// AssetCreateResponse type (as Assets.PatchContext does) fails with "cannot
// unmarshal string into Go struct field ...fieldset_id of type int" even
// though Snipe-IT applied the update successfully. PatchAsset must not use
// that decode path.
func TestPatchAsset_StringFieldsetID(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			http.Error(w, "expected PATCH", http.StatusMethodNotAllowed)
			return
		}
		// Raw JSON so fieldset_id is a quoted string, matching what Dan's
		// Snipe-IT instance actually returned.
		fmt.Fprint(w, `{
			"status": "success",
			"messages": "Asset updated",
			"payload": {
				"id": 5,
				"model": {"id": 12, "name": "MacBook Pro", "fieldset_id": "1"}
			}
		}`)
	})

	c := newTestClient(t, handler)
	asset, err := c.PatchAsset(context.Background(), 5, snipeit.Asset{})
	if err != nil {
		t.Fatalf("PatchAsset: %v", err)
	}
	if asset.ID != 5 {
		t.Errorf("asset.ID = %d, want 5", asset.ID)
	}
}

// TestCreateAsset_StringFieldsetID is the create-path counterpart of
// TestPatchAsset_StringFieldsetID -- CreateAsset goes through the same
// lenient decode and must not choke on a string fieldset_id either.
func TestCreateAsset_StringFieldsetID(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "expected POST", http.StatusMethodNotAllowed)
			return
		}
		fmt.Fprint(w, `{
			"status": "success",
			"messages": "Asset created",
			"payload": {
				"id": 101,
				"model": {"id": 12, "name": "MacBook Pro", "fieldset_id": "1"}
			}
		}`)
	})

	c := newTestClient(t, handler)
	asset, err := c.CreateAsset(context.Background(), snipeit.Asset{})
	if err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}
	if asset.ID != 101 {
		t.Errorf("asset.ID = %d, want 101", asset.ID)
	}
}

// patchRecorder captures the parsed body of each PATCH request to /api/v1/hardware/<id>.
type patchRecorder struct {
	bodies []map[string]any
}

func (pr *patchRecorder) handler(t *testing.T, responses []map[string]any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			http.Error(w, "expected PATCH", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading request body: %v", err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("parsing request body %q: %v", string(body), err)
		}
		pr.bodies = append(pr.bodies, parsed)
		if len(pr.bodies) > len(responses) {
			t.Fatalf("more requests (%d) than responses (%d)", len(pr.bodies), len(responses))
		}
		json.NewEncoder(w).Encode(responses[len(pr.bodies)-1])
	}
}

// TestPatchAsset_RetryStripsFieldsetMissingField verifies that fields rejected
// for not being in the asset model's fieldset are removed from the retry body.
func TestPatchAsset_RetryStripsFieldsetMissingField(t *testing.T) {
	pr := &patchRecorder{}
	errMsg := map[string][]string{
		"_snipeit_extra_5": {"extra is not available on this Asset Model's fieldset."},
	}
	errJSON, _ := json.Marshal(errMsg)
	c := newTestClient(t, pr.handler(t, []map[string]any{
		{"status": "error", "messages": string(errJSON)},
		{"status": "success", "messages": "OK", "payload": map[string]any{"id": 1}},
	}))

	asset := snipeit.Asset{}
	asset.CustomFields = map[string]string{
		"_snipeit_extra_5": "should-be-stripped",
		"_snipeit_keep_6":  "kept",
	}
	if _, err := c.PatchAsset(context.Background(), 1, asset); err != nil {
		t.Fatalf("PatchAsset: %v", err)
	}
	if len(pr.bodies) != 2 {
		t.Fatalf("expected 2 PATCH calls, got %d", len(pr.bodies))
	}
	if _, ok := pr.bodies[1]["_snipeit_extra_5"]; ok {
		t.Errorf("retry body still contains stripped field: %v", pr.bodies[1])
	}
	if pr.bodies[1]["_snipeit_keep_6"] != "kept" {
		t.Errorf("retry body lost preserved field: %v", pr.bodies[1])
	}
}

// TestPatchAsset_RetryClearsInvalidValue verifies that fields rejected with
// "is invalid." (value not in the field's allowed options) are sent with an
// empty value on retry so Snipe-IT clears the stored bad value. Stripping
// alone does not work because Snipe-IT re-validates stored custom field
// values on every PATCH against the current field definition.
func TestPatchAsset_RetryClearsInvalidValue(t *testing.T) {
	pr := &patchRecorder{}
	errMsg := map[string][]string{
		"_snipeit_axm_applecare_payment_type_12": {" snipeit axm applecare payment type 12 is invalid."},
	}
	errJSON, _ := json.Marshal(errMsg)
	c := newTestClient(t, pr.handler(t, []map[string]any{
		{"status": "error", "messages": string(errJSON)},
		{"status": "success", "messages": "OK", "payload": map[string]any{"id": 429}},
	}))

	asset := snipeit.Asset{}
	asset.CustomFields = map[string]string{
		"_snipeit_axm_applecare_payment_type_12": "Subscription",
		"_snipeit_other_1":                       "other",
	}
	if _, err := c.PatchAsset(context.Background(), 429, asset); err != nil {
		t.Fatalf("PatchAsset: %v", err)
	}
	if len(pr.bodies) != 2 {
		t.Fatalf("expected 2 PATCH calls, got %d", len(pr.bodies))
	}
	val, ok := pr.bodies[1]["_snipeit_axm_applecare_payment_type_12"]
	if !ok {
		t.Fatalf("retry body omits invalid-value field; want it sent with empty value to clear stored value: %v", pr.bodies[1])
	}
	if val != "" {
		t.Errorf("retry body should clear the field with empty string, got %q", val)
	}
	if pr.bodies[1]["_snipeit_other_1"] != "other" {
		t.Errorf("retry body lost preserved field: %v", pr.bodies[1])
	}
}

// TestPatchAsset_RetryMixedErrors verifies that a single error response
// containing both "fieldset missing" and "is invalid" rejections triggers
// the correct remediation per field.
func TestPatchAsset_RetryMixedErrors(t *testing.T) {
	pr := &patchRecorder{}
	errMsg := map[string][]string{
		"_snipeit_strip_1": {"strip is not available on this Asset Model's fieldset."},
		"_snipeit_clear_2": {" snipeit clear 2 is invalid."},
	}
	errJSON, _ := json.Marshal(errMsg)
	c := newTestClient(t, pr.handler(t, []map[string]any{
		{"status": "error", "messages": string(errJSON)},
		{"status": "success", "messages": "OK", "payload": map[string]any{"id": 1}},
	}))

	asset := snipeit.Asset{}
	asset.CustomFields = map[string]string{
		"_snipeit_strip_1": "x",
		"_snipeit_clear_2": "BadValue",
	}
	if _, err := c.PatchAsset(context.Background(), 1, asset); err != nil {
		t.Fatalf("PatchAsset: %v", err)
	}
	if _, ok := pr.bodies[1]["_snipeit_strip_1"]; ok {
		t.Errorf("retry body still contains stripped field: %v", pr.bodies[1])
	}
	val, ok := pr.bodies[1]["_snipeit_clear_2"]
	if !ok || val != "" {
		t.Errorf("retry body should clear _snipeit_clear_2 with empty string, got %v (present=%v)", val, ok)
	}
}

func TestListAllSuppliers_SinglePage(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"total": 2,
			"rows": []map[string]any{
				{"id": 1, "name": "Supplier 1"},
				{"id": 2, "name": "Supplier 2"},
			},
		}
		json.NewEncoder(w).Encode(resp)
	})

	c := newTestClient(t, handler)
	suppliers, err := c.ListAllSuppliers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(suppliers) != 2 {
		t.Errorf("expected 2 suppliers, got %d", len(suppliers))
	}
}
