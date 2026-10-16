package geo

import (
	"math"
	"testing"
)

func near(got, want, tol float64) bool { return math.Abs(got-want) <= tol }

func TestDistanceAndBearing(t *testing.T) {
	// One degree of latitude is ~111.195 km on the mean-radius sphere.
	a, b := Point{Lon: 71.43, Lat: 51}, Point{Lon: 71.43, Lat: 52}
	if d := Distance(a, b); !near(d, 111195, 1) {
		t.Errorf("Distance over 1 deg of latitude = %.1f", d)
	}
	if d := Distance(a, a); d != 0 {
		t.Errorf("Distance to itself = %v", d)
	}
	// Astana: Khan Shatyr -> Baiterek, ~1.9 km; reference value from the
	// haversine formula computed independently.
	ks, bt := Point{Lon: 71.403861, Lat: 51.132505}, Point{Lon: 71.430411, Lat: 51.128258}
	if d := Distance(ks, bt); !near(d, 1912, 5) {
		t.Errorf("Khan Shatyr -> Baiterek = %.1f m", d)
	}

	for _, c := range []struct {
		to   Point
		want float64
	}{
		{Point{Lon: 71.43, Lat: 51.01}, 0},
		{Point{Lon: 71.44, Lat: 51}, 90},
		{Point{Lon: 71.43, Lat: 50.99}, 180},
		{Point{Lon: 71.42, Lat: 51}, 270},
	} {
		if got := Bearing(a, c.to); !near(got, c.want, 0.01) {
			t.Errorf("Bearing to %+v = %.3f, want %v", c.to, got, c.want)
		}
	}
}

func TestAngles(t *testing.T) {
	for _, c := range []struct{ in, want float64 }{{0, 0}, {360, 0}, {-90, 270}, {725, 5}, {-1e-15, 0}} {
		if got := NormBearing(c.in); !near(got, c.want, 1e-9) || got >= 360 {
			t.Errorf("NormBearing(%v) = %v, want %v", c.in, got, c.want)
		}
	}
	for _, c := range []struct{ a, b, want float64 }{{10, 350, 20}, {0, 180, 180}, {90, 90, 0}, {-30, 30, 60}, {270, 45, 135}} {
		if got := AngleDiff(c.a, c.b); !near(got, c.want, 1e-9) {
			t.Errorf("AngleDiff(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestDestinationRoundTrip(t *testing.T) {
	p := Point{Lon: 71.4306, Lat: 51.1283}
	for _, brg := range []float64{0, 45, 90, 200, 315} {
		q := Destination(p, brg, 250)
		if d := Distance(p, q); !near(d, 250, 0.01) {
			t.Errorf("bearing %v: distance %.4f, want 250", brg, d)
		}
		if b := Bearing(p, q); AngleDiff(b, brg) > 0.01 {
			t.Errorf("bearing %v: came back as %.4f", brg, b)
		}
	}
}

// lShape is a 300 m line east, then 200 m north, with a repeated vertex.
func lShape(t *testing.T) (*Line, Point) {
	t.Helper()
	o := Point{Lon: 71.43, Lat: 51.13}
	corner := Destination(o, 90, 300)
	l, err := NewLine([]Point{o, corner, corner, Destination(corner, 0, 200)})
	if err != nil {
		t.Fatal(err)
	}
	return l, o
}

func TestLineBasics(t *testing.T) {
	l, o := lShape(t)
	if !near(l.Length(), 500, 0.01) || l.Segments() != 3 || l.Start() != o {
		t.Fatalf("length %.3f segments %d", l.Length(), l.Segments())
	}
	if !near(l.VertexAlong(2), 300, 0.01) {
		t.Errorf("VertexAlong(2) = %v", l.VertexAlong(2))
	}
	if d := Distance(l.PointAt(150), Destination(o, 90, 150)); d > 0.01 {
		t.Errorf("PointAt(150) is %.3f m off", d)
	}
	if d := Distance(l.PointAt(-5), o); d != 0 {
		t.Errorf("PointAt clamps below 0: %.3f", d)
	}
	if d := Distance(l.PointAt(1e9), l.End()); d != 0 {
		t.Errorf("PointAt clamps above length: %.3f", d)
	}
	// The zero-length segment at the corner borrows a neighbour's bearing.
	for _, c := range []struct{ along, want float64 }{{0, 90}, {299, 90}, {300, 0}, {450, 0}, {600, 0}} {
		if got := l.BearingAt(c.along); AngleDiff(got, c.want) > 0.1 {
			t.Errorf("BearingAt(%v) = %.2f, want %v", c.along, got, c.want)
		}
	}

	if _, err := NewLine([]Point{o}); err == nil {
		t.Error("a single point must not make a line")
	}
	if _, err := NewLine([]Point{o, {Lon: 200, Lat: 0}}); err == nil {
		t.Error("an invalid point must be rejected")
	}
}

func TestLineProject(t *testing.T) {
	l, o := lShape(t)

	// 20 m to the left (north) of the first leg, 100 m in.
	p := Destination(Destination(o, 90, 100), 0, 20)
	pr := l.Project(p, 0, l.Length())
	if !near(pr.Along, 100, 0.05) || !near(pr.Offset, 20, 0.05) || pr.Segment != 0 {
		t.Errorf("projection = %+v", pr)
	}
	if d := Distance(pr.Point, Destination(o, 90, 100)); d > 0.05 {
		t.Errorf("projected point %.3f m off", d)
	}

	// Beyond the corner on the outside: nearest is the corner itself.
	out := Destination(Destination(o, 90, 330), 180, 30)
	if pr := l.Project(out, 0, l.Length()); !near(pr.Along, 300, 0.05) || !near(pr.Offset, math.Hypot(30, 30), 0.1) {
		t.Errorf("outside the corner: %+v", pr)
	}

	// A window keeps the result inside it, even if a nearer part exists.
	if pr := l.Project(p, 250, 500); !near(pr.Along, 250, 0.05) || !near(pr.Offset, math.Hypot(150, 20), 0.2) {
		t.Errorf("windowed projection = %+v", pr)
	}
	// A window entirely past the end collapses to the end point.
	if pr := l.Project(p, 900, 1000); !near(pr.Along, 500, 1e-6) {
		t.Errorf("window past the end = %+v", pr)
	}
	// from > to is treated as the single point `from`.
	if pr := l.Project(p, 200, 100); !near(pr.Along, 200, 0.05) {
		t.Errorf("reversed window = %+v", pr)
	}
}

func TestLineProjectRouteThatDoublesBack(t *testing.T) {
	// Out 400 m east and back on a parallel lane 8 m north: without a window
	// a position on the return lane could snap to the outbound one.
	o := Point{Lon: 71.43, Lat: 51.13}
	e := Destination(o, 90, 400)
	l, err := NewLine([]Point{o, e, Destination(e, 0, 8), Destination(o, 0, 8)})
	if err != nil {
		t.Fatal(err)
	}
	pos := Destination(Destination(o, 90, 200), 0, 3) // closer to the outbound lane
	if pr := l.Project(pos, 0, l.Length()); pr.Segment != 0 {
		t.Errorf("unwindowed: segment %d", pr.Segment)
	}
	if pr := l.Project(pos, 400, l.Length()); pr.Segment != 2 || !near(pr.Along, 608, 0.1) || !near(pr.Offset, 5, 0.05) {
		t.Errorf("windowed to the return lane: %+v", pr)
	}
}
