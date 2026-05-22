//go:build windows

package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// ---------------------------------------------------------------------------
// 1. parseIndexSpec
// ---------------------------------------------------------------------------

func TestParseIndexSpec(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []int
	}{
		{
			name:  "single index",
			input: "5",
			want:  []int{5},
		},
		{
			name:  "range",
			input: "1-3",
			want:  []int{1, 2, 3},
		},
		{
			name:  "comma list",
			input: "1,3,5",
			want:  []int{1, 3, 5},
		},
		{
			name:  "mixed range and list",
			input: "1-3,7",
			want:  []int{1, 2, 3, 7},
		},
		{
			name:  "complex mixed",
			input: "5-7,1,10-12",
			want:  []int{5, 6, 7, 1, 10, 11, 12},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseIndexSpec(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("parseIndexSpec(%q) = %v, want %v", tt.input, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("parseIndexSpec(%q)[%d] = %d, want %d", tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 2. parseHexOffset
// ---------------------------------------------------------------------------

func TestParseHexOffset(t *testing.T) {
	// two's complement of 416 on the target platform
	negOf416 := uintptr(^uint64(416) + 1)

	tests := []struct {
		name  string
		input string
		want  uintptr
	}{
		{"0xD8", "0xD8", 216},
		{"-0x1A0 negative twos complement", "-0x1A0", negOf416},
		{"+1A0 legacy plus prefix", "+1A0", 416},
		{"1A0 bare hex", "1A0", 416},
		{"0x0 zero", "0x0", 0},
		{"0 bare zero", "0", 0},
		{"0x100", "0x100", 256},
		{"uppercase 0X", "0X1A0", 416},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseHexOffset(tt.input)
			if got != tt.want {
				t.Errorf("parseHexOffset(%q) = 0x%X (%d), want 0x%X (%d)",
					tt.input, got, int64(got), tt.want, int64(tt.want))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 3. quoteHexLiterals
// ---------------------------------------------------------------------------

func TestQuoteHexLiterals(t *testing.T) {
	tests := []struct {
		name  string
		input string
		// want: the expected substring or the full output
		containsWant string // if non-empty, just check Contains
		wantExact    string // if non-empty, check exact equality
	}{
		{
			name:         "positive hex gets quoted",
			input:        `0xA945820`,
			wantExact:    `"0xA945820"`,
		},
		{
			name:         "negative hex gets quoted",
			input:        `-0x1A0`,
			wantExact:    `"-0x1A0"`,
		},
		{
			name:         "zero hex gets quoted",
			input:        `0x0`,
			wantExact:    `"0x0"`,
		},
		{
			name:  "hex inside string literal is NOT replaced",
			input: `"label": "value 0xDEAD"`,
			// the entire thing should be passed through unchanged
			wantExact: `"label": "value 0xDEAD"`,
		},
		{
			name:      "plain decimal false unchanged",
			input:     `false`,
			wantExact: `false`,
		},
		{
			name:      "plain decimal true unchanged",
			input:     `true`,
			wantExact: `true`,
		},
		{
			name:      "plain decimal 42 unchanged",
			input:     `42`,
			wantExact: `42`,
		},
		{
			name:  "json field with hex offset gets quoted",
			input: `"base_offset": 0x1A2B3C`,
			// The value should become quoted; the key (inside "") unchanged
			containsWant: `"0x1A2B3C"`,
		},
		{
			name:  "negative offset in offsets array",
			input: `"offsets": [-0x10, 0x20]`,
			// both should be quoted
			containsWant: `"-0x10"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(quoteHexLiterals([]byte(tt.input)))
			if tt.wantExact != "" && got != tt.wantExact {
				t.Errorf("quoteHexLiterals(%q)\n  got  = %q\n  want = %q", tt.input, got, tt.wantExact)
			}
			if tt.containsWant != "" && !strings.Contains(got, tt.containsWant) {
				t.Errorf("quoteHexLiterals(%q)\n  got    = %q\n  missing = %q", tt.input, got, tt.containsWant)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 4. SavePointerResults / LoadPointerResults round-trip
// ---------------------------------------------------------------------------

func TestPointerResultsRoundTrip(t *testing.T) {
	// Negative offset for testing: two's complement of 0x1A0
	negOff := uintptr(^uint64(0x1A0) + 1)

	chains := []PointerResult{
		{
			Chain: PointerChain{
				BaseModule: "game.exe",
				BaseOffset: 0xA945820,
				Offsets:    []uintptr{0xD8, 0xB0, 0x0, negOff},
			},
			Label: "HP",
		},
		{
			Chain: PointerChain{
				BaseModule: "engine.dll",
				BaseOffset: 0x100,
				Offsets:    []uintptr{0x0},
			},
			Label: "stamina",
		},
		{
			Chain: PointerChain{
				BaseModule: "game.exe",
				BaseOffset: 0xFF00,
				Offsets:    []uintptr{0x8, 0x10, 0x18},
			},
			Label: "",
		},
	}

	tmpFile := filepath.Join(t.TempDir(), "test_chains.json")

	// Save — pass windows.Handle(0) so no live-process verification is attempted,
	// and nil modules so no game-exe lookup is done.
	if err := SavePointerResults(tmpFile, chains, "game.exe", false, TypeFloat32, windows.Handle(0), nil); err != nil {
		t.Fatalf("SavePointerResults failed: %v", err)
	}

	// Load back
	prf, loaded, err := LoadPointerResults(tmpFile)
	if err != nil {
		t.Fatalf("LoadPointerResults failed: %v", err)
	}

	if prf == nil {
		t.Fatal("expected non-nil PointerResultsFile")
	}
	if len(loaded) != len(chains) {
		t.Fatalf("loaded %d chains, want %d", len(loaded), len(chains))
	}

	for i, orig := range chains {
		got := loaded[i]

		if got.BaseModule != orig.Chain.BaseModule {
			t.Errorf("[%d] BaseModule = %q, want %q", i, got.BaseModule, orig.Chain.BaseModule)
		}
		if got.BaseOffset != orig.Chain.BaseOffset {
			t.Errorf("[%d] BaseOffset = 0x%X, want 0x%X", i, got.BaseOffset, orig.Chain.BaseOffset)
		}
		if len(got.Offsets) != len(orig.Chain.Offsets) {
			t.Errorf("[%d] len(Offsets) = %d, want %d", i, len(got.Offsets), len(orig.Chain.Offsets))
			continue
		}
		for j, o := range orig.Chain.Offsets {
			if got.Offsets[j] != o {
				t.Errorf("[%d] Offsets[%d] = 0x%X (%d), want 0x%X (%d)",
					i, j, got.Offsets[j], int64(got.Offsets[j]), o, int64(o))
			}
		}
	}

	// Labels are stored in the file but loaded into PointerResultsFile.Chains, not PointerChain.
	for i, orig := range chains {
		if prf.Chains[i].Label != orig.Label {
			t.Errorf("[%d] Label = %q, want %q", i, prf.Chains[i].Label, orig.Label)
		}
	}
}

// ---------------------------------------------------------------------------
// 5. PointerChain.Key() and String()
// ---------------------------------------------------------------------------

func TestPointerChainKey(t *testing.T) {
	c := PointerChain{
		BaseModule: "game.exe",
		BaseOffset: 0x1000,
		Offsets:    []uintptr{0xA0, 0xB0},
	}

	t.Run("Key is deterministic", func(t *testing.T) {
		k1 := c.Key()
		k2 := c.Key()
		if k1 != k2 {
			t.Errorf("Key() not deterministic: %q vs %q", k1, k2)
		}
	})

	t.Run("Key contains module and offset", func(t *testing.T) {
		k := c.Key()
		if !strings.Contains(k, "game.exe") {
			t.Errorf("Key() = %q, want to contain module name", k)
		}
		if !strings.Contains(k, "1000") {
			t.Errorf("Key() = %q, want to contain base offset hex", k)
		}
	})

	t.Run("Different offsets produce different keys", func(t *testing.T) {
		c2 := PointerChain{
			BaseModule: "game.exe",
			BaseOffset: 0x1000,
			Offsets:    []uintptr{0xA0, 0xC0}, // different last offset
		}
		if c.Key() == c2.Key() {
			t.Error("different chains should produce different keys")
		}
	})

	t.Run("Same chain produces same key for intersection logic", func(t *testing.T) {
		c3 := PointerChain{
			BaseModule: "game.exe",
			BaseOffset: 0x1000,
			Offsets:    []uintptr{0xA0, 0xB0},
		}
		if c.Key() != c3.Key() {
			t.Errorf("identical chains produce different keys: %q vs %q", c.Key(), c3.Key())
		}
	})
}

func TestPointerChainString(t *testing.T) {
	negOff := uintptr(^uint64(0x10) + 1) // -0x10 two's complement

	tests := []struct {
		name    string
		chain   PointerChain
		wantSub []string // substrings that must appear
	}{
		{
			name: "positive offsets use +",
			chain: PointerChain{
				BaseModule: "game.exe",
				BaseOffset: 0x100,
				Offsets:    []uintptr{0xA0, 0xB0},
			},
			wantSub: []string{"game.exe", "+A0", "+B0"},
		},
		{
			name: "negative offset uses -",
			chain: PointerChain{
				BaseModule: "game.exe",
				BaseOffset: 0x100,
				Offsets:    []uintptr{negOff},
			},
			wantSub: []string{"-10"},
		},
		{
			name: "zero offset",
			chain: PointerChain{
				BaseModule: "engine.dll",
				BaseOffset: 0x0,
				Offsets:    []uintptr{0x0},
			},
			wantSub: []string{"engine.dll"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.chain.String()
			for _, sub := range tt.wantSub {
				if !strings.Contains(s, sub) {
					t.Errorf("String() = %q, want to contain %q", s, sub)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 6. decodeValue / encodeValue round-trips
// ---------------------------------------------------------------------------

func TestEncodeDecodeRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		dt      DataType
		valStr  string
	}{
		{"Float32 100.0",   TypeFloat32, "100"},
		{"Float32 -3.14",   TypeFloat32, "-3.14"},
		{"Float32 0",       TypeFloat32, "0"},
		{"Float64 1234567", TypeFloat64, "1234567"},
		{"Float64 -0.001",  TypeFloat64, "-0.001"},
		{"Int32 42",        TypeInt32,   "42"},
		{"Int32 -1",        TypeInt32,   "-1"},
		{"Int32 0",         TypeInt32,   "0"},
		{"Int64 9999999",   TypeInt64,   "9999999"},
		{"Int64 -100",      TypeInt64,   "-100"},
		{"UInt32 0",        TypeUInt32,  "0"},
		{"UInt32 4294967295", TypeUInt32, "4294967295"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := encodeValue(tt.dt, tt.valStr)
			if err != nil {
				t.Fatalf("encodeValue(%v, %q) error: %v", tt.dt, tt.valStr, err)
			}
			decoded := decodeValue(tt.dt, encoded)

			// For floats, compare numerically to handle formatting differences like
			// "100" vs "100" but also "-3.14" vs "-3.14" (fmt.Sprintf("%g") normalises).
			switch tt.dt {
			case TypeFloat32, TypeFloat64:
				var wantF, gotF float64
				fmt.Sscanf(tt.valStr, "%f", &wantF)
				fmt.Sscanf(decoded, "%f", &gotF)
				if math.Abs(gotF-wantF) > math.Abs(wantF)*1e-5+1e-9 {
					t.Errorf("float round-trip: encode(%q) -> decode = %q, want ~%q", tt.valStr, decoded, tt.valStr)
				}
			default:
				if decoded != tt.valStr {
					t.Errorf("round-trip: encode(%q) -> decode = %q, want %q", tt.valStr, decoded, tt.valStr)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 7. dataTypeSize
// ---------------------------------------------------------------------------

func TestDataTypeSize(t *testing.T) {
	tests := []struct {
		dt   DataType
		want int
	}{
		{TypeInt8,    1},
		{TypeUInt8,   1},
		{TypeInt16,   2},
		{TypeUInt16,  2},
		{TypeInt32,   4},
		{TypeUInt32,  4},
		{TypeFloat32, 4},
		{TypeInt64,   8},
		{TypeUInt64,  8},
		{TypeFloat64, 8},
		// str/bytes default to 4 per dataTypeSize's default branch
		{TypeString,  4},
		{TypeBytes,   4},
	}

	for _, tt := range tests {
		t.Run(dataTypeName(tt.dt), func(t *testing.T) {
			got := dataTypeSize(tt.dt)
			if got != tt.want {
				t.Errorf("dataTypeSize(%v) = %d, want %d", tt.dt, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 8. fileCompletions
// ---------------------------------------------------------------------------

func TestFileCompletions(t *testing.T) {
	// Create a temp dir with known files
	tmp := t.TempDir()

	files := []string{"s1.pmap", "s2.pmap", "chains.json"}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(tmp, f), []byte{}, 0644); err != nil {
			t.Fatalf("setup: create %s: %v", f, err)
		}
	}

	// fileCompletions reads the current directory, so we must chdir to tmp.
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir to tmp: %v", err)
	}
	defer os.Chdir(orig)

	tests := []struct {
		name    string
		partial string
		want    []string // expected completions (sorted)
	}{
		{
			name:    "prefix s matches two pmaps",
			partial: "s",
			want:    []string{"s1.pmap", "s2.pmap"},
		},
		{
			name:    "prefix s1 matches one",
			partial: "s1",
			want:    []string{"s1.pmap"},
		},
		{
			name:    "prefix ch matches json",
			partial: "ch",
			want:    []string{"chains.json"},
		},
		{
			name:    "no match",
			partial: "nope",
			want:    nil,
		},
		{
			name:    "case sensitive uppercase S no match",
			partial: "S",
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fileCompletions(tt.partial)
			if len(got) != len(tt.want) {
				t.Fatalf("fileCompletions(%q) = %v, want %v", tt.partial, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("fileCompletions(%q)[%d] = %q, want %q", tt.partial, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 9. guessType
// ---------------------------------------------------------------------------

func makeF32Bytes(v float32) []byte {
	buf := make([]byte, 8) // 8 bytes so guessType can sniff full window
	binary.LittleEndian.PutUint32(buf, math.Float32bits(v))
	return buf
}

func makeI32Bytes(v int32) []byte {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint32(buf, uint32(v))
	return buf
}

func TestGuessType(t *testing.T) {
	// handle = 0 — pointer reads will always fail, that's expected and fine.
	handle := windows.Handle(0)

	tests := []struct {
		name           string
		peekBytes      []byte
		wantName       string
		wantConfGte    float64 // confidence must be >= this
	}{
		{
			name:        "all zeros → zero",
			peekBytes:   make([]byte, 8),
			wantName:    "zero",
			wantConfGte: 1.0,
		},
		{
			name:        "f32 = 100.0 with zero-padded upper 4 bytes → f32",
			peekBytes:   makeF32Bytes(100.0),
			wantName:    "f32",
			wantConfGte: 0.5,
		},
		{
			name:        "i32 = 42 → i32",
			peekBytes:   makeI32Bytes(42),
			wantName:    "i32",
			wantConfGte: 0.4,
		},
		{
			// NaN float bits: 0x7FC00000 (quiet NaN, standard).
			// guessType rejects NaN f32; the value is too small to be a pointer;
			// it's a small positive i32 (0x7FC00000 = 2143289344 > 1e9) so it also
			// fails the i64 check, but i32 check fires. With value > 1e6 the base
			// conf is 0.45, no bonus. So it might fall back to i8 if nothing else
			// wins — let's just assert it does NOT guess f32.
			name:        "NaN f32 bytes → not f32",
			peekBytes:   []byte{0x00, 0x00, 0xC0, 0x7F, 0x00, 0x00, 0x00, 0x00},
			wantName:    "", // we only check it's NOT f32 below
			wantConfGte: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := guessType(tt.peekBytes, handle, false)

			// Special case: NaN test — just assert it's not f32
			if tt.wantName == "" {
				if got.name == "f32" {
					t.Errorf("guessType(NaN bytes) = f32, expected anything but f32")
				}
				return
			}

			if got.name != tt.wantName {
				t.Errorf("guessType name = %q, want %q (confidence=%.2f)", got.name, tt.wantName, got.confidence)
			}
			if got.confidence < tt.wantConfGte {
				t.Errorf("guessType confidence = %.2f, want >= %.2f (name=%q)", got.confidence, tt.wantConfGte, got.name)
			}
		})
	}
}
