package play

import (
	"crypto/sha1"
	"encoding/hex"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// sessionParams are the query parameters whose values differ from one
// client session to the next and that a playlist only carries into its
// URIs: the credentials a client puts in a URL (stream, its stream token,
// minted per session and renewed every few hours; token, an OIDC access
// token) and where the session starts playing (t). No playlist's content
// depends on them otherwise. A parameter is one by its name as the query
// decodes it (%73tream is stream), as the auth middleware reads it.
var sessionParams = map[string]bool{"stream": true, "token": true, "t": true}

// playlistKey is what playlistCache keys an entry by: the playlist's path,
// the query with its session's values marked, and what else the body
// depends on (variant).
type playlistKey struct {
	path, query, variant string
}

// size is what an entry holds: its key's bytes and its body's.
func (k playlistKey) size(e *playlistCacheEntry) int64 {
	return int64(len(k.path) + len(k.query) + len(k.variant) + len(e.body))
}

// variantBase is what playlistVariants keys the variant cached last by.
type variantBase struct {
	path, query string
}

// templateQuery is query with the value of each session parameter replaced
// by a mark (sessionMark), and those values, in the order they stand: what
// keys playlistCache and what fillQuery puts back. Each parameter keeps its
// name as spelt. ok is false where the query is to be served as before,
// rewritten whole and kept by no cache: it holds a mark's bytes (a NUL,
// which no request's URL can), or a session's value holds another byte than
// those of a token, an OIDC token or a time (sessionValue) — a quote, say,
// which a playlist's tags would read otherwise in a body built with marks.
func templateQuery(query string) (marked string, values []string, ok bool) {
	if strings.Contains(query, "\x00") {
		return query, nil, false
	}
	if query == "" {
		return "", nil, true
	}
	parts := strings.Split(query, "&")
	for i, kv := range parts {
		k, v, found := strings.Cut(kv, "=")
		if !found {
			continue
		}
		name := k
		if u, err := url.QueryUnescape(k); err == nil {
			name = u
		}
		if !sessionParams[name] {
			continue
		}
		if !sessionValue(v) {
			return query, nil, false
		}
		parts[i] = k + "=" + sessionMark(len(values))
		values = append(values, v)
	}
	if values == nil {
		return query, nil, true
	}
	return strings.Join(parts, "&"), values, true
}

// sessionValue is whether a session's value, as the query spells it, holds
// only the bytes of a stream token (base64url and a dot), of an OIDC token
// (the same), of a time (digits, a dot) and of percent-encoding: none that
// a playlist's syntax gives a meaning.
func sessionValue(v string) bool {
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == '~', c == '%', c == '+', c == '=':
		default:
			return false
		}
	}
	return true
}

// sessionMark is the mark of the i-th session value: bytes no request's
// query holds (a NUL is %00 in a URL; templateQuery keys a query that holds
// one whole), so a mark is never taken for a part of a URI.
func sessionMark(i int) string {
	return "\x00" + strconv.Itoa(i) + "\x00"
}

// fillQuery is body with each mark templateQuery made replaced by its value:
// the bytes the playlist had, rewritten with the request's own query.
func fillQuery(body []byte, values []string) []byte {
	if len(values) == 0 {
		return body
	}
	pairs := make([]string, 0, 2*len(values))
	for i, v := range values {
		pairs = append(pairs, sessionMark(i), v)
	}
	return []byte(strings.NewReplacer(pairs...).Replace(string(body)))
}

// sessionETag is the ETag of a cached body filled with values: the cached
// body's own when there are none, else one that tells sessions apart as
// the bodies they get differ. It needs no body, so a 304 is decided before
// one is filled.
func sessionETag(etag string, values []string) string {
	if len(values) == 0 {
		return etag
	}
	h := sha1.New()
	h.Write([]byte(etag))
	for _, v := range values {
		h.Write([]byte{0})
		h.Write([]byte(v))
	}
	return `"` + hex.EncodeToString(h.Sum(nil)) + `"`
}

// storePlaylist puts e in playlistCache under key, in place of what it held
// there, and keeps the cache within playlistCacheBudget: its keys and its
// bodies.
func storePlaylist(key playlistKey, e *playlistCacheEntry) {
	e.lastTouch.Store(time.Now().UnixNano())
	delta := key.size(e)
	if old, loaded := playlistCache.Swap(key, e); loaded {
		delta -= key.size(old.(*playlistCacheEntry))
	}
	if playlistCacheBytes.Add(delta) > playlistCacheBudget {
		evictPlaylistCache()
	}
}

// dropPlaylist takes key's entry out of playlistCache.
func dropPlaylist(key playlistKey) {
	if old, loaded := playlistCache.LoadAndDelete(key); loaded {
		playlistCacheBytes.Add(-key.size(old.(*playlistCacheEntry)))
	}
}

// evictPlaylistCache drops the least recently served entries until the
// cache holds 80% of its budget, and the variants whose entry went.
func evictPlaylistCache() {
	playlistCacheMu.Lock()
	defer playlistCacheMu.Unlock()
	if playlistCacheBytes.Load() <= playlistCacheBudget {
		return
	}
	type kv struct {
		key   playlistKey
		entry *playlistCacheEntry
	}
	var all []kv
	playlistCache.Range(func(k, v any) bool {
		all = append(all, kv{k.(playlistKey), v.(*playlistCacheEntry)})
		return true
	})
	sort.Slice(all, func(i, j int) bool { return all[i].entry.lastTouch.Load() < all[j].entry.lastTouch.Load() })
	for _, x := range all {
		if playlistCacheBytes.Load() <= playlistCacheBudget*8/10 {
			break
		}
		if playlistCache.CompareAndDelete(x.key, x.entry) {
			playlistCacheBytes.Add(-x.key.size(x.entry))
			forgetVariant(x.key)
		}
	}
}

// forgetVariant drops the variant playlistVariants keeps for key's path
// and query when it is key's.
func forgetVariant(key playlistKey) {
	if key.variant != "" {
		playlistVariants.CompareAndDelete(variantBase{key.path, key.query}, key.variant)
	}
}

// pruneServeCaches drops what the serve caches hold that was not used
// since cutoff: playlistCache's entries and their variants; the versions,
// EXT-X-MAP URIs and package records kept by path (initVersionCache,
// mapURIsCache, manifestCache); and the load locks of the paths
// packagedCache holds nothing of (packagedCacheLoading). A library whose
// packages come and go would otherwise keep them for every path it ever
// served.
func pruneServeCaches(cutoff time.Time) {
	c := cutoff.UnixNano()
	playlistCache.Range(func(k, v any) bool {
		key, e := k.(playlistKey), v.(*playlistCacheEntry)
		if e.lastTouch.Load() < c && playlistCache.CompareAndDelete(key, e) {
			playlistCacheBytes.Add(-key.size(e))
			forgetVariant(key)
		}
		return true
	})
	playlistVariants.Range(func(k, v any) bool {
		base := k.(variantBase)
		if _, ok := playlistCache.Load(playlistKey{base.path, base.query, v.(string)}); !ok {
			playlistVariants.CompareAndDelete(k, v)
		}
		return true
	})
	initVersionCache.Range(func(k, v any) bool {
		if e := v.(*initVersionEntry); e.used.Load() < c {
			initVersionCache.CompareAndDelete(k, e)
		}
		return true
	})
	mapURIsCache.Range(func(k, v any) bool {
		if e := v.(*mapURIsEntry); e.used.Load() < c {
			mapURIsCache.CompareAndDelete(k, e)
		}
		return true
	})
	manifestCache.Range(func(k, v any) bool {
		if e := v.(*manifestCacheEntry); e.used.Load() < c {
			manifestCache.CompareAndDelete(k, e)
		}
		return true
	})
	// a lock is taken away only while no one holds it; one a request took a
	// moment before is let go by it, and the next request makes another:
	// at worst one more read of the same file
	packagedCacheLoading.Range(func(k, v any) bool {
		if _, cached := packagedCache.Load(k); cached {
			return true
		}
		if mu := v.(interface {
			TryLock() bool
			Unlock()
		}); mu.TryLock() {
			packagedCacheLoading.CompareAndDelete(k, v)
			mu.Unlock()
		}
		return true
	})
}
