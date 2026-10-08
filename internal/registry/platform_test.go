package registry

import "testing"

// TestParsePlatform pins the accepted --platform spellings: a bare
// architecture defaults the OS to linux, and "linux/arm/v7" keeps its
// variant so the arch picker can offer ARMv7 and ARMv6 separately.
func TestParsePlatform(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want Platform
	}{
		{"bare architecture", "amd64", Platform{OS: "linux", Architecture: "amd64"}},
		{"os/arch", "linux/amd64", Platform{OS: "linux", Architecture: "amd64"}},
		{"os/arch/variant", "linux/arm/v7", Platform{OS: "linux", Architecture: "arm", Variant: "v7"}},
		{"empty defaults to linux/amd64", "", Platform{OS: "linux", Architecture: "amd64"}},
		{"whitespace defaults to linux/amd64", "   ", Platform{OS: "linux", Architecture: "amd64"}},
		{"uppercase is lowercased", "Linux/ARM64", Platform{OS: "linux", Architecture: "arm64"}},
		{"os/arch/variant/extra joins the tail", "linux/arm/v7/v8", Platform{OS: "linux", Architecture: "arm", Variant: "v7/v8"}},
		{"windows is preserved", "windows/amd64", Platform{OS: "windows", Architecture: "amd64"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParsePlatform(tc.in); got != tc.want {
				t.Errorf("ParsePlatform(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

// TestPlatformString protects the round trip docker pull --platform and the
// manifest index need: "linux/arm/v7" must not lose its variant.
func TestPlatformString(t *testing.T) {
	tests := []struct {
		in   Platform
		want string
	}{
		{Platform{OS: "linux", Architecture: "amd64"}, "linux/amd64"},
		{Platform{OS: "linux", Architecture: "arm", Variant: "v7"}, "linux/arm/v7"},
		{Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}, "linux/arm64/v8"},
		{Platform{OS: "windows", Architecture: "amd64"}, "windows/amd64"},
		{Platform{Architecture: "amd64"}, "amd64"},
		{Platform{OS: "linux"}, "linux"},
		{Platform{}, ""},
	}
	for _, tc := range tests {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("Platform%+v.String() = %q, want %q", tc.in, got, tc.want)
		}
	}

	p := ParsePlatform("linux/arm/v7")
	if got := p.String(); got != "linux/arm/v7" {
		t.Errorf("round trip: ParsePlatform(%q).String() = %q", "linux/arm/v7", got)
	}
}

// TestCanonicalArch protects the alias table: Docker Hub publishes the same
// architecture under several spellings and they must all collapse to one.
func TestCanonicalArch(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"arm64v8", "arm64"},
		{"aarch64", "arm64"},
		{"arm64", "arm64"},
		{"x86_64", "amd64"},
		{"x64", "amd64"},
		{"amd64", "amd64"},
		{"armhf", "arm"},
		{"armv7", "arm"},
		{"i386", "386"},
		{"ppc64le", "ppc64le"},
		{"powerpc64le", "ppc64le"},
		{"riscv64", "riscv64"},
		{"s390x", "s390x"},
		{"ARM64V8", "arm64"},
		{"  amd64  ", "amd64"},
		{"unknown", "unknown"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := CanonicalArch(tc.in); got != tc.want {
			t.Errorf("CanonicalArch(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestResolveArch is the guard that stops the puller exporting an image for
// the wrong CPU: it is also where the "never silently ship the wrong ABI"
// rule for ARM variants lives.
func TestResolveArch(t *testing.T) {
	amd64 := Platform{OS: "linux", Architecture: "amd64"}
	arm64 := Platform{OS: "linux", Architecture: "arm64"}
	arm64v8 := Platform{OS: "linux", Architecture: "arm64v8"}
	armv7 := Platform{OS: "linux", Architecture: "arm", Variant: "v7"}
	armv6 := Platform{OS: "linux", Architecture: "arm", Variant: "v6"}
	winAmd64 := Platform{OS: "windows", Architecture: "amd64"}

	t.Run("exact match", func(t *testing.T) {
		got, ok := ResolveArch("linux/amd64", []Platform{arm64, amd64})
		if !ok || got != amd64 {
			t.Fatalf("ResolveArch(linux/amd64) = %+v, %v; want %+v", got, ok, amd64)
		}
	})

	t.Run("arm64 matches an arm64v8 entry", func(t *testing.T) {
		got, ok := ResolveArch("arm64", []Platform{arm64v8})
		if !ok || got != arm64v8 {
			t.Fatalf("ResolveArch(arm64) = %+v, %v; want the arm64v8 entry", got, ok)
		}
	})

	t.Run("arm/v7 prefers its own variant over v6", func(t *testing.T) {
		got, ok := ResolveArch("linux/arm/v7", []Platform{armv6, armv7})
		if !ok {
			t.Fatal("ResolveArch(linux/arm/v7) refused to pick the v7 entry")
		}
		if got != armv7 {
			t.Fatalf("ResolveArch(linux/arm/v7) = %+v, want the v7 entry (not v6)", got)
		}
	})

	// Regression for the "don't silently ship the wrong ABI" rule. The
	// implementation documents this rejection in its step-4 comment
	// ("reject rather than silently shipping the wrong ABI") but step 2
	// returns any candidate with a matching canonical architecture before
	// the variant is ever considered, so ARMv6-only indexes resolve to
	// ARMv6 and the wrong binaries land in the tar.
	t.Run("arm/v7 refused when only v6 exists", func(t *testing.T) {
		got, ok := ResolveArch("linux/arm/v7", []Platform{armv6})
		if ok {
			t.Fatalf("ResolveArch(linux/arm/v7) accepted %+v from a v6-only index; "+
				"want refusal (platform.go:142-148 shadows the variant check at platform.go:159-166)", got)
		}
	})

	t.Run("windows entries are never selected", func(t *testing.T) {
		// A mixed index: the arch matches only the windows entry, which
		// must not be chosen for a linux host.
		got, ok := ResolveArch("amd64", []Platform{winAmd64, arm64})
		if ok {
			t.Fatalf("ResolveArch(amd64) picked %+v from a windows + linux/arm64 index; want refusal", got)
		}
	})

	t.Run("windows entry ignored when a linux entry matches", func(t *testing.T) {
		got, ok := ResolveArch("amd64", []Platform{winAmd64, amd64})
		if !ok || got != amd64 {
			t.Fatalf("ResolveArch(amd64) = %+v, %v; want the linux/amd64 entry", got, ok)
		}
	})

	t.Run("full linux/amd64 form", func(t *testing.T) {
		got, ok := ResolveArch("linux/amd64", []Platform{arm64, amd64})
		if !ok || got.Architecture != "amd64" {
			t.Fatalf("ResolveArch(linux/amd64) = %+v, %v", got, ok)
		}
	})

	t.Run("unknown architecture", func(t *testing.T) {
		if got, ok := ResolveArch("ppc64le", []Platform{amd64, arm64}); ok {
			t.Fatalf("ResolveArch(ppc64le) = %+v, want refusal", got)
		}
	})

	t.Run("empty available list", func(t *testing.T) {
		if got, ok := ResolveArch("amd64", nil); ok {
			t.Fatalf("ResolveArch(amd64, nil) = %+v, want refusal", got)
		}
		if got, ok := ResolveArch("amd64", []Platform{}); ok {
			t.Fatalf("ResolveArch(amd64, []) = %+v, want refusal", got)
		}
	})

	t.Run("empty target means linux/amd64", func(t *testing.T) {
		got, ok := ResolveArch("", []Platform{arm64, amd64})
		if !ok || got != amd64 {
			t.Fatalf("ResolveArch(\"\", ...) = %+v, %v; want linux/amd64", got, ok)
		}
	})
}

// TestDedupePlatforms protects the arch picker: an index that lists the same
// platform twice must render one row, in the manifest's own order.
func TestDedupePlatforms(t *testing.T) {
	amd64 := Platform{OS: "linux", Architecture: "amd64"}
	arm64 := Platform{OS: "linux", Architecture: "arm64"}
	armv7 := Platform{OS: "linux", Architecture: "arm", Variant: "v7"}

	got := DedupePlatforms([]Platform{amd64, arm64, amd64, armv7, arm64, {}})
	want := []Platform{amd64, arm64, armv7}
	if len(got) != len(want) {
		t.Fatalf("DedupePlatforms = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("DedupePlatforms[%d] = %+v, want %+v (order must be preserved)", i, got[i], want[i])
		}
	}

	if got := DedupePlatforms(nil); len(got) != 0 {
		t.Errorf("DedupePlatforms(nil) = %+v, want empty", got)
	}

	// Variant is part of the identity: arm/v6 and arm/v7 are distinct.
	got = DedupePlatforms([]Platform{armv7, Platform{OS: "linux", Architecture: "arm", Variant: "v6"}})
	if len(got) != 2 {
		t.Errorf("DedupePlatforms collapsed distinct variants: %+v", got)
	}

	if s := PlatformStrings([]Platform{amd64, armv7}); len(s) != 2 || s[0] != "linux/amd64" || s[1] != "linux/arm/v7" {
		t.Errorf("PlatformStrings = %v", s)
	}
}
