package registry

import (
	"strings"
)

// Platform is an OS/architecture/variant triple as it appears in a
// manifest index's `platform` object.
type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

// String renders "linux/amd64" or, when a variant is present,
// "linux/arm/v7" — the same form `docker pull --platform` accepts.
func (p Platform) String() string {
	parts := make([]string, 0, 3)
	if p.OS != "" {
		parts = append(parts, p.OS)
	}
	if p.Architecture != "" {
		parts = append(parts, p.Architecture)
	}
	if p.Variant != "" {
		parts = append(parts, p.Variant)
	}
	return strings.Join(parts, "/")
}

// IsZero reports whether the platform carries no information.
func (p Platform) IsZero() bool { return p.OS == "" && p.Architecture == "" }

// Equal compares two platforms, treating a missing variant as a wildcard
// on the *query* side only — see Match.
func (p Platform) Equal(o Platform) bool {
	return strings.EqualFold(p.OS, o.OS) &&
		strings.EqualFold(p.Architecture, o.Architecture) &&
		strings.EqualFold(p.Variant, o.Variant)
}

// ArchAliases maps a canonical architecture name to every spelling seen in
// the wild. Docker Hub indexes use both image-variant names (arm64v8) and
// kernel names (aarch64) for the same thing.
var ArchAliases = map[string][]string{
	"amd64":    {"amd64", "x86_64", "x64", "x86-64"},
	"arm64":    {"arm64", "arm64v8", "aarch64"},
	"arm":      {"arm", "arm32", "arm32v7", "arm32v6", "arm32v5", "armhf", "armv7", "armv6"},
	"386":      {"386", "i386", "x86", "i686"},
	"ppc64le":  {"ppc64le", "powerpc64le"},
	"riscv64":  {"riscv64"},
	"s390x":    {"s390x"},
	"mips64le": {"mips64le"},
}

// CanonicalArch maps any known spelling to its canonical name. Unknown
// names are returned lowercased and unchanged.
func CanonicalArch(arch string) string {
	a := strings.ToLower(strings.TrimSpace(arch))
	if a == "" {
		return ""
	}
	for canonical, names := range ArchAliases {
		for _, n := range names {
			if a == n {
				return canonical
			}
		}
	}
	return a
}

// aliasGroup returns every spelling that means the same architecture.
func aliasGroup(arch string) []string {
	canonical := CanonicalArch(arch)
	if names, ok := ArchAliases[canonical]; ok {
		return names
	}
	return []string{canonical}
}

// ParsePlatform parses "amd64", "linux/amd64", "linux/arm/v7" or
// "linux/arm64/v8". A bare architecture defaults the OS to linux.
func ParsePlatform(s string) Platform {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return Platform{OS: "linux", Architecture: "amd64"}
	}
	parts := strings.Split(s, "/")
	switch len(parts) {
	case 1:
		return Platform{OS: "linux", Architecture: parts[0]}
	case 2:
		return Platform{OS: parts[0], Architecture: parts[1]}
	default:
		// "linux/arm/v7" — anything past the variant is ignored.
		return Platform{OS: parts[0], Architecture: parts[1], Variant: strings.Join(parts[2:], "/")}
	}
}

// ResolveArch picks the platform in available that best matches target.
//
// Matching order, first hit wins:
//
//  1. full "os/arch[/variant]" equality when target names an OS
//  2. exact architecture equality
//  3. canonical alias-group membership (arm64v8 == aarch64 == arm64)
//  4. variant equality within the same architecture (arm/v7)
//  5. bidirectional substring (riscv64 vs riscv64gc)
//
// The OS must match. An index routinely lists windows/darwin entries next to
// linux ones, and a linux/amd64 request must never be served a windows/amd64
// image: the tar would be unusable on the host it was fetched for and nothing
// downstream would notice. The old code got this safety by having
// Manifest.Platforms() drop every non-linux entry BEFORE resolution, which also
// meant a windows-only image could not be pulled at all and its available list
// printed empty. Reporting and resolution are now separated: Platforms() is
// truthful and the guard lives here.
func ResolveArch(target string, available []Platform) (Platform, bool) {
	if len(available) == 0 {
		return Platform{}, false
	}
	want := ParsePlatform(target)
	wantOS := want.OS
	wantArch := CanonicalArch(want.Architecture)
	wantVariant := strings.ToLower(want.Variant)

	// Cross-OS candidates are not candidates. An entry with no OS at all is
	// kept: it is degenerate metadata, not a conflicting claim.
	candidates := make([]Platform, 0, len(available))
	for _, p := range available {
		if p.OS == "" || strings.EqualFold(p.OS, wantOS) {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		return Platform{}, false
	}

	// 1. Exact "os/arch[/variant]".
	for _, p := range candidates {
		if strings.EqualFold(p.OS, wantOS) &&
			CanonicalArch(p.Architecture) == wantArch &&
			(wantVariant == "" || strings.EqualFold(p.Variant, wantVariant)) {
			return p, true
		}
	}

	// 2. Architecture match by alias group, i.e. arm64v8 == aarch64 == arm64.
	//
	// When the target names a VARIANT, an entry with a different variant is
	// not a match: linux/arm/v7 and linux/arm/v6 are different ABIs, and
	// silently handing back v6 for a v7 request produces an image the target
	// cannot run. Better to report "not found" and list what IS available.
	for _, p := range candidates {
		if CanonicalArch(p.Architecture) != wantArch {
			continue
		}
		if wantVariant != "" && p.Variant != "" && !strings.EqualFold(p.Variant, wantVariant) {
			continue
		}
		return p, true
	}
	for _, p := range candidates {
		if wantVariant != "" && p.Variant != "" && !strings.EqualFold(p.Variant, wantVariant) {
			continue
		}
		for _, alias := range aliasGroup(want.Architecture) {
			if strings.EqualFold(p.Architecture, alias) {
				return p, true
			}
		}
	}

	// 3. The target asked for a specific variant and no entry matched it
	// (step 2 already rejected the wrong-variant entries).
	if wantVariant != "" {
		return Platform{}, false
	}

	// 4. Fuzzy substring, as a last resort (riscv64 / riscv64gc).
	for _, p := range candidates {
		pa := strings.ToLower(p.Architecture)
		if strings.Contains(pa, wantArch) || strings.Contains(wantArch, pa) {
			return p, true
		}
	}
	return Platform{}, false
}

// DedupePlatforms removes duplicate platform entries while preserving the
// manifest's original order.
func DedupePlatforms(in []Platform) []Platform {
	seen := make(map[string]bool, len(in))
	out := make([]Platform, 0, len(in))
	for _, p := range in {
		key := strings.ToLower(p.String())
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, p)
	}
	return out
}

// PlatformStrings renders a platform list for error messages and prompts.
func PlatformStrings(in []Platform) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		out = append(out, p.String())
	}
	return out
}
