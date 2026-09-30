package size

import "testing"

func TestParse(t *testing.T) {
	cases := map[string]uint64{
		"1000GB": 1000 << 30,
		"1GB":    1 << 30,
		"1gb":    1 << 30,
		"512MB":  512 << 20,
		"1TB":    1 << 40,
		"1.5GB":  uint64(1.5 * (1 << 30)),
		"1024":   1024,
		"2K":     2 << 10,
	}
	for in, want := range cases {
		got, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("Parse(%q) = %d, want %d", in, got, want)
		}
	}
	if _, err := Parse("10XB"); err == nil {
		t.Fatal("expected error for bad unit")
	}
}

func TestFormat(t *testing.T) {
	if Format(1<<30) != "1.00 GB" {
		t.Fatalf("Format(1GB) = %q", Format(1<<30))
	}
}
