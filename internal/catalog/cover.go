package catalog

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// A custom region is clipped from the cheapest set of Geofabrik extracts that together cover it:
// a box across the Texas/New Mexico line downloads texas + new-mexico (~0.8 GB), not all of
// the US (~12 GB). Coverage is decided on a dense sample of the area (grid points inside it plus
// its vertices and points along its edges); Geofabrik boundaries overlap by a small buffer, so
// adjacent extracts leave no gap between samples.

// Parent is one extract a custom region is clipped from.
type Parent struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
	// Estimated is true when the size is an area-based guess (the extract was never HEADed).
	Estimated bool `json:"estimated,omitempty"`
}

// CoverPlan is the set of extracts a custom area is built from.
type CoverPlan struct {
	Parents    []Parent `json:"parents"`
	TotalBytes int64    `json:"total_bytes"`
	Estimated  bool     `json:"estimated,omitempty"` // some sizes are guesses
	// SingleBest is the cheapest single extract that alone covers the area, for comparison.
	SingleBest *Parent `json:"single_best,omitempty"`
}

// sampleTarget is about how many interior grid points the coverage test uses.
const sampleTarget = 1600

// coverSamples returns points that represent the area: grid points inside it, every vertex, and
// points along every edge of the outer ring.
func coverSamples(poly [][][2]float64) [][2]float64 {
	if len(poly) == 0 || len(poly[0]) < 3 {
		return nil
	}
	b := ringsBBox(poly)
	w, h := b[2]-b[0], b[3]-b[1]
	n := int(math.Ceil(math.Sqrt(sampleTarget)))
	var pts [][2]float64
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			p := [2]float64{b[0] + w*(float64(i)+0.5)/float64(n), b[1] + h*(float64(j)+0.5)/float64(n)}
			if ringsContain(poly, p) {
				pts = append(pts, p)
			}
		}
	}
	outer := poly[0]
	var perim float64
	for i := range outer {
		q := outer[(i+1)%len(outer)]
		perim += math.Hypot(q[0]-outer[i][0], q[1]-outer[i][1])
	}
	step := perim / 400
	for i := range outer {
		p, q := outer[i], outer[(i+1)%len(outer)]
		pts = append(pts, p)
		d := math.Hypot(q[0]-p[0], q[1]-p[1])
		if step <= 0 {
			continue
		}
		for t := step; t < d; t += step {
			f := t / d
			pts = append(pts, [2]float64{p[0] + (q[0]-p[0])*f, p[1] + (q[1]-p[1])*f})
		}
	}
	return pts
}

func ringsBBox(poly [][][2]float64) BBox {
	b := BBox{180, 90, -180, -90}
	for _, p := range poly[0] {
		b[0], b[1] = math.Min(b[0], p[0]), math.Min(b[1], p[1])
		b[2], b[3] = math.Max(b[2], p[0]), math.Max(b[3], p[1])
	}
	return b
}

// ringsContain is the even-odd test over every ring.
func ringsContain(poly [][][2]float64, p [2]float64) bool {
	in := false
	for _, r := range poly {
		if ringContains(r, p) {
			in = !in
		}
	}
	return in
}

func bboxOverlap(a, b BBox) bool {
	return a[0] <= b[2] && b[0] <= a[2] && a[1] <= b[3] && b[1] <= a[3]
}

// planCover picks the cheapest set of entries covering the samples. size returns an entry's bytes
// and whether the size is known. Samples no extract contains (open ocean beyond every boundary)
// are ignored: there is nothing to download there.
func planCover(entries []Entry, poly [][][2]float64, size func(Entry) (int64, bool)) (CoverPlan, error) {
	samples := coverSamples(poly)
	if len(samples) == 0 {
		return CoverPlan{}, errors.New("catalog: area has no interior")
	}
	box := ringsBBox(poly)
	type cand struct {
		e     Entry
		bytes int64
		known bool
		hits  []int
	}
	var cands []cand
	coverable := make([]bool, len(samples))
	for _, e := range entries {
		if len(e.geom) == 0 || !bboxOverlap(e.BBox, box) {
			continue
		}
		var hits []int
		for i, p := range samples {
			if e.geom.contains(p) {
				hits = append(hits, i)
				coverable[i] = true
			}
		}
		if len(hits) == 0 {
			continue
		}
		b, known := size(e)
		if b <= 0 {
			b = 1
		}
		cands = append(cands, cand{e: e, bytes: b, known: known, hits: hits})
	}
	need := 0
	for _, c := range coverable {
		if c {
			need++
		}
	}
	if need == 0 {
		return CoverPlan{}, ErrNotCovered
	}

	// Greedy weighted set cover: repeatedly take the extract with the lowest bytes per newly
	// covered sample.
	covered := make([]bool, len(samples))
	left := need
	var chosen []int
	for left > 0 {
		best, bestRatio := -1, math.Inf(1)
		for i, c := range cands {
			n := 0
			for _, h := range c.hits {
				if !covered[h] {
					n++
				}
			}
			if n == 0 {
				continue
			}
			r := float64(c.bytes) / float64(n)
			if r < bestRatio || (r == bestRatio && best >= 0 && c.bytes < cands[best].bytes) {
				best, bestRatio = i, r
			}
		}
		if best < 0 {
			break
		}
		chosen = append(chosen, best)
		for _, h := range cands[best].hits {
			if !covered[h] {
				covered[h] = true
				left--
			}
		}
	}
	// Prune parents that others already cover, most expensive first.
	sort.Slice(chosen, func(i, j int) bool { return cands[chosen[i]].bytes > cands[chosen[j]].bytes })
	for k := 0; k < len(chosen); k++ {
		count := make([]int, len(samples))
		for m, ci := range chosen {
			if m == k {
				continue
			}
			for _, h := range cands[ci].hits {
				count[h]++
			}
		}
		redundant := true
		for _, h := range cands[chosen[k]].hits {
			if count[h] == 0 {
				redundant = false
				break
			}
		}
		if redundant {
			chosen = append(chosen[:k], chosen[k+1:]...)
			k--
		}
	}

	plan := CoverPlan{}
	for _, ci := range chosen {
		c := cands[ci]
		plan.Parents = append(plan.Parents, Parent{ID: c.e.ID, Name: c.e.Name, Bytes: c.bytes, Estimated: !c.known})
		plan.TotalBytes += c.bytes
		plan.Estimated = plan.Estimated || !c.known
	}
	// The cheapest single extract covering everything coverable, if it beats the union.
	for _, c := range cands {
		if len(c.hits) < need {
			continue
		}
		if plan.SingleBest == nil || c.bytes < plan.SingleBest.Bytes {
			plan.SingleBest = &Parent{ID: c.e.ID, Name: c.e.Name, Bytes: c.bytes, Estimated: !c.known}
		}
	}
	if sb := plan.SingleBest; sb != nil && (sb.Bytes < plan.TotalBytes || (sb.Bytes == plan.TotalBytes && len(plan.Parents) > 1)) {
		plan.Parents, plan.TotalBytes, plan.Estimated = []Parent{*sb}, sb.Bytes, sb.Estimated
	}
	sort.Slice(plan.Parents, func(i, j int) bool { return plan.Parents[i].ID < plan.Parents[j].ID })
	return plan, nil
}

// normalisePolygon closes rings and checks the outer ring.
func normalisePolygon(poly [][][2]float64) ([][][2]float64, error) {
	if len(poly) == 0 || len(poly[0]) < 3 {
		return nil, errors.New("catalog: polygon outer ring needs at least 3 points")
	}
	out := make([][][2]float64, 0, len(poly))
	for _, r := range poly {
		if len(r) < 3 {
			continue
		}
		if r[0] != r[len(r)-1] {
			r = append(append([][2]float64(nil), r...), r[0])
		}
		out = append(out, r)
	}
	return out, nil
}

// areaPolygon turns a bbox or polygon (one of them) into closed rings and their bbox.
func areaPolygon(bbox *BBox, polygon [][][2]float64) ([][][2]float64, BBox, error) {
	switch {
	case len(polygon) > 0:
		p, err := normalisePolygon(polygon)
		if err != nil {
			return nil, BBox{}, err
		}
		box := ringsBBox(p)
		if !box.Valid() {
			return nil, BBox{}, fmt.Errorf("catalog: invalid polygon bounds %v", box)
		}
		return p, box, nil
	case bbox != nil:
		b := *bbox
		if !b.Valid() {
			return nil, BBox{}, fmt.Errorf("catalog: invalid bbox %v", b)
		}
		return [][][2]float64{{{b[0], b[1]}, {b[2], b[1]}, {b[2], b[3]}, {b[0], b[3]}, {b[0], b[1]}}}, b, nil
	}
	return nil, BBox{}, errors.New("catalog: custom region needs a bbox or polygon")
}

// defaultBytesPerDeg2 guesses an extract's size from its bbox when nothing is known.
const defaultBytesPerDeg2 = 10 << 20

// sizeFunc returns cached sizes, falling back to an area estimate scaled by the median density of
// the known ones.
func (c *Catalog) sizeFunc() func(Entry) (int64, bool) {
	c.mu.RLock()
	var dens []float64
	sizes := make(map[string]int64, len(c.sizes))
	for id, s := range c.sizes {
		sizes[id] = s.Size
		if i, ok := c.byID[id]; ok && s.Size > 0 && c.entries[i].BBox.Area() > 0 {
			dens = append(dens, float64(s.Size)/c.entries[i].BBox.Area())
		}
	}
	c.mu.RUnlock()
	density := float64(defaultBytesPerDeg2)
	if len(dens) >= 3 {
		sort.Float64s(dens)
		density = dens[len(dens)/2]
	}
	return func(e Entry) (int64, bool) {
		if s := sizes[e.ID]; s > 0 {
			return s, true
		}
		return int64(e.BBox.Area() * density), false
	}
}

// candidateIDs are the entries whose bbox overlaps the area (for FillSizes).
func (c *Catalog) candidateIDs(box BBox) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var ids []string
	for _, e := range c.entries {
		if len(e.geom) > 0 && bboxOverlap(e.BBox, box) {
			ids = append(ids, e.ID)
		}
	}
	return ids
}

// PlanCustom plans the parents for a custom area, HEADing candidate extracts first (cached for
// MaxAge) so the choice uses real sizes.
func (c *Catalog) PlanCustom(ctx context.Context, bbox *BBox, polygon [][][2]float64) (CoverPlan, error) {
	poly, box, err := areaPolygon(bbox, polygon)
	if err != nil {
		return CoverPlan{}, err
	}
	_ = c.FillSizes(ctx, c.candidateIDs(box))
	return c.plan(poly)
}

func (c *Catalog) plan(poly [][][2]float64) (CoverPlan, error) {
	size := c.sizeFunc()
	c.mu.RLock()
	entries := c.entries
	c.mu.RUnlock()
	return planCover(entries, poly, size)
}

// ParentIDs lists the plan's parent ids, joined for display.
func (p CoverPlan) ParentIDs() string {
	ids := make([]string, len(p.Parents))
	for i, x := range p.Parents {
		ids[i] = x.ID
	}
	return strings.Join(ids, ", ")
}
