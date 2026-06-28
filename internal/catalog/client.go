// Package catalog is a thin HTTP client to the catalog API — the read
// side of the CQRS split. chino-stream calls it to map item_id → file path
// instead of querying the Postgres catalog directly.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ErrNotFound mirrors katalog-api's 404 — the item exists or it doesn't have
// an asset row; either way the caller treats it as "missing".
var ErrNotFound = errors.New("item has no playback asset")

// Client targets a katalog-api base URL (typically the in-cluster Service).
// It carries no per-user state — the caller passes the user's Bearer token
// into each call so katalog-api can apply per-tenant visibility.
type Client struct {
	baseURL string
	http    *http.Client
}

// New constructs a client. baseURL example: http://katalog-api.zaentrum.svc.cluster.local
func New(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 5 * time.Second},
	}
}

// Asset is the wire shape returned by katalog-api /api/v1/items/{id}/asset.
type Asset struct {
	Path      string `json:"path"`
	IsPrimary bool   `json:"isPrimary"`
}

// SubtitleAsset mirrors katalog-api /api/v1/subtitles/{id}/asset for
// resolving a sidecar subtitle id into the on-disk .vtt path on the
// packages PVC. Format defaults to "webvtt" but srt / ass / mov_text
// values are possible; if non-vtt is ever returned, the caller should
// transmux via ffmpeg before serving.
type SubtitleAsset struct {
	ItemID string `json:"itemId"`
	Path   string `json:"path"`
	Format string `json:"format,omitempty"`
	Lang   string `json:"lang,omitempty"`
}

// SubtitleAssetByID fetches the subtitle metadata for a sidecar id.
// Returns ErrNotFound for unknown ids.
func (c *Client) SubtitleAssetByID(ctx context.Context, subID, bearer string) (SubtitleAsset, error) {
	u := fmt.Sprintf("%s/api/v1/subtitles/%s/asset", c.baseURL, url.PathEscape(subID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return SubtitleAsset{}, err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return SubtitleAsset{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var a SubtitleAsset
		if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
			return SubtitleAsset{}, fmt.Errorf("decode: %w", err)
		}
		return a, nil
	case http.StatusNotFound:
		return SubtitleAsset{}, ErrNotFound
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return SubtitleAsset{}, fmt.Errorf("katalog-api %s: %d %s", u, resp.StatusCode, string(body))
	}
}

// PrimaryAssetPath fetches the playback asset path for an item. The bearer
// is the OIDC token the caller received from the end user; katalog-api's
// audience allowlist includes the *-web-beta clients so it validates.
func (c *Client) PrimaryAssetPath(ctx context.Context, itemID, bearer string) (string, error) {
	a, err := c.PrimaryAsset(ctx, itemID, bearer)
	if err != nil {
		return "", err
	}
	return a.Path, nil
}

// PrimaryAsset is the full-shape variant — currently only used for tests
// that want to assert isPrimary. Production code goes through
// PrimaryAssetPath.
func (c *Client) PrimaryAsset(ctx context.Context, itemID, bearer string) (Asset, error) {
	u := fmt.Sprintf("%s/api/v1/items/%s/asset", c.baseURL, url.PathEscape(itemID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Asset{}, err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return Asset{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var a Asset
		if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
			return Asset{}, fmt.Errorf("decode: %w", err)
		}
		return a, nil
	case http.StatusNotFound:
		return Asset{}, ErrNotFound
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Asset{}, fmt.Errorf("katalog-api %s: %d %s", u, resp.StatusCode, string(body))
	}
}
