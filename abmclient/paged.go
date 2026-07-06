package abmclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/CampusTech/abm"
)

// PagedFetchOptions configures a paced, resumable org-devices fetch.
type PagedFetchOptions struct {
	// Fields restricts the orgDevices sparse fieldset. Empty fetches all fields.
	Fields []string
	// PageSize is the number of devices requested per page (1-1000). Zero lets
	// Apple use its own default page size.
	PageSize int
	// Delay is how long to wait before each page request after the first.
	// A zero delay issues requests back-to-back with no pacing of its own.
	Delay time.Duration
	// Resume is an absolute "next page" URL to continue from, as previously
	// returned via PagedDevicesResult.NextURL. Empty starts from page one.
	Resume string
}

// PagedDevicesResult is passed to onPage after each successfully fetched page.
type PagedDevicesResult struct {
	// Devices are the org devices returned on this page.
	Devices []abm.OrgDevice
	// NextURL is the absolute URL for the next page, or "" if this was the last page.
	NextURL string
}

// FetchDevicesPaged fetches org devices one page at a time, invoking onPage
// after each successfully fetched page so the caller can persist partial
// progress (e.g. to disk) as it goes.
//
// Unlike Client.GetAllDevices / the upstream library's FetchAllOrgDevices,
// this method:
//
//   - paces itself with a caller-controlled Delay between page requests,
//     using its own OAuth2 HTTP client rather than the upstream library's
//     fixed internal rate limiter (roughly 1 request/3s). This matters when
//     Apple's real, undocumented rate limit needs to be treated more
//     conservatively than that — e.g. because the limit appears to be shared
//     across every integration polling the same Apple Business Manager
//     organization, not just axm2snipe's own request rate.
//
//   - does not retry on failure. On error it returns the URL of the page that
//     failed (failedURL) so the caller can persist it as a resume point and
//     continue later via PagedFetchOptions.Resume, instead of re-fetching
//     pages already collected.
//
// If onPage itself returns an error (e.g. a disk write failed), FetchDevicesPaged
// returns the URL of the page that was just fetched (but not yet successfully
// processed) as failedURL, so a retry re-fetches that same page rather than
// skipping it.
func (c *Client) FetchDevicesPaged(ctx context.Context, opts PagedFetchOptions, onPage func(PagedDevicesResult) error) (failedURL string, err error) {
	current := opts.Resume
	if current == "" {
		current, err = buildOrgDevicesURL(opts.Fields, opts.PageSize)
		if err != nil {
			return "", err
		}
	}

	for first := true; current != ""; first = false {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return current, ctxErr
		}
		if !first && opts.Delay > 0 {
			select {
			case <-ctx.Done():
				return current, ctx.Err()
			case <-time.After(opts.Delay):
			}
		}

		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if reqErr != nil {
			return current, fmt.Errorf("build request: %w", reqErr)
		}
		req.Header.Set("Accept", "application/json")

		resp, doErr := c.httpClient.Do(req)
		if doErr != nil {
			return current, fmt.Errorf("request failed: %w", doErr)
		}

		payload, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return current, fmt.Errorf("read response: %w", readErr)
		}
		if resp.StatusCode != http.StatusOK {
			return current, fmt.Errorf("request failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(payload)))
		}

		var page abm.OrgDevicesResponse
		if jsonErr := json.Unmarshal(payload, &page); jsonErr != nil {
			return current, fmt.Errorf("decode response: %w", jsonErr)
		}

		if cbErr := onPage(PagedDevicesResult{Devices: page.Data, NextURL: page.Links.Next}); cbErr != nil {
			return current, cbErr
		}

		current = page.Links.Next
	}

	return "", nil
}

// buildOrgDevicesURL builds the starting URL for a fresh (non-resumed) paged
// orgDevices fetch.
func buildOrgDevicesURL(fields []string, pageSize int) (string, error) {
	base, err := url.Parse(abm.DefaultAPIBaseURL + "v1/orgDevices")
	if err != nil {
		return "", fmt.Errorf("parse base URL: %w", err)
	}
	q := url.Values{}
	if len(fields) > 0 {
		q.Set("fields[orgDevices]", strings.Join(fields, ","))
	}
	if pageSize > 0 {
		q.Set("limit", strconv.Itoa(pageSize))
	}
	base.RawQuery = q.Encode()
	return base.String(), nil
}
