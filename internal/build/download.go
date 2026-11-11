package build

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Geofabrik publishes MD5 checksums: integrity, not security
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jclement/mapthing/internal/catalog"
)

// DownloadMeta is downloads/<region>/meta.json.
type DownloadMeta struct {
	URL             string    `json:"url"`
	ETag            string    `json:"etag,omitempty"`
	LastModified    string    `json:"last_modified,omitempty"`
	Size            int64     `json:"size"`
	MD5             string    `json:"md5"`
	Sequence        int64     `json:"sequence,omitempty"`         // Geofabrik replication sequence
	SourceTimestamp string    `json:"source_timestamp,omitempty"` // data as of (state.txt)
	DownloadedAt    time.Time `json:"downloaded_at"`
	CheckedAt       time.Time `json:"checked_at"`
	// Custom regions.
	Parents   []string `json:"parents,omitempty"`    // parent extract ids
	ParentMD5 string   `json:"parent_md5,omitempty"` // "id:md5,…" of the parents clipped from
	ClipHash  string   `json:"clip_hash,omitempty"`
}

func (b *Builder) httpClient() *http.Client {
	if b.HTTP != nil {
		return b.HTTP
	}
	return &http.Client{Timeout: 6 * time.Hour}
}

// RegionDir is where a region's extract lives.
func (b *Builder) RegionDir(id string) string {
	return filepath.Join(b.downloadsDir(), filepath.FromSlash(id))
}

func (b *Builder) regionFile(id string) string {
	return filepath.Join(b.RegionDir(id), path.Base(id)+".osm.pbf")
}

func readMeta(dir string) (*DownloadMeta, error) {
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, err
	}
	var m DownloadMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func writeMeta(dir string, m *DownloadMeta) error {
	data, _ := json.MarshalIndent(m, "", "  ")
	tmp := filepath.Join(dir, "meta.json.part")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "meta.json"))
}

func (s *state) download(ctx context.Context) error {
	s.sources = make([]string, len(s.opt.Regions))
	for i, r := range s.opt.Regions {
		s.rep.Progress(float64(i)/float64(len(s.opt.Regions)), r.ID)
		var (
			file string
			meta *DownloadMeta
			err  error
		)
		if r.Custom != nil {
			file, meta, err = s.ensureCustom(ctx, r)
		} else {
			file, meta, err = s.ensureExtractOnce(ctx, r)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", r.ID, err)
		}
		s.sources[i] = file
		mr := &s.m.Regions[i]
		mr.Source, mr.SourceTimestamp, mr.Sequence = meta.URL, meta.SourceTimestamp, meta.Sequence
		mr.SizeBytes, mr.MD5, mr.Parents = meta.Size, meta.MD5, meta.Parents
	}
	s.rep.Progress(1, "downloads ready")
	return nil
}

// ensureExtract makes sure a Geofabrik extract is on disk (and, when refreshing, current).
// The previous file stays in place until a new one has downloaded and verified.
func (s *state) ensureExtract(ctx context.Context, r catalog.Region) (string, *DownloadMeta, error) {
	if r.PBFURL == "" {
		return "", nil, errors.New("region has no download URL")
	}
	dir := s.b.RegionDir(r.ID)
	file := s.b.regionFile(r.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	meta, metaErr := readMeta(dir)
	_, fileErr := os.Stat(file)
	have := metaErr == nil && fileErr == nil && meta.URL == r.PBFURL
	if have && (!s.opt.Refresh || s.opt.SkipRefresh[r.ID]) {
		s.logf("%s: using existing download (%s)", r.ID, humanBytes(meta.Size))
		return file, meta, nil
	}

	var seq int64
	var stamp string
	if r.UpdatesURL != "" {
		var err error
		seq, stamp, err = s.fetchState(ctx, strings.TrimSuffix(r.UpdatesURL, "/")+"/state.txt")
		if err != nil {
			s.logf("%s: state.txt unavailable (%v); falling back to conditional GET", r.ID, err)
		} else if have && seq != 0 && seq == meta.Sequence {
			s.logf("%s: up to date (sequence %d)", r.ID, seq)
			meta.CheckedAt = s.b.now().UTC()
			_ = writeMeta(dir, meta)
			return file, meta, nil
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.PBFURL, nil)
	if err != nil {
		return "", nil, err
	}
	if have {
		if meta.ETag != "" {
			req.Header.Set("If-None-Match", meta.ETag)
		}
		if meta.LastModified != "" {
			req.Header.Set("If-Modified-Since", meta.LastModified)
		}
	}
	resp, err := s.b.httpClient().Do(req)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotModified && have {
		s.logf("%s: not modified", r.ID)
		meta.CheckedAt = s.b.now().UTC()
		_ = writeMeta(dir, meta)
		return file, meta, nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("GET %s: %s", r.PBFURL, resp.Status)
	}

	part := file + ".part"
	sum, n, err := s.copyWithProgress(part, resp.Body, resp.ContentLength, r.ID)
	if err != nil {
		_ = os.Remove(part)
		return "", nil, err
	}
	if resp.ContentLength > 0 && n != resp.ContentLength {
		_ = os.Remove(part)
		return "", nil, fmt.Errorf("short download: %d of %d bytes", n, resp.ContentLength)
	}
	if want, err := s.fetchMD5(ctx, r.PBFURL+".md5"); err == nil && want != "" && want != sum {
		_ = os.Remove(part)
		return "", nil, fmt.Errorf("md5 mismatch: got %s, published %s", sum, want)
	} else if err != nil {
		s.logf("%s: no published md5 (%v); size and header checks only", r.ID, err)
	}
	if err := checkPBFHeader(part); err != nil {
		_ = os.Remove(part)
		return "", nil, err
	}
	if err := os.Rename(part, file); err != nil {
		return "", nil, err
	}
	now := s.b.now().UTC()
	if stamp == "" {
		stamp = resp.Header.Get("Last-Modified")
	}
	nm := &DownloadMeta{
		URL: r.PBFURL, ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified"),
		Size: n, MD5: sum, Sequence: seq, SourceTimestamp: stamp, DownloadedAt: now, CheckedAt: now,
	}
	if err := writeMeta(dir, nm); err != nil {
		return "", nil, err
	}
	s.logf("%s: downloaded %s (md5 %s)", r.ID, humanBytes(n), sum)
	return file, nm, nil
}

// ensureExtractOnce is ensureExtract, run at most once per build for each extract: two custom
// regions over Texas (or a custom region and the Texas region itself) share one texas.osm.pbf.
func (s *state) ensureExtractOnce(ctx context.Context, r catalog.Region) (string, *DownloadMeta, error) {
	if s.extracts == nil {
		s.extracts = map[string]extractResult{}
	}
	if got, ok := s.extracts[r.ID]; ok {
		return got.file, got.meta, nil
	}
	file, meta, err := s.ensureExtract(ctx, r)
	if err != nil {
		return "", nil, err
	}
	s.extracts[r.ID] = extractResult{file, meta}
	return file, meta, nil
}

type extractResult struct {
	file string
	meta *DownloadMeta
}

// ensureCustom clips a custom region from each of its parent extracts and merges the clips,
// re-clipping only when a parent changed or the clip geometry changed.
//
// A region that is not being refreshed in this build (no refresh at all, or it is in
// SkipRefresh) keeps the clip it has: a parent extract it shares with a region that is being
// refreshed may be newer on disk, but this region's data only changes when its own refresh
// comes round. Its parents are not even looked at.
func (s *state) ensureCustom(ctx context.Context, r catalog.Region) (string, *DownloadMeta, error) {
	if len(r.Custom.Parents) == 0 {
		return "", nil, errors.New("custom region has no parent extracts")
	}
	dir := s.b.RegionDir(r.ID)
	file := s.b.regionFile(r.ID)
	clipHash := customClipHash(r)
	if !s.opt.Refresh || s.opt.SkipRefresh[r.ID] {
		if meta, err := readMeta(dir); err == nil && meta.ClipHash == clipHash {
			if _, err := os.Stat(file); err == nil {
				s.logf("%s: keeping its clip (data of %s)", r.ID, dateOnly(meta.SourceTimestamp))
				return file, meta, nil
			}
		}
	}
	type parentFile struct {
		id   string
		file string
		meta *DownloadMeta
	}
	var parents []parentFile
	for _, p := range r.Custom.Parents {
		file, meta, err := s.ensureExtractOnce(ctx, p)
		if err != nil {
			return "", nil, fmt.Errorf("parent %s: %w", p.ID, err)
		}
		parents = append(parents, parentFile{p.ID, file, meta})
	}
	sort.Slice(parents, func(i, j int) bool { return parents[i].id < parents[j].id })
	var sig []string
	for _, p := range parents {
		sig = append(sig, p.id+":"+p.meta.MD5)
	}
	parentSig := strings.Join(sig, ",")

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	if meta, err := readMeta(dir); err == nil && meta.ParentMD5 == parentSig && meta.ClipHash == clipHash {
		if _, err := os.Stat(file); err == nil {
			s.logf("%s: clip up to date", r.ID)
			return file, meta, nil
		}
	}

	clipArgs := []string{}
	if len(r.Polygon) > 0 {
		gj := filepath.Join(dir, "clip.geojson")
		data, _ := json.Marshal(map[string]any{
			"type": "Feature", "properties": map[string]any{},
			"geometry": map[string]any{"type": "Polygon", "coordinates": r.Polygon},
		})
		if err := os.WriteFile(gj, data, 0o644); err != nil {
			return "", nil, err
		}
		clipArgs = append(clipArgs, "-p", gj)
	} else {
		b := r.BBox
		clipArgs = append(clipArgs, "-b", fmt.Sprintf("%g,%g,%g,%g", b[0], b[1], b[2], b[3]))
	}
	partsDir := filepath.Join(dir, "parts")
	_ = os.RemoveAll(partsDir)
	if err := os.MkdirAll(partsDir, 0o755); err != nil {
		return "", nil, err
	}
	defer func() { _ = os.RemoveAll(partsDir) }()
	var clips []string
	for i, p := range parents {
		out := filepath.Join(partsDir, fmt.Sprintf("%02d.osm.pbf", i))
		args := append([]string{"extract", "--overwrite", "-o", out}, clipArgs...)
		if err := s.exec(ctx, dir, s.b.Tools.Osmium, append(args, p.file)...); err != nil {
			return "", nil, fmt.Errorf("clip from %s: %w", p.id, err)
		}
		clips = append(clips, out)
	}
	part := file + ".part"
	if len(clips) == 1 {
		if err := os.Rename(clips[0], part); err != nil {
			return "", nil, err
		}
	} else {
		// Ways and nodes along the shared border appear in both clips; merge keeps one copy.
		args := append([]string{"merge", "--overwrite", "-f", "pbf", "-o", part}, clips...)
		if err := s.exec(ctx, dir, s.b.Tools.Osmium, args...); err != nil {
			_ = os.Remove(part)
			return "", nil, err
		}
	}
	if err := os.Rename(part, file); err != nil {
		return "", nil, err
	}
	sum, size, err := md5File(file)
	if err != nil {
		return "", nil, err
	}
	now := s.b.now().UTC()
	meta := &DownloadMeta{
		URL: parents[0].meta.URL, Size: size, MD5: sum, DownloadedAt: now, CheckedAt: now,
		ParentMD5: parentSig, ClipHash: clipHash,
	}
	// The clip is as old as its oldest parent.
	for i, p := range parents {
		meta.Parents = append(meta.Parents, p.id)
		if i == 0 || (p.meta.SourceTimestamp != "" && p.meta.SourceTimestamp < meta.SourceTimestamp) {
			meta.SourceTimestamp, meta.Sequence = p.meta.SourceTimestamp, p.meta.Sequence
		}
	}
	s.logf("%s: clipped from %s (%s)", r.ID, strings.Join(meta.Parents, " + "), humanBytes(size))
	return file, meta, writeMeta(dir, meta)
}

// customClipHash identifies a custom region's clip geometry.
func customClipHash(r catalog.Region) string {
	clip, _ := json.Marshal(struct {
		B catalog.BBox
		P [][][2]float64
	}{r.BBox, r.Polygon})
	h := sha256.Sum256(clip)
	return hex.EncodeToString(h[:8])
}

// parentSignature is "id:md5,…" of the extracts a clip was made from, sorted by id.
func parentSignature(ids, md5s []string) string {
	type pm struct{ id, md5 string }
	ps := make([]pm, len(ids))
	for i := range ids {
		ps[i] = pm{ids[i], md5s[i]}
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].id < ps[j].id })
	sig := make([]string, len(ps))
	for i, p := range ps {
		sig[i] = p.id + ":" + p.md5
	}
	return strings.Join(sig, ",")
}

// fetchState reads sequenceNumber and timestamp from an osmosis state.txt.
func (s *state) fetchState(ctx context.Context, url string) (int64, string, error) {
	body, err := s.getSmall(ctx, url)
	if err != nil {
		return 0, "", err
	}
	var seq int64
	var stamp string
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "sequenceNumber":
			seq, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		case "timestamp":
			stamp = strings.ReplaceAll(strings.TrimSpace(v), `\:`, ":")
		}
	}
	if seq == 0 {
		return 0, "", errors.New("no sequenceNumber")
	}
	return seq, stamp, nil
}

func (s *state) fetchMD5(ctx context.Context, url string) (string, error) {
	body, err := s.getSmall(ctx, url)
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(body))
	if len(f) == 0 || len(f[0]) != 32 {
		return "", errors.New("malformed md5 file")
	}
	return strings.ToLower(f[0]), nil
}

func (s *state) getSmall(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.b.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func (s *state) copyWithProgress(dst string, src io.Reader, total int64, label string) (string, int64, error) {
	f, err := os.Create(dst)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := md5.New() //nolint:gosec // matches the published .md5 files
	var n atomic.Int64
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				got := n.Load()
				frac := 0.0
				if total > 0 {
					frac = float64(got) / float64(total)
				}
				s.rep.Progress(frac, fmt.Sprintf("%s: %s / %s", label, humanBytes(got), humanBytes(total)))
			}
		}
	}()
	defer close(done)
	buf := make([]byte, 1<<20)
	for {
		k, rerr := src.Read(buf)
		if k > 0 {
			if _, err := f.Write(buf[:k]); err != nil {
				return "", n.Load(), err
			}
			h.Write(buf[:k])
			n.Add(int64(k))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", n.Load(), rerr
		}
	}
	if err := f.Sync(); err != nil {
		return "", n.Load(), err
	}
	return hex.EncodeToString(h.Sum(nil)), n.Load(), f.Close()
}

// checkPBFHeader confirms the file starts with an OSM PBF header blob.
func checkPBFHeader(file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 64)
	k, _ := io.ReadFull(f, head)
	if !bytes.Contains(head[:k], []byte("OSMHeader")) {
		return errors.New("not an OSM PBF file (no OSMHeader blob)")
	}
	return nil
}

func md5File(file string) (string, int64, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := md5.New() //nolint:gosec // matches the published .md5 files
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
