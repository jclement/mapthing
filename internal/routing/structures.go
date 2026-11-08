package routing

import (
	"context"
	"errors"
)

// Structure kinds reported by Structures.
const (
	StructureBridge = "bridge"
	StructureTunnel = "tunnel"
)

type vStructureTrace struct {
	Edges []struct {
		Bridge bool `json:"bridge"`
		Tunnel bool `json:"tunnel"`
	} `json:"edges"`
	MatchedPoints []struct {
		Type      string `json:"type"` // matched | interpolated | unmatched
		EdgeIndex *int   `json:"edge_index"`
	} `json:"matched_points"`
}

// Structures map-matches pts (lon, lat, in path order) onto the road graph and reports
// for each point whether its matched edge is a bridge or a tunnel ("" = ground, or not
// matched). matched is the fraction of points Valhalla could place on an edge; callers
// should distrust the answer when it is low. It uses the auto costing and Valhalla's
// default search radius, so it suits paths that follow roads (routes, GPS traces).
func (c *Client) Structures(ctx context.Context, pts [][2]float64) (kinds []string, matched float64, err error) {
	if len(pts) < 2 {
		return make([]string, len(pts)), 0, nil
	}
	body := map[string]any{
		"encoded_polyline": EncodePolyline(pts, 6),
		"shape_match":      "map_snap",
		"costing":          "auto",
		"filters": map[string]any{
			"attributes": []string{"edge.bridge", "edge.tunnel", "matched.type", "matched.edge_index"},
			"action":     "include",
		},
	}
	var tr vStructureTrace
	if err := c.post(ctx, "trace_attributes", body, &tr); err != nil {
		return nil, 0, err
	}
	return structureKinds(tr, len(pts))
}

// structureKinds turns a trace_attributes answer into per-point kinds.
func structureKinds(tr vStructureTrace, n int) ([]string, float64, error) {
	if len(tr.MatchedPoints) != n {
		return nil, 0, errors.New("routing: trace_attributes returned a different number of matched points")
	}
	kinds := make([]string, n)
	var ok int
	for i, mp := range tr.MatchedPoints {
		if mp.Type == "unmatched" || mp.EdgeIndex == nil || *mp.EdgeIndex < 0 || *mp.EdgeIndex >= len(tr.Edges) {
			continue
		}
		ok++
		e := tr.Edges[*mp.EdgeIndex]
		switch {
		case e.Tunnel:
			kinds[i] = StructureTunnel
		case e.Bridge:
			kinds[i] = StructureBridge
		}
	}
	return kinds, float64(ok) / float64(n), nil
}
