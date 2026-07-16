package core

import "testing"

// Cyrb53 is a byte-exact port of the original TypeScript hash. These vectors were
// produced by that implementation and pin the port's output (32-bit Math.imul
// wrap, charCodeAt UTF-16 code units, base-36 encoding). Any drift here means the
// hash — and thus every data-odoc-aid — has changed.
func TestCyrb53Vectors(t *testing.T) {
	cases := []struct {
		input string
		seed  uint32
		want  string
	}{
		{"", 0, "wvjl67o803"},
		{"a", 0, "262p94epp1d"},
		{"hello world", 0, "w38l36pd04"},
		{"The quick brown fox jumps over the lazy dog.", 0, "qml8mx5qqz"},
		{"你好，世界", 0, "2mzaffzikv"},            // multi-byte UTF-8 / BMP
		{"日本語のテキスト", 0, "6w9ojiihhp"},         // more BMP CJK
		{"🚀🔥✅ emoji mix 🎉", 0, "1m6zub30lid"}, // astral (surrogate pairs)
		{"𝕌𝕟𝕚𝕔𝕠𝕕𝕖 astral plane", 0, "1v994wbmw7m"},
		{"mixed 中文 and English 123", 0, "m9u63s6blh"},
		{"<svg viewBox=\"0 0 24 24\"><path d=\"M3 8.5\"/></svg>", 0, "29vp5jpv89k"},
		{"hello world", 42, "2cnqnm2cxdq"}, // non-zero seed
		{"你好", 7, "1ozg6u5y19y"},
	}
	for _, c := range cases {
		if got := Cyrb53(c.input, c.seed); got != c.want {
			t.Errorf("Cyrb53(%q, %d) = %q, want %q", c.input, c.seed, got, c.want)
		}
	}
}

// A long single-byte run exercises the multi-round accumulation without overflow
// surprises (the TS used Math.imul, which wraps at 32 bits).
func TestCyrb53LongInput(t *testing.T) {
	in := ""
	for range 1000 {
		in += "x"
	}
	if got := Cyrb53(in, 0); got != "21ded0ooxff" {
		t.Errorf("Cyrb53(1000×'x', 0) = %q, want 21ded0ooxff", got)
	}
}

// The hash must be seed-sensitive (the vector table above already pins exact,
// deterministic outputs).
func TestCyrb53SeedSensitive(t *testing.T) {
	if Cyrb53("stable", 0) == Cyrb53("stable", 1) {
		t.Error("Cyrb53 ignored the seed")
	}
}
