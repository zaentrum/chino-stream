package play

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zaentrum/chino-stream/internal/catalog"
)

// Where a package is: katalog-api says, chino-stream never works it out.
//
// katalog-manager owns every path of the library and records each package as
// it completes, so chino-stream asks katalog-api where an item plays from
// (/api/v1/items/{id}/playback) and an extra (/api/v1/extras/{id}/playback),
// and serves the folder it is answered:
//
//   - in the library, a version's folder <item>/versions/<versionId>/, read
//     by its package.json; a newer version supersedes it, and it stays on the
//     storage for a grace (24 h) before it is removed. Its files never change.
//   - before the library, the package store's folder, read by its
//     manifest.json (the answer has no version). A title packaged again is
//     written in place there, so caches of its files are keyed by mtime.
//
// The answers are cached per item: 15 s fresh, then served stale while one
// fetch revalidates them (stale-while-revalidate, single-flight), and on a
// katalog-api error served stale for up to 10 min, so a katalog-api restart
// never stops playback. That the catalog has no such item is remembered for
// 5 s. A version folder is played once its .complete marker is visible:
// right after the packager's rename an NFS client may not see it yet (its
// attribute cache), and the version the new one superseded plays meanwhile.
// A file missing from a folder resolved (a version removed after its grace,
// an answer katalog-api has since changed) resolves the item once more.
//
// A playback session is pinned to its version: the master's URIs carry
// v=<versionId>, and every request of the session is served from that
// version while katalog-api still lists it (the current or a previous one),
// so a session started on a version superseded meanwhile ends on it. A
// version it no longer lists (removed) serves the current one.
//
// A nil *Resolver resolves no package: everything plays from the original.

const (
	// resolveFresh is how long an answer is served without asking again.
	resolveFresh = 15 * time.Second
	// resolveStaleMax is how old an answer may be served while katalog-api
	// fails to answer anew.
	resolveStaleMax = 10 * time.Minute
	// resolveNegative is how long "no such item" is remembered.
	resolveNegative = 5 * time.Second
	// resolveRefetchMin is how soon after an answer a file missing from its
	// folder asks again: at most once a second per item, whatever a player
	// asks for that is not there.
	resolveRefetchMin = time.Second
	// resolveTimeout bounds one katalog-api lookup (the client's own timeout
	// is the same).
	resolveTimeout = 5 * time.Second
	// packagedIDsTTL is how long the packaged ids are served before they are
	// asked for again (in the background: the cached ids are served
	// meanwhile).
	packagedIDsTTL = 60 * time.Second
)

// Resolver answers where the packages of items and extras are, through
// katalog-api (see the top of this file).
type Resolver struct {
	catalog *catalog.Client
	now     func() time.Time // nil: time.Now (tests stub it)

	mu     sync.Mutex
	items  map[string]*resolved[catalog.Playback]
	extras map[string]*resolved[catalog.ExtraPlayback]

	// complete holds the version folders whose .complete was seen: a version
	// folder never changes, so a marker seen once is there for good (until
	// the folder is removed, which a missing file notices).
	complete sync.Map // map[string]struct{}

	ids packagedIDs
}

// NewResolver is a Resolver asking c.
func NewResolver(c *catalog.Client) *Resolver {
	return &Resolver{
		catalog: c,
		items:   map[string]*resolved[catalog.Playback]{},
		extras:  map[string]*resolved[catalog.ExtraPlayback]{},
	}
}

func (r *Resolver) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// resolved is the cached answer of one katalog-api lookup.
type resolved[T any] struct {
	val     T
	has     bool      // val is the answer as of at
	missing bool      // katalog-api said there is no such thing, at at
	at      time.Time // when the answer (or the 404) came
	err     error     // the last lookup's failure; nil since one succeeded
	used    time.Time // when a request last asked for it (prune)
	// fetching is closed when the lookup in flight ends; nil while none is.
	fetching chan struct{}
}

// lookup answers key from m, asking fetch when the cached answer is not
// fresh: a stale one is served at once while one lookup revalidates it, an
// answer older than resolveStaleMax only when katalog-api fails to answer.
// force asks again now (a file was missing from the folder answered) unless
// the answer is less than resolveRefetchMin old. Lookups of a key are never
// run twice at a time; they run apart from the request, so a request that
// goes away does not fail the others waiting for it.
func lookup[T any](ctx context.Context, r *Resolver, m map[string]*resolved[T], key string, force bool,
	fetch func(context.Context) (T, error)) (T, error) {
	var zero T
	r.mu.Lock()
	e := m[key]
	if e == nil {
		e = &resolved[T]{}
		m[key] = e
	}
	now := r.clock()
	e.used = now
	age := now.Sub(e.at)
	if force && (e.has || e.missing) && age < resolveRefetchMin {
		force = false
	}
	switch {
	case force:
	case e.has && age < resolveFresh:
		v := e.val
		r.mu.Unlock()
		return v, nil
	case e.missing && age < resolveNegative:
		r.mu.Unlock()
		return zero, catalog.ErrNotFound
	case e.has && age < resolveStaleMax:
		startLookup(r, e, key, fetch)
		v := e.val
		r.mu.Unlock()
		return v, nil
	}
	done := startLookup(r, e, key, fetch)
	r.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
		return zero, ctx.Err()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case e.has && r.clock().Sub(e.at) < resolveStaleMax:
		return e.val, nil // the new answer, or the one before when katalog-api failed
	case e.missing:
		return zero, catalog.ErrNotFound
	case e.err != nil:
		return zero, e.err
	}
	return zero, catalog.ErrNotFound
}

// startLookup starts one lookup of e unless one is in flight, and returns
// the channel closed when it ends. The caller holds r.mu.
func startLookup[T any](r *Resolver, e *resolved[T], key string, fetch func(context.Context) (T, error)) chan struct{} {
	if e.fetching != nil {
		return e.fetching
	}
	done := make(chan struct{})
	e.fetching = done
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
		defer cancel()
		v, err := fetch(ctx)
		r.mu.Lock()
		switch {
		case err == nil:
			e.val, e.has, e.missing, e.at, e.err = v, true, false, r.clock(), nil
		case errors.Is(err, catalog.ErrNotFound):
			var zero T
			e.val, e.has, e.missing, e.at, e.err = zero, false, true, r.clock(), nil
		default:
			if e.err == nil {
				log.Printf("resolve %s: %v (serving the answer from %s while it fails)", key, err, e.at.Format(time.RFC3339))
			}
			e.err = err // the answer before, if any, stays: served stale
		}
		e.fetching = nil
		r.mu.Unlock()
		close(done)
	}()
	return done
}

// item is katalog-api's answer for the item itemID; force asks again (see
// lookup).
func (r *Resolver) item(ctx context.Context, itemID string, force bool) (catalog.Playback, error) {
	if r == nil || r.catalog == nil {
		return catalog.Playback{}, catalog.ErrNotFound
	}
	return lookup(ctx, r, r.items, itemID, force, func(ctx context.Context) (catalog.Playback, error) {
		return r.catalog.Playback(ctx, itemID)
	})
}

// extra is katalog-api's answer for the extra extraID; force asks again.
func (r *Resolver) extra(ctx context.Context, extraID string, force bool) (catalog.ExtraPlayback, error) {
	if r == nil || r.catalog == nil {
		return catalog.ExtraPlayback{}, catalog.ErrNotFound
	}
	return lookup(ctx, r, r.extras, extraID, force, func(ctx context.Context) (catalog.ExtraPlayback, error) {
		return r.catalog.ExtraPlayback(ctx, extraID)
	})
}

// prune forgets the answers no request asked for in resolveStaleMax.
func (r *Resolver) prune() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cut := r.clock().Add(-resolveStaleMax)
	for k, e := range r.items {
		if e.fetching == nil && e.used.Before(cut) {
			delete(r.items, k)
		}
	}
	for k, e := range r.extras {
		if e.fetching == nil && e.used.Before(cut) {
			delete(r.extras, k)
		}
	}
}

// pkgDir is one package folder chino-stream serves.
type pkgDir struct {
	// dir is the folder: a version's, an extra's, or the package store's.
	dir string
	// record is the file it is read by: package.json in the library,
	// manifest.json before it.
	record string
	// versionID is the version the folder is, "" before the library: the
	// v= a session is pinned to.
	versionID string
}

// path is the file rel of the package.
func (p *pkgDir) path(rel ...string) string {
	return filepath.Clean(filepath.Join(append([]string{p.dir}, rel...)...))
}

// library reports whether the package is in the library: its folder is
// written once and never changes.
func (p *pkgDir) library() bool { return p.record == recordPackage }

// The records a package folder is read by.
const (
	recordPackage  = "package.json"
	recordManifest = "manifest.json"
)

// pkgDirOf is the folder of a katalog-api package answer.
func pkgDirOf(ref catalog.PackageRef) *pkgDir {
	p := &pkgDir{dir: filepath.Clean(ref.Dir), record: ref.Record, versionID: ref.VersionID}
	if p.record != recordPackage && p.record != recordManifest {
		p.record = recordManifest
		if p.versionID != "" {
			p.record = recordPackage
		}
	}
	return p
}

// itemPackage is the package the item itemID is served from: with pin (the
// session's v=) that version while it is the current one or a previous one;
// else the current package, or while its .complete is not visible yet the one
// it superseded. nil when it has none to serve. gate requires the .complete
// marker of a package from before the library too, as its master, /info and
// /prewarm always did (a library version's is always required); its renditions'
// files are served from its folder as they are. The answer comes with it.
func (r *Resolver) itemPackage(ctx context.Context, itemID, pin string, gate bool) (*pkgDir, catalog.Playback, error) {
	a, err := r.item(ctx, itemID, false)
	if err != nil {
		return nil, a, err
	}
	return r.choose(a, pin, gate), a, nil
}

// itemPackageAgain is itemPackage once more after a file was missing from
// the folder it answered: katalog-api is asked again (at most once a second).
func (r *Resolver) itemPackageAgain(ctx context.Context, itemID, pin string, gate bool) (*pkgDir, error) {
	a, err := r.item(ctx, itemID, true)
	if err != nil {
		return nil, err
	}
	return r.choose(a, pin, gate), nil
}

// choose picks the package to serve of answer a (see itemPackage).
func (r *Resolver) choose(a catalog.Playback, pin string, gate bool) *pkgDir {
	if pin != "" {
		refs := a.Previous
		if a.Package != nil {
			refs = append([]catalog.PackageRef{*a.Package}, refs...)
		}
		for _, ref := range refs {
			if ref.VersionID == pin && r.ready(ref, gate) {
				return pkgDirOf(ref)
			}
		}
	}
	if a.Package != nil && r.ready(*a.Package, gate) {
		return pkgDirOf(*a.Package)
	}
	if len(a.Previous) > 0 && r.ready(a.Previous[0], gate) {
		return pkgDirOf(a.Previous[0])
	}
	return nil
}

// hasPackage reports whether answer a names a package at all, played or not
// (a version whose folder is missing or not complete).
func hasPackage(a catalog.Playback) bool {
	return (a.Package != nil && a.Package.Dir != "") || len(a.Previous) > 0
}

// ready reports whether the package ref may be served: a library version
// once its .complete marker is visible (remembered, the folder never
// changes); a package from before the library when gate is off, else while
// its .complete is there.
func (r *Resolver) ready(ref catalog.PackageRef, gate bool) bool {
	if ref.Dir == "" {
		return false
	}
	p := pkgDirOf(ref)
	if !p.library() && !gate {
		return true
	}
	if p.library() {
		if _, ok := r.complete.Load(p.dir); ok {
			return true
		}
	}
	st, err := os.Stat(filepath.Join(p.dir, ".complete"))
	if err != nil || st.IsDir() {
		return false
	}
	if p.library() {
		r.complete.Store(p.dir, struct{}{})
	}
	return true
}

// forgetComplete drops what is remembered of dir's .complete, when a file of
// it was found missing: the folder may be gone.
func (r *Resolver) forgetComplete(dir string) {
	if r != nil {
		r.complete.Delete(dir)
	}
}

// packagedIDs caches katalog-api's packaged ids, stale-while-revalidate.
type packagedIDs struct {
	mu         sync.Mutex
	val        []string
	at         time.Time
	refreshing bool
}

// PackagedIDs returns the ids of the movies and episodes that have a
// package, as katalog-api last said. Stale-while-revalidate: the cached ids
// are returned at once, and past packagedIDsTTL one lookup refreshes them in
// the background; before the first answer (a pod just started) none — the
// Zap feed degrades to its cold pool — while it runs. A failed lookup keeps
// the ids it had.
func (r *Resolver) PackagedIDs() []string {
	if r == nil || r.catalog == nil {
		return []string{}
	}
	c := &r.ids
	c.mu.Lock()
	defer c.mu.Unlock()
	if (c.val == nil || r.clock().Sub(c.at) >= packagedIDsTTL) && !c.refreshing {
		c.refreshing = true
		go r.refreshPackagedIDs()
	}
	if c.val == nil {
		return []string{}
	}
	return c.val
}

// refreshPackagedIDs asks katalog-api for the packaged ids and swaps them in
// (the slice is replaced, never changed in place, so a caller holding the old
// one reads it undisturbed).
func (r *Resolver) refreshPackagedIDs() {
	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()
	ids, err := r.catalog.PackagedIDs(ctx)
	c := &r.ids
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshing = false
	if err != nil {
		log.Printf("packaged ids: %v (keeping the %d known)", err, len(c.val))
		return
	}
	c.val, c.at = ids, r.clock()
}
