package build

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jclement/mapthing/internal/catalog"
)

// SourceStatus is what SourceChanged found for a region.
type SourceStatus struct {
	// Changed: a build would put newer data for this region on the map. Either the publisher
	// has a newer extract than the one downloaded, or nothing is downloaded, or what is
	// downloaded is not what the live generation was built from (a build downloaded it and
	// then failed, or has not run since).
	Changed bool
	// Detail says how that was decided, for people.
	Detail string
}

// SourceChanged answers the scheduler's question for a region: is there data a build would
// newly put into service? It never downloads an extract.
//
// The comparison that matters is against the **live generation**, not the download cache. A
// build that fetched sequence 101 and then failed in a later stage leaves 101 in the cache and
// 100 on the map; asking only "is the cache current?" would answer "nothing new" for ever.
// So, in order:
//
//  1. nothing downloaded (or, for a custom region, not clipped) → changed;
//  2. the downloaded extract or clip is not the one in the live generation's manifest (by
//     MD5), or the region is not in the live generation, or nothing is live → changed, with
//     no request to the publisher at all;
//  3. a custom region whose parent extracts on disk are newer than its clip → changed;
//  4. the publisher has a newer extract than the one downloaded (Geofabrik's replication
//     sequence in state.txt; a conditional HEAD for an extract with no update feed) → changed.
//
// A custom region follows every extract it is clipped from. Requests to the publisher are
// cached for a minute by URL, so regions that share an extract ask once.
func (b *Builder) SourceChanged(ctx context.Context, r catalog.Region) (SourceStatus, error) {
	live, hasLive := b.liveRegions()
	if r.Custom != nil {
		if len(r.Custom.Parents) == 0 {
			return SourceStatus{}, errors.New("custom region has no parent extracts")
		}
		meta, metaErr := readMeta(b.RegionDir(r.ID))
		if _, err := os.Stat(b.regionFile(r.ID)); err != nil || metaErr != nil || meta.ClipHash != customClipHash(r) {
			return SourceStatus{Changed: true, Detail: "not clipped yet"}, nil //nolint:nilerr // a missing clip is the answer, not a failure
		}
		if st, unpublished := notLive(r.ID, meta, live, hasLive); unpublished {
			return st, nil
		}
		var ids, sums []string
		complete := true
		for _, p := range r.Custom.Parents {
			pm, err := readMeta(b.RegionDir(p.ID))
			if err != nil {
				complete = false
				break
			}
			ids, sums = append(ids, p.ID), append(sums, pm.MD5)
		}
		if complete && parentSignature(ids, sums) != meta.ParentMD5 {
			return SourceStatus{Changed: true, Detail: "an extract it is clipped from was downloaded again since it was clipped"}, nil
		}
		var same []string
		for _, p := range r.Custom.Parents {
			st, err := b.publisherChanged(ctx, p)
			if err != nil {
				return SourceStatus{}, fmt.Errorf("%s: %w", p.ID, err)
			}
			if st.Changed {
				return SourceStatus{Changed: true, Detail: p.ID + ": " + st.Detail}, nil
			}
			same = append(same, st.Detail)
		}
		return SourceStatus{Detail: strings.Join(same, "; ")}, nil
	}
	meta, metaErr := readMeta(b.RegionDir(r.ID))
	if _, fileErr := os.Stat(b.regionFile(r.ID)); metaErr != nil || fileErr != nil || (r.PBFURL != "" && meta.URL != r.PBFURL) {
		return SourceStatus{Changed: true, Detail: "not downloaded yet"}, nil //nolint:nilerr // a missing download is the answer, not a failure
	}
	if st, unpublished := notLive(r.ID, meta, live, hasLive); unpublished {
		return st, nil
	}
	return b.publisherChanged(ctx, r)
}

// liveRegions returns the live generation's regions by id, and whether a generation is live.
func (b *Builder) liveRegions() (map[string]ManifestRegion, bool) {
	cur := b.CurrentGeneration()
	if cur == "" {
		return nil, false
	}
	m, err := ReadManifest(filepath.Join(b.generationsDir(), cur))
	if err != nil {
		return nil, false
	}
	out := make(map[string]ManifestRegion, len(m.Regions))
	for _, r := range m.Regions {
		out[r.ID] = r
	}
	return out, true
}

// notLive reports whether the downloaded data for a region is something the live generation
// does not carry.
func notLive(id string, meta *DownloadMeta, live map[string]ManifestRegion, hasLive bool) (SourceStatus, bool) {
	if !hasLive {
		return SourceStatus{Changed: true, Detail: "no map has been built from it yet"}, true
	}
	lr, ok := live[id]
	if !ok {
		return SourceStatus{Changed: true, Detail: "not in the live map yet"}, true
	}
	if lr.MD5 != "" && meta.MD5 != "" && lr.MD5 != meta.MD5 {
		return SourceStatus{Changed: true, Detail: fmt.Sprintf("the downloaded data (%s) is newer than the live map's (%s): the build that fetched it did not finish",
			dateOnly(meta.SourceTimestamp), dateOnly(lr.SourceTimestamp))}, true
	}
	return SourceStatus{}, false
}

// publisherChanged asks the publisher whether it has a newer extract than the one downloaded.
func (b *Builder) publisherChanged(ctx context.Context, r catalog.Region) (SourceStatus, error) {
	if r.PBFURL == "" {
		return SourceStatus{}, errors.New("region has no download URL")
	}
	meta, metaErr := readMeta(b.RegionDir(r.ID))
	if _, fileErr := os.Stat(b.regionFile(r.ID)); metaErr != nil || fileErr != nil || meta.URL != r.PBFURL {
		return SourceStatus{Changed: true, Detail: "not downloaded yet"}, nil //nolint:nilerr // a missing download is the answer, not a failure
	}
	if r.UpdatesURL != "" {
		seq, stamp, err := b.cachedState(ctx, strings.TrimSuffix(r.UpdatesURL, "/")+"/state.txt")
		if err == nil && meta.Sequence != 0 {
			if seq != meta.Sequence {
				return SourceStatus{Changed: true, Detail: fmt.Sprintf("newer data published (%s; sequence %d, downloaded %d)", dateOnly(stamp), seq, meta.Sequence)}, nil
			}
			return SourceStatus{Detail: fmt.Sprintf("data of %s is the latest (sequence %d)", dateOnly(stamp), seq)}, nil
		}
		// No usable sequence on one side: fall through to the file's own headers.
	}
	head, err := b.cachedHead(ctx, r.PBFURL, meta.ETag, meta.LastModified)
	if err != nil {
		return SourceStatus{}, err
	}
	switch {
	case head.status == http.StatusNotModified:
		return SourceStatus{Detail: "not modified since it was downloaded"}, nil
	case head.status != http.StatusOK:
		return SourceStatus{}, fmt.Errorf("HEAD %s: %d", r.PBFURL, head.status)
	case head.etag != "" && meta.ETag != "":
		if head.etag == meta.ETag {
			return SourceStatus{Detail: "same file as downloaded"}, nil
		}
		return SourceStatus{Changed: true, Detail: "the published file has changed"}, nil
	case head.lastModified != "" && meta.LastModified != "":
		if head.lastModified == meta.LastModified {
			return SourceStatus{Detail: "not modified since " + head.lastModified}, nil
		}
		return SourceStatus{Changed: true, Detail: "published " + head.lastModified}, nil
	}
	// Nothing to compare: say so rather than guess. The caller treats an error as "not
	// checked", and a region set to refresh whether or not it changed is unaffected.
	return SourceStatus{}, errors.New("the source gives no sequence, ETag or date to compare")
}

// sourceCacheTTL is how long an answer from a publisher is reused. Regions that share an
// extract, a scheduled check and a "check now" a moment apart, all ask once.
const sourceCacheTTL = time.Minute

type sourceAnswer struct {
	at                 time.Time
	seq                int64
	stamp              string
	status             int
	etag, lastModified string
	err                error
}

// sourceCache holds recent publisher answers by request (URL and validators).
type sourceCache struct {
	mu sync.Mutex
	m  map[string]sourceAnswer
}

func (c *sourceCache) get(key string, now time.Time) (sourceAnswer, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.m[key]
	if !ok || now.Sub(a.at) > sourceCacheTTL {
		return sourceAnswer{}, false
	}
	return a, true
}

func (c *sourceCache) put(key string, a sourceAnswer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]sourceAnswer{}
	}
	for k, v := range c.m { // small and short-lived: drop what has expired
		if a.at.Sub(v.at) > sourceCacheTTL {
			delete(c.m, k)
		}
	}
	c.m[key] = a
}

func (b *Builder) cachedState(ctx context.Context, url string) (int64, string, error) {
	now := b.now()
	if a, ok := b.sources.get("state "+url, now); ok {
		return a.seq, a.stamp, a.err
	}
	s := &state{b: b}
	seq, stamp, err := s.fetchState(ctx, url)
	if ctx.Err() == nil { // a cancelled request says nothing about the source
		b.sources.put("state "+url, sourceAnswer{at: now, seq: seq, stamp: stamp, err: err})
	}
	return seq, stamp, err
}

func (b *Builder) cachedHead(ctx context.Context, url, etag, lastModified string) (sourceAnswer, error) {
	now := b.now()
	key := "head " + url + " " + etag + " " + lastModified
	if a, ok := b.sources.get(key, now); ok {
		return a, a.err
	}
	hctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(hctx, http.MethodHead, url, nil)
	if err != nil {
		return sourceAnswer{}, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastModified != "" {
		req.Header.Set("If-Modified-Since", lastModified)
	}
	a := sourceAnswer{at: now}
	resp, err := b.httpClient().Do(req)
	if err != nil {
		a.err = err
	} else {
		_ = resp.Body.Close()
		a.status, a.etag, a.lastModified = resp.StatusCode, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
	}
	if ctx.Err() == nil {
		b.sources.put(key, a)
	}
	return a, a.err
}

// dateOnly shortens an RFC 3339 timestamp to its date for messages.
func dateOnly(stamp string) string {
	if len(stamp) >= 10 && stamp[4] == '-' {
		return stamp[:10]
	}
	if stamp == "" {
		return "unknown date"
	}
	return stamp
}
