// Package snipe wraps the go-snipeit library with dry-run enforcement and
// convenience methods for the axm2snipe sync process.
package snipe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

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
	resp, _, err := c.Models.CreateContext(ctx, model)
	if err != nil {
		return nil, fmt.Errorf("creating model: %w", err)
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("creating model failed: %s", resp.Message)
	}
	return &resp.Payload, nil
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

// GetAssetBySerial looks up an asset by serial number.
// The Snipe-IT /byserial endpoint performs a partial search, so we filter
// the results to exact case-insensitive matches to avoid updating the wrong asset.
func (c *Client) GetAssetBySerial(ctx context.Context, serial string) (*snipeit.AssetsResponse, error) {
	resp, _, err := c.Assets.GetAssetBySerialContext(ctx, serial)
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

// CreateAsset creates a new hardware asset.
func (c *Client) CreateAsset(ctx context.Context, asset snipeit.Asset) (*snipeit.Asset, error) {
	if c.DryRun {
		return nil, ErrDryRun
	}
	resp, _, err := c.Assets.CreateContext(ctx, asset)
	if err != nil {
		return nil, fmt.Errorf("creating asset: %w", err)
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("creating asset failed: %s", resp.Message)
	}
	return &resp.Payload, nil
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
	resp, _, err := c.Assets.PatchContext(ctx, id, asset)
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
			resp, _, err = c.Assets.PatchContext(ctx, id, asset)
			if err != nil {
				return nil, fmt.Errorf("updating asset %d: %w", id, err)
			}
			if resp.Status != "success" {
				return nil, fmt.Errorf("updating asset %d failed: %s", id, resp.Message)
			}
			return &resp.Payload, nil
		}
		return nil, fmt.Errorf("updating asset %d failed: %s", id, resp.Message)
	}
	return &resp.Payload, nil
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
