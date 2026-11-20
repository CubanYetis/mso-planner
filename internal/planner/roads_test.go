package planner

import (
	"reflect"
	"testing"

	"github.com/BigRedS/mso-planner/internal/geo"
)

func TestNormalizeRoadName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"motorway", "M1", "M1"},
		{"a road", "A14", "A14"},
		{"a road with a letter-free number", "A414", "A414"},
		{"motorway-grade a road", "A1(M)", "A1(M)"},
		{"a1 and a1(m) stay distinct", "A1", "A1"},
		{"toll road keeps its suffix", "M6 Toll", "M6 Toll"},
		{"lowercase and padding", "  a1(m) ", "A1(M)"},
		{"space before (M)", "A1 (M)", "A1(M)"},
		{"irish motorway is not plain M1", "M1 (Ireland)", ""},
		{"road number inside a name", "North Orbital Road A405", ""},
		{"empty", "", ""},
		{"unnumbered street", "High Street", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeRoadName(tt.in); got != tt.want {
				t.Errorf("normalizeRoadName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestExtractRoadNames uses steps captured from the public OSRM server for
// N3 3DU to AL2 2AB. Road numbers are in ref and name is empty on the M1,
// which is why reading only name found no roads at all.
func TestExtractRoadNames(t *testing.T) {
	steps := []geo.Step{
		{Name: "Lichfield Grove"},
		{Name: "Regent's Park Road", Ref: "A598"},
		{Name: ""},
		{Name: "Hendon Lane", Ref: "A5000"},
		{Name: "Great North Way", Ref: "A1"},
		{Name: "", Ref: "M1"},
		{Name: "", Ref: "M1"},
		{Name: "North Orbital Road", Ref: "A405"},
		{Name: "North Orbital Road", Ref: "A405"},
		{Name: "Tippendell Lane"},
		{Name: "Park Street", Ref: "A5183"},
	}
	got := ExtractRoadNames(steps)
	want := []string{"A598", "A5000", "A1", "M1", "A405", "A5183"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v (unique, in route order)", got, want)
	}
}

func TestExtractRoadNamesMultipleRefs(t *testing.T) {
	steps := []geo.Step{
		{Name: "", Ref: "A1(M);A1"},
		{Name: "", Ref: "A1"},
		{Name: "M6 Toll"},
		{Name: "", Ref: "E15;M1"},
	}
	got := ExtractRoadNames(steps)
	want := []string{"A1(M)", "A1", "M6 Toll", "M1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v (A1(M) and A1 kept separate; non-UK refs dropped)", got, want)
	}
}

// TestExtractRoadNamesRefWinsOverName is a regression test: a step whose Ref
// is one road and whose Name spells out another must yield only the Ref.
func TestExtractRoadNamesRefWinsOverName(t *testing.T) {
	got := ExtractRoadNames([]geo.Step{{Name: "M6", Ref: "M6 Toll"}, {Name: "A1(M)", Ref: "E15"}})
	want := []string{"M6 Toll", "A1(M)"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v (Name only when Ref has no road)", got, want)
	}
}
