package registry

import "testing"

// TestPullHostForSearchSource pins the fix for the worst kind of bug: a search
// that silently downloads from somewhere else.
//
// Searching "nginx" on the 1ms accelerator returns "library/nginx" — a Docker
// Hub repository NAME. The pull used to parse that name bare and reach
// registry-1.docker.io, i.e. exactly the host the user chose 1ms to avoid, and
// on a network where Docker Hub is unreachable the download simply failed with
// a connection error that named the wrong registry.
func TestPullHostForSearchSource(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   string
		why    string
	}{
		{"1ms", "docker.1ms.run", "a Docker Hub accelerator serves its own search results"},
		{"1MS", "docker.1ms.run", "ids are matched case-insensitively"},
		{"  1ms  ", "docker.1ms.run", "and after trimming"},
		{"1ms 加速源", "docker.1ms.run", "the seeded row stores the NAME, not the id"},
		{"quay", "quay.io", "an own-namespace registry serves its own results"},
		{"Quay.io (Red Hat)", "quay.io", "matched by name too"},
		{"dockerhub", "", "Docker Hub's own search says nothing about which accelerator is preferred; defer to the configured default"},
		{"", "", "no source: the default mirror applies"},
		{"some-user-added-source", "", "unknown source: the default mirror applies"},
	} {
		if got := PullHostForSearchSource(tc.source); got != tc.want {
			t.Errorf("PullHostForSearchSource(%q) = %q, want %q — %s", tc.source, got, tc.want, tc.why)
		}
	}
}

// TestPullHostIsAPullableReference guards the part that actually matters: the
// returned host must survive Parse as a registry, so the download really goes
// there. A "host" that Parse treats as a repository name would be worse than no
// hint at all.
func TestPullHostIsAPullableReference(t *testing.T) {
	for _, source := range []string{"1ms", "quay"} {
		host := PullHostForSearchSource(source)
		if host == "" {
			t.Fatalf("no pull host for %q", source)
		}
		ref, err := Parse("library/nginx:latest", host)
		if err != nil {
			t.Fatalf("Parse with %q: %v", host, err)
		}
		if ref.Registry != host {
			t.Errorf("Parse with %q produced registry %q", host, ref.Registry)
		}
		if ref.Repository != "library/nginx" {
			t.Errorf("repository = %q, want library/nginx", ref.Repository)
		}
	}
}

// TestAcceleratorHostNeedsTheLibraryRule: the accelerator proxies Docker Hub's
// namespace, so a single-segment name must still gain "library/". If this were
// false, "1ms" + "nginx" would request the nonexistent repository "nginx".
func TestAcceleratorHostNeedsTheLibraryRule(t *testing.T) {
	host := PullHostForSearchSource("1ms")
	if host == "" {
		t.Fatal("no pull host for 1ms")
	}
	if !NeedsLibraryPrefix(host) {
		t.Errorf("NeedsLibraryPrefix(%q) = false; single-segment Docker Hub names would not resolve through the accelerator", host)
	}
	ref, err := Parse("nginx:latest", host)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ref.Repository != "library/nginx" {
		t.Errorf("repository = %q, want library/nginx", ref.Repository)
	}
	// And a quay result keeps its own namespace: prefixing would break it.
	quay := PullHostForSearchSource("quay")
	if NeedsLibraryPrefix(quay) {
		t.Errorf("NeedsLibraryPrefix(%q) = true, but quay.io has no library/ namespace", quay)
	}
}

// TestPullHostMatchesOnlyKnownSources keeps the mapping honest: a randomly named
// source must not be guessed onto someone's mirror.
func TestPullHostMatchesOnlyKnownSources(t *testing.T) {
	for _, source := range []string{"dockerhub", "", "harbor", "src_abc123", "docker.io"} {
		if got := PullHostForSearchSource(source); got != "" {
			t.Errorf("PullHostForSearchSource(%q) = %q, want empty (the caller's default mirror)", source, got)
		}
	}
}

// TestPullHostAcceptsEverySpellingAUserTypes covers the whole point of the
// resolver: `--mirror` and the API's `registry` field are ONE knob, and the user
// reaches for whichever name they saw in the UI.
//
// Resolving only mirror ids meant `--mirror mcr` / `--mirror quay` fell through
// to DNS as a hostname and failed with "lookup mcr: no such host" — a confusing
// way to say "that is a known source, spelled as an id".
func TestPullHostAcceptsEverySpellingAUserTypes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		// Docker Hub accelerators, by id, name and host.
		{"1ms", "docker.1ms.run", true},
		{"1ms 加速源", "docker.1ms.run", true},
		{"docker.1ms.run", "docker.1ms.run", true},
		{"nju", "docker.nju.edu.cn", true},
		{"南大镜像源", "docker.nju.edu.cn", true},
		{"  1ms  ", "docker.1ms.run", true},
		{"1MS", "docker.1ms.run", true},
		// UPSTREAM registries, by id, name and host — the case that used to fail.
		{"mcr", "mcr.microsoft.com", true},
		{"quay", "quay.io", true},
		{"ghcr", "ghcr.io", true},
		{"k8s", "registry.k8s.io", true},
		{"Microsoft Container Registry", "mcr.microsoft.com", true},
		{"Quay.io (Red Hat)", "quay.io", true},
		{"mcr.microsoft.com", "mcr.microsoft.com", true},
		{"quay.io", "quay.io", true},
		{"registry.k8s.io", "registry.k8s.io", true},
		// Docker Hub itself.
		{"dockerhub", "registry-1.docker.io", true},
		{"registry-1.docker.io", "registry-1.docker.io", true},
		// A private registry, and a pasted URL.
		{"harbor.internal:5000", "harbor.internal:5000", true},
		{"localhost:5000", "localhost:5000", true},
		{"https://quay.io/v2/", "quay.io", true},
		{"http://127.0.0.1:5000/", "127.0.0.1:5000", true},
		// Refusals: a bare word is neither a known source nor host-shaped, so it
		// must not be dialled as a hostname.
		{"", "", false},
		{"   ", "", false},
		{"mirror-that-was-deleted", "", false},
		{"notahost", "", false},
	} {
		got, ok := PullHost(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("PullHost(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestPullHostAgreesWithSearchSourceRouting: the two resolvers must not disagree,
// or a search result would be pulled from one host while the picker claims
// another.
func TestPullHostAgreesWithSearchSourceRouting(t *testing.T) {
	for _, s := range BuiltinSearchSources {
		viaSource := PullHostForSearchSource(s.ID)
		viaPull, ok := PullHost(s.ID)
		if viaSource == "" {
			continue // Docker Hub: deliberately defers to the configured default.
		}
		if !ok || viaPull != viaSource {
			t.Errorf("source %q: PullHostForSearchSource=%q but PullHost=(%q,%v)", s.ID, viaSource, viaPull, ok)
		}
	}
}

// TestSearcherServesRegistry pins the namespace rule for the tag fallback.
//
// A searcher may only describe the namespace it belongs to. Ghcr's repo
// linuxcontainers/alpine has 38 tags; Docker Hub's repo of the same name has 42
// — a fallback therefore offers tags the download cannot have, which is exactly
// how a ghcr.io pull came to be shown Docker Hub's tags.
func TestSearcherServesRegistry(t *testing.T) {
	for _, tc := range []struct {
		host     string
		searcher string
		want     bool
		why      string
	}{
		{"registry-1.docker.io", "dockerhub", true, "Hub answering for Hub"},
		{"docker.io", "dockerhub", true, "an alias of the same namespace"},
		{"docker.1ms.run", "dockerhub", true, "an accelerator serves Hub's namespace"},
		{"docker.1ms.run", "1ms", true, "the accelerator's own searcher"},
		{"registry-1.docker.io", "1ms", true, "a Hub accelerator's searcher on Hub"}, // 1ms pins docker.1ms.run -> NOT the same host
		{"quay.io", "quay", true, "quay answering for quay"},
		{"mcr.microsoft.com", "mcr", true, "MCR answering for MCR"},
		{"ghcr.io", "dockerhub", false, "the case that misled: Hub does NOT describe ghcr"},
		{"ghcr.io", "quay", false, "nor does quay"},
		{"mcr.microsoft.com", "dockerhub", false, "Hub does not describe MCR"},
		{"quay.io", "dockerhub", false, "Hub does not describe quay"},
		{"", "dockerhub", false, "no host, no answer"},
		{"ghcr.io", "", false, "no searcher, no answer"},
	} {
		if got := SearcherServesRegistry(tc.host, tc.searcher); got != tc.want {
			t.Errorf("SearcherServesRegistry(%q, %q) = %v, want %v — %s", tc.host, tc.searcher, got, tc.want, tc.why)
		}
	}
}
