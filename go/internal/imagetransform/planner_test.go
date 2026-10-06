package imagetransform

import (
	"encoding/json"
	"os"
	"testing"
)

func TestLegacyGeometryOracle(t *testing.T) {
	raw, e := os.ReadFile("testdata/geometry.json")
	if e != nil {
		t.Fatal(e)
	}
	var rows []struct {
		Width, Height int
		Recipe        string
		Plan          Plan
	}
	if e = json.Unmarshal(raw, &rows); e != nil {
		t.Fatal(e)
	}
	for _, r := range rows {
		p, e := PlanTransform(r.Width, r.Height, r.Recipe)
		if e != nil || p != r.Plan {
			t.Errorf("%dx%d %s: got %+v (%v), want %+v", r.Width, r.Height, r.Recipe, p, e, r.Plan)
		}
	}
	if len(rows) < 40 {
		t.Fatal("oracle fixture coverage missing")
	}
}
func TestPlannerRefusesEmptyRaster(t *testing.T) {
	for _, c := range []struct {
		w, h int
		r    string
	}{{0, 1, "profile"}, {1, 0, "preview"}, {1, 10000, "preview"}, {1, 1, "unknown"}} {
		if _, e := PlanTransform(c.w, c.h, c.r); e == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}
