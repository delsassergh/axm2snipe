package abmclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/CampusTech/abm"
)

// ReleasedDevice is one DEVICE_REMOVED_FROM_ORG audit event, flattened to the
// two things sync.go needs: which device, and when.
type ReleasedDevice struct {
	Serial        string
	EventDateTime time.Time
}

// auditEvent mirrors the subset of Apple's polymorphic AuditEvent resource
// this package cares about. Only requested via
// fields[auditEvents]=eventDateTime,eventDataDeviceRemovedFromOrg and only
// ever queried with filter[type]=DEVICE_REMOVED_FROM_ORG, so
// eventDataDeviceRemovedFromOrg is the only variant attribute needed here --
// see AuditEventDeviceRemovedFromOrg in Apple's docs for the full resource
// shape (it also carries releaseEntityId/releaseEntityType, unused here).
type auditEvent struct {
	ID         string `json:"id"`
	Attributes struct {
		EventDateTime               time.Time `json:"eventDateTime"`
		EventDataDeviceRemovedFromOrg *struct {
			SerialNumber string `json:"serialNumber"`
		} `json:"eventDataDeviceRemovedFromOrg"`
	} `json:"attributes"`
}

type auditEventsResponse struct {
	Data  []auditEvent `json:"data"`
	Links struct {
		Next string `json:"next"`
	} `json:"links"`
}

// buildAuditEventsURL builds the starting URL for a DEVICE_REMOVED_FROM_ORG
// audit events query covering [start, end].
func buildAuditEventsURL(start, end time.Time) (string, error) {
	base, err := url.Parse(abm.DefaultAPIBaseURL + "v1/auditEvents")
	if err != nil {
		return "", fmt.Errorf("parse base URL: %w", err)
	}
	q := url.Values{}
	q.Set("filter[type]", "DEVICE_REMOVED_FROM_ORG")
	q.Set("filter[startTimestamp]", start.UTC().Format(time.RFC3339))
	q.Set("filter[endTimestamp]", end.UTC().Format(time.RFC3339))
	q.Set("fields[auditEvents]", "eventDateTime,eventDataDeviceRemovedFromOrg")
	q.Set("limit", "1000")
	base.RawQuery = q.Encode()
	return base.String(), nil
}

// FetchReleasedDevices returns every device released from the org (per
// Apple's audit log) within the last `lookback` duration, keyed by serial
// number. This exists because /v1/orgDevices (the bulk device list) never
// returns released devices, no matter what fields are requested -- see
// ABMConfig.ReleaseLookbackDays for the full explanation. Unlike
// FetchDevicesPaged, this is not resumable/paced: audit events queries are
// cheap and bounded by the lookback window rather than the size of the whole
// device fleet, so a single run fetching all pages back-to-back is fine.
func (c *Client) FetchReleasedDevices(ctx context.Context, lookback time.Duration) (map[string]ReleasedDevice, error) {
	now := time.Now()
	startURL, err := buildAuditEventsURL(now.Add(-lookback), now)
	if err != nil {
		return nil, err
	}
	return c.fetchAuditEventsFrom(ctx, startURL)
}

// fetchAuditEventsFrom does the actual paginated fetch starting at startURL,
// following links.next until exhausted. Split out from FetchReleasedDevices
// so tests can point it at a local server directly -- like buildOrgDevicesURL
// / FetchDevicesPaged's Resume option, the fresh-start URL in
// FetchReleasedDevices is built against abm.DefaultAPIBaseURL, which isn't
// overridable for a local httptest server.
func (c *Client) fetchAuditEventsFrom(ctx context.Context, startURL string) (map[string]ReleasedDevice, error) {
	current := startURL
	released := make(map[string]ReleasedDevice)
	pageNum := 0
	for current != "" {
		pageNum++
		if ctxErr := ctx.Err(); ctxErr != nil {
			return released, ctxErr
		}

		log.Debugf("Fetching auditEvents page %d: %s", pageNum, current)

		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if reqErr != nil {
			return released, fmt.Errorf("build request: %w", reqErr)
		}
		req.Header.Set("Accept", "application/json")

		resp, doErr := c.httpClient.Do(req)
		if doErr != nil {
			return released, fmt.Errorf("request failed: %w", doErr)
		}

		payload, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return released, fmt.Errorf("read response: %w", readErr)
		}
		if resp.StatusCode != http.StatusOK {
			return released, fmt.Errorf("request failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(payload)))
		}

		var page auditEventsResponse
		if jsonErr := json.Unmarshal(payload, &page); jsonErr != nil {
			return released, fmt.Errorf("decode response: %w", jsonErr)
		}

		log.Debugf("auditEvents page %d: %d event(s), next=%q", pageNum, len(page.Data), page.Links.Next)

		for _, ev := range page.Data {
			if ev.Attributes.EventDataDeviceRemovedFromOrg == nil {
				continue
			}
			serial := ev.Attributes.EventDataDeviceRemovedFromOrg.SerialNumber
			if serial == "" {
				continue
			}
			// A device can be added, released, re-added, and released again;
			// keep the most recent release event if we see the same serial
			// more than once across pages.
			if existing, ok := released[serial]; !ok || ev.Attributes.EventDateTime.After(existing.EventDateTime) {
				released[serial] = ReleasedDevice{Serial: serial, EventDateTime: ev.Attributes.EventDateTime}
			}
		}

		current = page.Links.Next
	}

	return released, nil
}
