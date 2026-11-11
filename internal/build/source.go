package build

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jclement/mapthing/internal/catalog"
)

// SourceStatus is what SourceChanged found for a region.
type SourceStatus struct {
	// Changed: the publisher has a newer extract than the one downloaded, or nothing is
	// downloaded yet. A refresh would fetch something.
	Changed bool
	// Detail says how that was decided, for people.
	Detail string
}

// SourceChanged looks at whether a region's source has moved on since its extract was
// downloaded, without downloading it: Geofabrik's replication sequence (state.txt) against the
// one recorded in downloads/<region>/meta.json, or a conditional HEAD when an extract has no
// update feed. A custom region has changed when any of the extracts it is clipped from has.
// It is the same test the download stage applies before fetching; the scheduler uses it to
// decide whether a build is worth starting at all.
func (b *Builder) SourceChanged(ctx context.Context, r catalog.Region) (SourceStatus, error) {
	if r.Custom != nil {
		if len(r.Custom.Parents) == 0 {
			return SourceStatus{}, errors.New("custom region has no parent extracts")
		}
		var same []string
		for _, p := range r.Custom.Parents {
			st, err := b.SourceChanged(ctx, p)
			if err != nil {
				return SourceStatus{}, fmt.Errorf("%s: %w", p.ID, err)
			}
			if st.Changed {
				return SourceStatus{Changed: true, Detail: p.ID + ": " + st.Detail}, nil
			}
			same = append(same, st.Detail)
		}
		if _, err := os.Stat(b.regionFile(r.ID)); err != nil {
			return SourceStatus{Changed: true, Detail: "not clipped yet"}, nil
		}
		return SourceStatus{Detail: strings.Join(same, "; ")}, nil
	}
	if r.PBFURL == "" {
		return SourceStatus{}, errors.New("region has no download URL")
	}
	meta, metaErr := readMeta(b.RegionDir(r.ID))
	if _, fileErr := os.Stat(b.regionFile(r.ID)); metaErr != nil || fileErr != nil || meta.URL != r.PBFURL {
		return SourceStatus{Changed: true, Detail: "not downloaded yet"}, nil
	}
	s := &state{b: b}
	if r.UpdatesURL != "" {
		seq, stamp, err := s.fetchState(ctx, strings.TrimSuffix(r.UpdatesURL, "/")+"/state.txt")
		if err == nil && meta.Sequence != 0 {
			if seq != meta.Sequence {
				return SourceStatus{Changed: true, Detail: fmt.Sprintf("newer data published (%s; sequence %d, downloaded %d)", dateOnly(stamp), seq, meta.Sequence)}, nil
			}
			return SourceStatus{Detail: fmt.Sprintf("data of %s is the latest (sequence %d)", dateOnly(stamp), seq)}, nil
		}
		// No usable sequence on one side: fall through to the file's own headers.
	}
	hctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(hctx, http.MethodHead, r.PBFURL, nil)
	if err != nil {
		return SourceStatus{}, err
	}
	if meta.ETag != "" {
		req.Header.Set("If-None-Match", meta.ETag)
	}
	if meta.LastModified != "" {
		req.Header.Set("If-Modified-Since", meta.LastModified)
	}
	resp, err := b.httpClient().Do(req)
	if err != nil {
		return SourceStatus{}, err
	}
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified:
		return SourceStatus{Detail: "not modified since it was downloaded"}, nil
	case resp.StatusCode != http.StatusOK:
		return SourceStatus{}, fmt.Errorf("HEAD %s: %s", r.PBFURL, resp.Status)
	}
	etag, lm := resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
	switch {
	case etag != "" && meta.ETag != "":
		if etag == meta.ETag {
			return SourceStatus{Detail: "same file as downloaded"}, nil
		}
		return SourceStatus{Changed: true, Detail: "the published file has changed"}, nil
	case lm != "" && meta.LastModified != "":
		if lm == meta.LastModified {
			return SourceStatus{Detail: "not modified since " + lm}, nil
		}
		return SourceStatus{Changed: true, Detail: "published " + lm}, nil
	}
	// Nothing to compare: say so rather than guess. The caller treats an error as "not
	// checked", and a region set to refresh whether or not it changed is unaffected.
	return SourceStatus{}, errors.New("the source gives no sequence, ETag or date to compare")
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
