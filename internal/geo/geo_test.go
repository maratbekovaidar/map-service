package geo

import (
	"math"
	"testing"
)

func TestParseBBox(t *testing.T) {
	b, err := ParseBBox("71.04,50.74,71.96,51.46")
	if err != nil {
		t.Fatal(err)
	}
	if b.MinLon != 71.04 || b.MaxLat != 51.46 {
		t.Fatalf("unexpected bbox %+v", b)
	}
	if got, want := b.Viewbox(), "71.04,51.46,71.96,50.74"; got != want {
		t.Errorf("Viewbox() = %q, want %q", got, want)
	}
	if got, want := b.String(), "71.04,50.74,71.96,51.46"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	for _, bad := range []string{"", "1,2,3", "a,b,c,d", "5,5,4,6", "-200,0,10,10", "1,2,3,95"} {
		if _, err := ParseBBox(bad); err == nil {
			t.Errorf("ParseBBox(%q) succeeded, want error", bad)
		}
	}
}

func TestBBoxContains(t *testing.T) {
	b, _ := ParseBBox("71.04,50.74,71.96,51.46")
	cases := []struct {
		p    Point
		want bool
	}{
		{Point{71.4306, 51.1283}, true},
		{Point{71.04, 50.74}, true}, // edge is inclusive
		{Point{71.0, 51.0}, false},
		{Point{71.5, 52.0}, false},
		{Point{math.NaN(), 51}, false},
	}
	for _, c := range cases {
		if got := b.Contains(c.p); got != c.want {
			t.Errorf("Contains(%+v) = %v, want %v", c.p, got, c.want)
		}
	}
}

func TestParsePoint(t *testing.T) {
	p, err := ParsePoint("71.4306, 51.1283")
	if err != nil || p.Lon != 71.4306 || p.Lat != 51.1283 {
		t.Fatalf("ParsePoint = %+v, %v", p, err)
	}
	for _, bad := range []string{"", "1", "x,y", "181,0", "0,91"} {
		if _, err := ParsePoint(bad); err == nil {
			t.Errorf("ParsePoint(%q) succeeded, want error", bad)
		}
	}
}

func TestRound5(t *testing.T) {
	if got := FormatCoord(71.430612345); got != "71.43061" {
		t.Errorf("FormatCoord = %q", got)
	}
	if got := FormatCoord(51.1); got != "51.10000" {
		t.Errorf("FormatCoord = %q", got)
	}
}

func TestTileXY(t *testing.T) {
	// Well-known reference: lon 0, lat 0 at z1 is tile (1,1).
	if x, y := TileXY(Point{0, 0}, 1); x != 1 || y != 1 {
		t.Errorf("TileXY(0,0,z1) = %d,%d", x, y)
	}
	// Astana centre at z14; expected values computed independently with the
	// textbook slippy-map formula (x = (lon+180)/360*2^z,
	// y = (1-ln(tan(lat)+sec(lat))/pi)/2*2^z) -> 11442.88, 5475.70.
	x, y := TileXY(Point{71.4306, 51.1283}, 14)
	if x != 11442 || y != 5475 {
		t.Errorf("TileXY(Astana,z14) = %d,%d, want 11442,5475", x, y)
	}
}

func TestPolylineGoogleExample(t *testing.T) {
	pts := []Point{{Lon: -120.2, Lat: 38.5}, {Lon: -120.95, Lat: 40.7}, {Lon: -126.453, Lat: 43.252}}
	const want = "_p~iF~ps|U_ulLnnqC_mqNvxq`@"
	if got := EncodePolyline(pts, 5); got != want {
		t.Fatalf("EncodePolyline = %q, want %q", got, want)
	}
	back, err := DecodePolyline(want, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != len(pts) {
		t.Fatalf("decoded %d points, want %d", len(back), len(pts))
	}
	for i := range pts {
		if math.Abs(back[i].Lat-pts[i].Lat) > 1e-6 || math.Abs(back[i].Lon-pts[i].Lon) > 1e-6 {
			t.Errorf("point %d = %+v, want %+v", i, back[i], pts[i])
		}
	}
}

func TestPolylineRoundTripPrecision6(t *testing.T) {
	pts := []Point{{71.430612, 51.128312}, {71.431, 51.1284}, {71.4, 51.2}}
	enc := EncodePolyline(pts, 6)
	back, err := DecodePolyline(enc, 6)
	if err != nil {
		t.Fatal(err)
	}
	for i := range pts {
		if math.Abs(back[i].Lat-pts[i].Lat) > 1e-6 || math.Abs(back[i].Lon-pts[i].Lon) > 1e-6 {
			t.Errorf("point %d = %+v, want %+v", i, back[i], pts[i])
		}
	}
	if _, err := DecodePolyline("_p~iF~ps|", 5); err == nil {
		t.Error("truncated polyline decoded without error")
	}
}
