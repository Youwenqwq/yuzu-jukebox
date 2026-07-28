package shortcode

import "testing"

func TestCodecFormatsAndNormalizesHumanInput(t *testing.T) {
	canonical, ok := Encode([]byte{0, 0, 0, 0, 0, 0, 0, 0})
	if !ok || canonical != "000000000000" {
		t.Fatalf("Encode() = %q, %v", canonical, ok)
	}
	display, ok := Format(canonical)
	if !ok || display != "0000-0000-0000" {
		t.Fatalf("Format() = %q, %v", display, ok)
	}
	normalized, ok := Normalize(" 0000  -  0000-0000 ")
	if !ok || normalized != canonical {
		t.Fatalf("Normalize() = %q, %v", normalized, ok)
	}
	if _, ok := Normalize("0000-0000-000I"); ok {
		t.Fatal("ambiguous alphabet character was accepted")
	}
}
