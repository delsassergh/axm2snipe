// Package snipe wraps the go-snipeit library with dry-run enforcement and
// convenience methods for the axm2snipe sync process.
package snipe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	snipeit "github.com/michellepellon/go-snipeit"
	"github.com/sirupsen/logrus"
)

var log = logrus.New()

// SetLogLevel sets the logger level for the snipe package.
func SetLogLevel(level logrus.Level) {
	log.SetLevel(level)
}

// SetLogFormatter sets the logger formatter for the snipe package.
func SetLogFormatter(formatter logrus.Formatter) {
	log.SetFormatter(formatter)
}

// SetLogOutput sets the logger output for the snipe package.
func SetLogOutput(output io.Writer) {
	log.SetOutput(output)
}

// ErrDryRun is returned when a write operation is attempted in dry-run mode.
var ErrDryRun = fmt.Errorf("write blocked: dry-run mode is enabled")

// Client wraps the go-snipeit client with dry-run enforcement.
type Client struct {
	*snipeit.Client
	DryRun bool
}

// snipeLogger adapts the package-level logrus logger to the snipeit.Logger interface.
type snipeLogger struct{}

func (l *snipeLogger) LogRequest(method, url string, body []byte) {
	log.WithFields(logrus.Fields{"method": method, "url": url}).Debug("snipe-it request")
}

func (l *snipeLogger) LogResponse(method, url string, statusCode int, body []byte) {
	log.WithFields(logrus.Fields{"method": method, "url": url, "status": statusCode}).Debug("snipe-it response")
}

// NewClient creates a new Snipe-IT client.
// When rateLimit is true, a token bucket rate limiter (2 req/s, burst 5) is applied.
func NewClient(baseURL, apiKey string, rateLimit bool) (*Client, error) {
	baseURL = strings.TrimRight(baseURL, "/")

	opts := &snipeit.ClientOptions{
		Logger: &snipeLogger{},
		// The upstream library's default retry policy retries ANY transient
		// network error, including on POST/PUT/PATCH — not just safe,
		// idempotent requests. That's dangerous for asset/model creation: if
		// Snipe-IT successfully processes a create but the response is lost
		// in transit, the library silently resends the same create, which
		// then fails on the now-duplicate asset_tag/name even though the
		// original write already succeeded. axm2snipe's own per-device loop
		// already tolerates a failed device gracefully (logs it and moves on,
		// retried on the next run), so there's no upside to retrying here —
		// only the risk of double-submitting non-idempotent writes.
		DisableRetries: true,
	}
	if rateLimit {
		opts.RateLimiter = snipeit.NewTokenBucketRateLimiter(2, 5)
	}

	sc, err := snipeit.NewClientWithOptions(baseURL, apiKey, opts)
	if err != nil {
		return nil, fmt.Errorf("creating snipe-it client: %w", err)
	}

	return &Client{Client: sc}, nil
}

// ListAllModels returns all models from Snipe-IT, handling pagination.
func (c *Client) ListAllModels(ctx context.Context) ([]snipeit.Model, error) {
	var all []snipeit.Model
	offset := 0
	limit := 500

	for {
		resp, _, err := c.Models.ListContext(ctx, &snipeit.ListOptions{Limit: limit, Offset: offset})
		if err != nil {
			return nil, fmt.Errorf("listing models: %w", err)
		}
		all = append(all, resp.Rows...)
		if len(all) >= resp.Total {
			break
		}
		offset += limit
	}

	return all, nil
}

// CreateModel creates a new asset model in Snipe-IT.
func (c *Client) CreateModel(ctx context.Context, model snipeit.Model) (*snipeit.Model, error) {
	if c.DryRun {
		return nil, ErrDryRun
	}

	if model.Image == "" {
		resp, _, err := c.Models.CreateContext(ctx, model)
		if err != nil {
			return nil, fmt.Errorf("creating model: %w", err)
		}
		if resp.Status != "success" {
			return nil, fmt.Errorf("creating model failed: %s", resp.Message)
		}
		return &resp.Payload, nil
	}

	// model.Image is set: snipeit.Model.MarshalJSON flattens Category/
	// Manufacturer to *_id fields for the write API but does not carry the
	// Image field through, so a plain CreateContext call silently drops any
	// image data with no error. Work around it by re-marshaling through the
	// same (buggy) MarshalJSON to get the correct flattened payload, then
	// patching the image back in before sending it ourselves.
	body, err := json.Marshal(model)
	if err != nil {
		return nil, fmt.Errorf("marshaling model: %w", err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("marshaling model: %w", err)
	}
	payload["image"] = model.Image

	req, err := c.NewRequest(http.MethodPost, "api/v1/models", payload)
	if err != nil {
		return nil, fmt.Errorf("creating model: %w", err)
	}
	var resp snipeit.ModelResponse
	if _, err := c.DoContext(ctx, req, &resp); err != nil {
		return nil, fmt.Errorf("creating model: %w", err)
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("creating model failed: %s", resp.Message)
	}
	return &resp.Payload, nil
}

// UpdateModelImage sets (or replaces) the image on an existing model,
// preserving all its other fields. Like CreateModel, it works around
// snipeit.Model.MarshalJSON dropping the Image field on writes: it builds the
// payload from the model's own (correctly-flattened) marshaled fields and
// patches image back in before sending, rather than letting the upstream
// UpdateContext silently drop it.
func (c *Client) UpdateModelImage(ctx context.Context, model snipeit.Model, image string) (*snipeit.Model, error) {
	if c.DryRun {
		return nil, ErrDryRun
	}

	body, err := json.Marshal(model)
	if err != nil {
		return nil, fmt.Errorf("marshaling model: %w", err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("marshaling model: %w", err)
	}
	payload["image"] = image

	req, err := c.NewRequest(http.MethodPut, fmt.Sprintf("api/v1/models/%d", model.ID), payload)
	if err != nil {
		return nil, fmt.Errorf("updating model image: %w", err)
	}
	var resp snipeit.ModelResponse
	if _, err := c.DoContext(ctx, req, &resp); err != nil {
		return nil, fmt.Errorf("updating model image: %w", err)
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("updating model image failed: %s", resp.Message)
	}
	return &resp.Payload, nil
}

// ListAllAssets returns all hardware assets from Snipe-IT, handling
// pagination. Used to build an in-memory serial lookup once per sync run
// instead of calling GetAssetBySerial once per device — see
// sync.Engine.loadAssets for why.
func (c *Client) ListAllAssets(ctx context.Context) ([]snipeit.Asset, error) {
	var all []snipeit.Asset
	offset := 0
	limit := 500

	for {
		resp, _, err := c.Assets.ListContext(ctx, &snipeit.ListOptions{Limit: limit, Offset: offset})
		if err != nil {
			return nil, fmt.Errorf("listing assets: %w", err)
		}
		all = append(all, resp.Rows...)
		if len(all) >= resp.Total {
			break
		}
		offset += limit
	}

	return all, nil
}

// ListAllSuppliers returns all suppliers from Snipe-IT, handling pagination.
func (c *Client) ListAllSuppliers(ctx context.Context) ([]snipeit.Supplier, error) {
	var all []snipeit.Supplier
	offset := 0
	limit := 500

	for {
		resp, _, err := c.Suppliers.ListContext(ctx, &snipeit.ListOptions{Limit: limit, Offset: offset})
		if err != nil {
			return nil, fmt.Errorf("listing suppliers: %w", err)
		}
		all = append(all, resp.Rows...)
		if len(all) >= resp.Total {
			break
		}
		offset += limit
	}

	return all, nil
}

// CreateSupplier creates a new supplier in Snipe-IT.
func (c *Client) CreateSupplier(ctx context.Context, name string) (*snipeit.Supplier, error) {
	if c.DryRun {
		return nil, ErrDryRun
	}
	supplier := snipeit.Supplier{}
	supplier.Name = name
	resp, _, err := c.Suppliers.CreateContext(ctx, supplier)
	if err != nil {
		return nil, fmt.Errorf("creating supplier: %w", err)
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("creating supplier failed: %s", resp.Message)
	}
	return &resp.Payload, nil
}

// retryOn429 calls fn, retrying with backoff (honoring a Retry-After header
// when Snipe-IT sends one) if it fails with a 429 (rate limited) response.
//
// This is deliberately narrower than the upstream library's retry policy,
// which NewClient disables client-wide (DisableRetries: true) because
// blindly retrying on ANY transient network error is unsafe for writes: if
// Snipe-IT actually processed a create but the response was lost in
// transit, retrying resends the same create and collides with the
// now-duplicate asset_tag (the original "asset_tag must be unique" bug).
// A 429, however, means Snipe-IT's rate-limit middleware rejected the
// request before it ever reached the create/update logic -- nothing was
// written, so retrying it carries none of that risk, for reads or writes.
// There's no way to express "retry only on 429" using the upstream policy
// once DisableRetries is set client-wide, so callers that need it (byserial
// lookups, asset create/update) use this instead.
func retryOn429[T any](ctx context.Context, fields logrus.Fields, fn func() (T, error)) (T, error) {
	const maxAttempts = 5
	backoff := 2 * time.Second

	var result T
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		result, err = fn()
		if err == nil {
			return result, nil
		}
		wait, retryable := rateLimitRetryAfter(err)
		if !retryable || attempt == maxAttempts {
			return result, err
		}
		if wait <= 0 {
			wait = backoff
		}
		f := logrus.Fields{"attempt": attempt, "wait": wait}
		for k, v := range fields {
			f[k] = v
		}
		log.WithFields(f).Warn("Snipe-IT rate-limited request, retrying")
		select {
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		case <-time.After(wait):
		}
		backoff *= 2
	}
	return result, err
}

// rateLimitRetryAfter reports whether err represents a 429 (rate limited)
// response from Snipe-IT and, if so, how long to wait before retrying,
// honoring a Retry-After header when Snipe-IT sends one. The caller falls
// back to its own backoff when no header is present.
func rateLimitRetryAfter(err error) (time.Duration, bool) {
	var se *snipeit.ErrorResponse
	if !errors.As(err, &se) || se.Response == nil || se.Response.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}
	if ra := se.Response.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second, true
		}
	}
	return 0, true
}

// GetAssetBySerial looks up an asset by serial number, retrying automatically
// on a 429 (rate limited) response from Snipe-IT (see retryOn429).
// The Snipe-IT /byserial endpoint performs a partial search, so we filter
// the results to exact case-insensitive matches to avoid updating the wrong asset.
//
// Called once per device on every sync -- thousands of times on a full run
// -- making it by far the most likely request to trip Snipe-IT's own API
// rate limit. sync.Engine.loadAssets avoids most of these calls entirely by
// preloading all assets once per run; this remains the fallback path (e.g.
// for RunSingle, which only ever processes one device).
func (c *Client) GetAssetBySerial(ctx context.Context, serial string) (*snipeit.AssetsResponse, error) {
	resp, err := retryOn429(ctx, logrus.Fields{"serial": serial, "request": "byserial lookup"}, func() (*snipeit.AssetsResponse, error) {
		resp, _, err := c.Assets.GetAssetBySerialContext(ctx, serial)
		return resp, err
	})
	if err != nil {
		return nil, fmt.Errorf("looking up serial %s: %w", serial, err)
	}

	// Filter to exact matches only.
	exact := resp.Rows[:0]
	for _, a := range resp.Rows {
		if strings.EqualFold(a.Serial, serial) {
			exact = append(exact, a)
		}
	}
	resp.Rows = exact
	resp.Total = len(exact)
	return resp, nil
}

// assetWriteResponse mirrors the top-level shape of snipeit.AssetCreateResponse
// (used for asset create/update/patch responses), but decodes the payload
// leniently. Snipe-IT sometimes returns the embedded payload.model.fieldset_id
// as a JSON string instead of a number; the upstream go-snipeit library
// declares Model.FieldsetID as a plain int (unlike EOL/WarrantyMonths, which
// use its own FlexInt for exactly this kind of inconsistency), so decoding
// straight into snipeit.AssetCreateResponse — as Assets.CreateContext/
// UpdateContext/PatchContext do internally — fails with "cannot unmarshal
// string into Go struct field ...fieldset_id of type int", even though the
// write itself succeeded on Snipe-IT's side. axm2snipe never uses the
// returned asset for anything beyond its ID (both createAsset and
// updateAsset in sync/sync.go discard it), so we decode only that much and
// never touch the broken field.
type assetWriteResponse struct {
	Status  string              `json:"status"`
	Message snipeit.FlexMessage `json:"messages,omitempty"`
	Payload struct {
		ID int `json:"id"`
	} `json:"payload"`
}

// createOrUpdateAsset sends the given method/URL/asset and decodes the
// response via assetWriteResponse, bypassing the upstream library's
// AssetsService methods (see assetWriteResponse for why). Retries on a 429
// from Snipe-IT (see retryOn429) -- safe even for this non-idempotent write,
// because a 429 means Snipe-IT rejected the request before processing it.
func (c *Client) createOrUpdateAsset(ctx context.Context, method, url string, asset snipeit.Asset) (*assetWriteResponse, error) {
	return retryOn429(ctx, logrus.Fields{"method": method, "url": url}, func() (*assetWriteResponse, error) {
		req, err := c.NewRequest(method, url, asset)
		if err != nil {
			return nil, err
		}
		var resp assetWriteResponse
		if _, err := c.DoContext(ctx, req, &resp); err != nil {
			return nil, err
		}
		return &resp, nil
	})
}

// CreateAsset creates a new hardware asset.
func (c *Client) CreateAsset(ctx context.Context, asset snipeit.Asset) (*snipeit.Asset, error) {
	if c.DryRun {
		return nil, ErrDryRun
	}
	resp, err := c.createOrUpdateAsset(ctx, http.MethodPost, "api/v1/hardware", asset)
	if err != nil {
		return nil, fmt.Errorf("creating asset: %w", err)
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("creating asset failed: %s", resp.Message)
	}
	return &snipeit.Asset{CommonFields: snipeit.CommonFields{ID: resp.Payload.ID}}, nil
}

// PatchAsset partially updates an existing hardware asset by ID.
// When Snipe-IT rejects custom fields, the update is retried once with
// per-field remediation:
//   - Fields not in the model's fieldset are stripped from the request.
//   - Fields with values rejected as "is invalid" are sent with an empty
//     value to clear the stored bad value, because Snipe-IT re-validates
//     stored custom field values on every PATCH against the current field
//     definition — simply stripping leaves the stored value intact and the
//     next PATCH would fail with the same error.
func (c *Client) PatchAsset(ctx context.Context, id int, asset snipeit.Asset) (*snipeit.Asset, error) {
	if c.DryRun {
		return nil, ErrDryRun
	}
	path := fmt.Sprintf("api/v1/hardware/%d", id)
	resp, err := c.createOrUpdateAsset(ctx, http.MethodPatch, path, asset)
	if err != nil {
		return nil, fmt.Errorf("updating asset %d: %w", id, err)
	}
	if resp.Status != "success" {
		toStrip, toClear := invalidFieldErrors(string(resp.Message))
		if (len(toStrip) > 0 || len(toClear) > 0) && asset.CustomFields != nil {
			log.WithFields(logrus.Fields{
				"asset_id":    id,
				"model_id":    asset.Model.ID,
				"model_name":  asset.Model.Name,
				"fieldset_id": asset.Model.FieldsetID,
				"strip":       toStrip,
				"clear":       toClear,
			}).Warn("Snipe-IT rejected custom fields — retrying with stripped/cleared fields. Run 'axm2snipe setup' to fix field configuration.")
			// Copy CustomFields to avoid mutating the caller's map.
			fieldsCopy := make(map[string]string, len(asset.CustomFields))
			for k, v := range asset.CustomFields {
				fieldsCopy[k] = v
			}
			for _, key := range toStrip {
				delete(fieldsCopy, key)
			}
			for _, key := range toClear {
				fieldsCopy[key] = ""
			}
			asset.CustomFields = fieldsCopy
			resp, err = c.createOrUpdateAsset(ctx, http.MethodPatch, path, asset)
			if err != nil {
				return nil, fmt.Errorf("updating asset %d: %w", id, err)
			}
			if resp.Status != "success" {
				return nil, fmt.Errorf("updating asset %d failed: %s", id, resp.Message)
			}
			return &snipeit.Asset{CommonFields: snipeit.CommonFields{ID: resp.Payload.ID}}, nil
		}
		return nil, fmt.Errorf("updating asset %d failed: %s", id, resp.Message)
	}
	return &snipeit.Asset{CommonFields: snipeit.CommonFields{ID: resp.Payload.ID}}, nil
}

// invalidFieldErrors parses a Snipe-IT validation error message and classifies
// rejected custom field keys by the remediation the caller should apply on
// retry:
//   - toStrip: "not available on this Asset Model's fieldset" — the field
//     does not belong on this asset; remove it from the request.
//   - toClear: "is invalid." — the value is not in the field's allowed
//     options; send the field with an empty value to clear the stored bad
//     value (Snipe-IT re-validates stored values on every PATCH).
func invalidFieldErrors(msg string) (toStrip, toClear []string) {
	// Message is a JSON object: {"_snipeit_foo_1": ["..."]}
	var errs map[string][]string
	if err := json.Unmarshal([]byte(msg), &errs); err != nil {
		return nil, nil
	}
fieldLoop:
	for key, msgs := range errs {
		for _, m := range msgs {
			switch {
			case strings.Contains(m, "not available on this Asset Model's fieldset"):
				toStrip = append(toStrip, key)
				continue fieldLoop
			case strings.Contains(m, "is invalid."):
				toClear = append(toClear, key)
				continue fieldLoop
			}
		}
	}
	return toStrip, toClear
}

// --- Custom fields setup ---

// FieldDef defines a custom field to create in Snipe-IT.
type FieldDef struct {
	Name        string // display name
	Element     string // form element type: text, textarea, radio, listbox, checkbox
	Format      string // validation format: ANY, DATE, BOOLEAN, etc.
	HelpText    string // help text shown to users
	FieldValues string // newline-separated list of allowed values (for radio/listbox)
}

// SetupFields creates or updates custom fields in Snipe-IT and associates them
// with the given fieldset. Returns a map of field name -> db_column_name.
func (c *Client) SetupFields(fieldsetID int, fields []FieldDef) (map[string]string, error) {
	if c.DryRun {
		return nil, ErrDryRun
	}
	existing, _, err := c.Fields.List(nil)
	if err != nil {
		return nil, fmt.Errorf("listing existing fields: %w", err)
	}
	existingByName := make(map[string]snipeit.Field)
	for _, f := range existing.Rows {
		existingByName[f.Name] = f
	}

	results := make(map[string]string)

	for _, f := range fields {
		field := snipeit.Field{}
		field.Name = f.Name
		field.Element = f.Element
		field.Format = f.Format
		field.HelpText = f.HelpText
		field.FieldValues = f.FieldValues

		var fieldID int
		var dbColumn string

		if ex, ok := existingByName[f.Name]; ok {
			resp, _, err := c.Fields.Update(ex.ID, field)
			if err != nil {
				return results, fmt.Errorf("updating field %q: %w", f.Name, err)
			}
			if resp.Status != "success" {
				return results, fmt.Errorf("updating field %q: %s", f.Name, resp.Message)
			}
			fieldID = resp.Payload.ID
			dbColumn = resp.Payload.DBColumnName
			if dbColumn == "" {
				dbColumn = ex.DBColumnName
			}
		} else {
			resp, _, err := c.Fields.Create(field)
			if err != nil {
				return results, fmt.Errorf("creating field %q: %w", f.Name, err)
			}
			if resp.Status != "success" {
				return results, fmt.Errorf("creating field %q: %s", f.Name, resp.Message)
			}
			fieldID = resp.Payload.ID
			dbColumn = resp.Payload.DBColumnName
		}

		results[f.Name] = dbColumn

		if fieldsetID > 0 {
			if _, err := c.Fields.Associate(fieldID, fieldsetID); err != nil {
				return results, fmt.Errorf("associating field %q (ID %d) with fieldset %d: %w", f.Name, fieldID, fieldsetID, err)
			}
		}
	}

	// Re-fetch to fill in any missing db_column_name values
	hasMissing := false
	for _, v := range results {
		if v == "" {
			hasMissing = true
			break
		}
	}
	if hasMissing {
		refreshed, _, err := c.Fields.List(nil)
		if err == nil {
			byName := make(map[string]string)
			for _, f := range refreshed.Rows {
				byName[f.Name] = f.DBColumnName
			}
			for name, dbCol := range results {
				if dbCol == "" {
					if col, ok := byName[name]; ok && col != "" {
						results[name] = col
					}
				}
			}
		}
	}

	return results, nil
}
