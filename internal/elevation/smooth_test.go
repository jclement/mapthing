package elevation

import (
	"math"
	"testing"
)

// A road profile on a 1 m DEM: flat at 50 m, one bridge whose bare-earth
// profile dips 6 m into the creek over 30 m, then a real 40 m climb over 2 km.
func bridgeAndHill(spacing float64) (elev, dist []float64) {
	for d := 0.0; d <= 6000; d += spacing {
		e := 50.0
		if d > 1000 && d < 1030 {
			e = 44 // creek bed under the bridge
		}
		if d >= 3000 && d < 5000 {
			e = 50 + 40*(d-3000)/2000
		} else if d >= 5000 {
			e = 90
		}
		elev = append(elev, e)
		dist = append(dist, d)
	}
	return elev, dist
}

func TestMedianRemovesBridgeDipKeepsClimb(t *testing.T) {
	for _, sp := range []float64{5, 10, 30} {
		elev, dist := bridgeAndHill(sp)
		g, l := GainLoss(elev, DefaultHysteresisM)
		if math.Abs(g-46) > 0.01 || math.Abs(l-6) > 0.01 {
			t.Errorf("spacing %v without median: gain/loss %v/%v, want 46/6 (dip counted)", sp, g, l)
		}
		g, l = GainLoss(MedianFilter(elev, dist, DefaultSmoothWindowM), DefaultHysteresisM)
		if math.Abs(g-40) > 0.5 || l > 0.01 {
			t.Errorf("spacing %v with median: gain/loss %v/%v, want 40/0", sp, g, l)
		}
	}
}

func TestMedianFilterEdges(t *testing.T) {
	// Samples further apart than the window pass through unchanged.
	e := []float64{1, 9, 2, 8}
	d := []float64{0, 500, 1000, 1500}
	if got := MedianFilter(e, d, 150); got[1] != 9 || got[3] != 8 {
		t.Errorf("sparse series changed: %v", got)
	}
	// Window off.
	if got := MedianFilter(e, d, 0); got[1] != 9 {
		t.Errorf("window 0 changed series: %v", got)
	}
	// Odd/even windows and a single spike.
	e = []float64{10, 10, 30, 10, 10}
	d = []float64{0, 10, 20, 30, 40}
	if got := MedianFilter(e, d, 25); got[2] != 10 {
		t.Errorf("spike survived: %v", got)
	}
}

func TestGreatCircle(t *testing.T) {
	a, b := LonLat{-120, 50}, LonLat{-100, 50}
	m := greatCircle(a, b, 0.5)
	// The great-circle midpoint along a parallel lies poleward of it.
	if math.Abs(m[0]+110) > 1e-9 || m[1] <= 50.3 {
		t.Errorf("midpoint %v", m)
	}
	if d1, d2 := HaversineM(a, m), HaversineM(m, b); math.Abs(d1-d2) > 1e-3 {
		t.Errorf("midpoint not equidistant: %v vs %v", d1, d2)
	}
	if greatCircle(a, b, 0) != a || greatCircle(a, b, 1) != b || greatCircle(a, a, 0.3) != a {
		t.Error("endpoints")
	}
}

// Google semantics: N samples, ends included, equal great-circle spacing even
// on one long segment.
func TestDensifySamplesEvenOnLongSegment(t *testing.T) {
	path := []LonLat{{-120, 50}, {-100, 50}}
	pts, dist := Densify(path, 0, 11)
	if len(pts) != 11 || pts[0] != path[0] || pts[10] != path[1] {
		t.Fatalf("pts %v", pts)
	}
	step := HaversineM(path[0], path[1]) / 10
	for i := 1; i < 11; i++ {
		if got := HaversineM(pts[i-1], pts[i]); math.Abs(got-step) > 0.01 {
			t.Errorf("gap %d = %.3f m, want %.3f", i, got, step)
		}
		if math.Abs(dist[i]-dist[i-1]-step) > 1e-6 {
			t.Errorf("reported gap %d", i)
		}
	}
}
