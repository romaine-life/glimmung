package steperr

import "testing"

func TestNormalizeCoercesUnknownLayerToHarness(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", LayerHarness},
		{"bogus", LayerHarness},
		{"  host ", LayerHost},
		{LayerModel, LayerModel},
	}
	for _, tc := range cases {
		got := Block{Layer: tc.in, Message: "m"}.Normalize().Layer
		if got != tc.want {
			t.Fatalf("Normalize(%q).Layer = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidRequiresMessage(t *testing.T) {
	if (&Block{Layer: LayerHost}).Valid() {
		t.Fatal("block without a message must be invalid")
	}
	if !(&Block{Message: "boom"}).Valid() {
		t.Fatal("block with a message must be valid regardless of layer")
	}
	var nilBlock *Block
	if nilBlock.Valid() {
		t.Fatal("nil block is invalid")
	}
}

func TestSuspectedCause(t *testing.T) {
	cases := map[string]string{
		LayerHarness: "harness_flake",
		LayerHost:    "environment_config",
		LayerModel:   "code_bug",
		"unknown":    "harness_flake",
	}
	for layer, want := range cases {
		if got := SuspectedCause(layer); got != want {
			t.Fatalf("SuspectedCause(%q) = %q, want %q", layer, got, want)
		}
	}
}
