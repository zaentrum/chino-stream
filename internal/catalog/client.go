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
	var a Asset
	err := c.getJSON(ctx, fmt.Sprintf("/api/v1/items/%s/asset", url.PathEscape(itemID)), bearer, &a)
	return a, err
}

// Playback is katalog-api's /api/v1/items/{id}/playback: where an item plays
// from. Package is the package that plays — the item's current version in
// the library, else the folder of its packaged asset (a package from before
// the library) — nil when it has none, as a series never has. Previous are
// its superseded versions not removed yet, the one superseded last first: a
// session started on one of them is served from it to its end. Original is
// the file it was taken in from, nil once that is retired.
type Playback struct {
	ItemID   string       `json:"itemId"`
	Type     string       `json:"type"`
	Package  *PackageRef  `json:"package"`
	Previous []PackageRef `json:"previous"`
	Original *Original    `json:"original"`
}

// PackageRef is one package folder and the record it is read by. VersionID
// is "" (null on the wire) for a package from before the library, which is
// read by its manifest.json and has nothing to pin a session to;
// CompletedAt is zero where katalog-api omits it (a previous version, a
// package from before the library).
type PackageRef struct {
	VersionID   string    `json:"versionId"`
	Dir         string    `json:"dir"`
	Record      string    `json:"record"` // "package.json" | "manifest.json"
	CompletedAt time.Time `json:"completedAt"`
}

// Original is the file an item was taken in from; SourceID is "" before the
// library.
type Original struct {
	Path     string `json:"path"`
	SourceID string `json:"sourceId"`
}

// ExtraPlayback is katalog-api's /api/v1/extras/{extraId}/playback: where a
// packaged extra's package is, and the title it belongs to (ItemID).
type ExtraPlayback struct {
	ExtraID    string    `json:"extraId"`
	ItemID     string    `json:"itemId"`
	Dir        string    `json:"dir"`
	Record     string    `json:"record"` // "package.json" | "manifest.json"
	PackagedAt time.Time `json:"packagedAt"`
}

// Playback asks katalog-api where the item itemID plays from. ErrNotFound:
// the catalog has no such item. No bearer: the route trusts the cluster
// network, as /asset does.
func (c *Client) Playback(ctx context.Context, itemID string) (Playback, error) {
	var p Playback
	err := c.getJSON(ctx, fmt.Sprintf("/api/v1/items/%s/playback", url.PathEscape(itemID)), "", &p)
	return p, err
}

// ExtraPlayback asks katalog-api where the package of the extra extraID is.
// ErrNotFound: no such extra, or one not packaged or removed.
func (c *Client) ExtraPlayback(ctx context.Context, extraID string) (ExtraPlayback, error) {
	var x ExtraPlayback
	err := c.getJSON(ctx, fmt.Sprintf("/api/v1/extras/%s/playback", url.PathEscape(extraID)), "", &x)
	return x, err
}

// PackagedIDs asks katalog-api for the ids of the movies and episodes that
// have a package.
func (c *Client) PackagedIDs(ctx context.Context) ([]string, error) {
	var out struct {
		IDs []string `json:"ids"`
	}
	if err := c.getJSON(ctx, "/api/v1/packaged-ids", "", &out); err != nil {
		return nil, err
	}
	if out.IDs == nil {
		out.IDs = []string{}
	}
	return out.IDs, nil
}

// getJSON GETs path from katalog-api and decodes its JSON answer into v:
// ErrNotFound on a 404, an error naming the URL, status and the start of the
// body on any other failure.
func (c *Client) getJSON(ctx context.Context, path, bearer string, v any) error {
	u := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			return fmt.Errorf("decode %s: %w", u, err)
		}
		return nil
	case http.StatusNotFound:
		return ErrNotFound
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("katalog-api %s: %d %s", u, resp.StatusCode, string(body))
	}
}
