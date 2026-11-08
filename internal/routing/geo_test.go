package routing

import (
	"math"
	"testing"
)

func TestPolylineGoogleExample(t *testing.T) {
	// The example from Google's polyline algorithm documentation.
	const enc = "_p~iF~ps|U_ulLnnqC_mqNvxq`@"
	want := [][2]float64{{-120.2, 38.5}, {-120.95, 40.7}, {-126.453, 43.252}}
	got, err := DecodePolyline(enc, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d points, want %d", len(got), len(want))
	}
	for i := range want {
		if math.Abs(got[i][0]-want[i][0]) > 1e-9 || math.Abs(got[i][1]-want[i][1]) > 1e-9 {
			t.Errorf("point %d: got %v want %v", i, got[i], want[i])
		}
	}
	if e := EncodePolyline(want, 5); e != enc {
		t.Errorf("encode: got %q want %q", e, enc)
	}
}

func TestPolylineRoundTrip(t *testing.T) {
	pts := [][2]float64{{-75.385, 38.69}, {-75.384994, 38.690074}, {-75.2588452, 38.471795}, {179.999999, -89.999999}}
	for _, prec := range []int{5, 6} {
		got, err := DecodePolyline(EncodePolyline(pts, prec), prec)
		if err != nil {
			t.Fatal(err)
		}
		tol := math.Pow10(-prec)
		for i := range pts {
			if math.Abs(got[i][0]-pts[i][0]) > tol || math.Abs(got[i][1]-pts[i][1]) > tol {
				t.Errorf("prec %d point %d: got %v want %v", prec, i, got[i], pts[i])
			}
		}
	}
}

func TestPolylineInvalid(t *testing.T) {
	for _, s := range []string{"_p~iF~ps|U_", "\x01\x02"} {
		if _, err := DecodePolyline(s, 5); err == nil {
			t.Errorf("%q: expected an error", s)
		}
	}
}

func TestHaversineAndSimplify(t *testing.T) {
	// One degree of latitude is about 111.2 km.
	if d := haversine([2]float64{0, 0}, [2]float64{0, 1}); math.Abs(d-111195) > 50 {
		t.Errorf("haversine: %f", d)
	}
	line := [][2]float64{{0, 0}, {0.0001, 0.000001}, {0.0002, 0}, {0.0003, 0.01}}
	s := simplify(line, 5)
	if len(s) != 3 {
		t.Errorf("simplify kept %d points, want 3: %v", len(s), s)
	}
	if b := bbox(line); b != [4]float64{0, 0, 0.0003, 0.01} {
		t.Errorf("bbox %v", b)
	}
}
