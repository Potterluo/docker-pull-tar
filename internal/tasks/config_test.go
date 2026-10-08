package tasks

// config_test.go covers the pure configuration resolver.
//
// internal/tasks is the bridge between the CLI and the GUI: both front ends
// write the same setting keys through SaveConfig/EnsureConfig — `dockerpull
// pull --insecure` on one side, the Settings page on the other — and both
// read the resolved Config back. A bug in ResolveConfig therefore changes
// what a download actually does (workers, TLS verification, proxy, output
// directory) without any code that looks wrong at the call site.
//
// ResolveConfig is a pure function, so it can be tested exhaustively here
// without a store, a network, or a manager.

import (
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"testing"

	"github.com/Potterluo/docker-pull-tar/internal/registry"
)

// distinctSettingValues supplies one value per setting key that the
// resolver must hand back unchanged. Every key in SettingKeys() must appear
// here (TestResolveConfigHonoursEverySettingKey fails loudly otherwise), so
// a newly added key cannot silently slip through untested.
func distinctSettingValues(dataDir string) map[string]string {
	return map[string]string{
		KeyOutputDir:           filepath.Join(dataDir, "custom-downloads"),
		KeyWorkers:             "7",
		KeyVerifyTLS:           "false",
		KeyProxyMode:           ProxyCustom,
		KeyProxyURL:            "http://proxy.invalid:3128",
		KeyDefaultMirror:       "nju",
		KeyDefaultSearchSource: "1ms",
		KeyChunkThresholdMB:    "12",
		KeyMaxRetries:          "3",
	}
}

// settingValueCompanions supplies the sibling keys a value only resolves
// against. proxy_url is the one dependency: the resolver deliberately CLEARS
// a stored URL unless proxy_mode is custom (a leftover URL must not leak into
// system/none mode), so round-tripping proxy_url alone is meaningless.
// TestResolveConfigProxyMode covers the cleared direction.
var settingValueCompanions = map[string]map[string]string{
	KeyProxyURL: {KeyProxyMode: ProxyCustom},
}

// settingValue reads the resolved Config field a setting key feeds, in the
// same string form the setting is stored as. A key that has no case here
// fails the round-trip test rather than passing vacuously.
func settingValue(cfg Config, key string) string {
	switch key {
	case KeyOutputDir:
		return cfg.OutputDir
	case KeyWorkers:
		return strconv.Itoa(cfg.Workers)
	case KeyVerifyTLS:
		return strconv.FormatBool(cfg.VerifyTLS)
	case KeyProxyMode:
		return cfg.ProxyMode
	case KeyProxyURL:
		return cfg.ProxyURL
	case KeyDefaultMirror:
		return cfg.DefaultMirror
	case KeyDefaultSearchSource:
		return cfg.DefaultSearchSource
	case KeyChunkThresholdMB:
		return strconv.Itoa(cfg.ChunkThresholdMB)
	case KeyMaxRetries:
		return strconv.Itoa(cfg.MaxRetries)
	default:
		return ""
	}
}

// TestResolveConfigDefaults pins the shipped behaviour for an empty store.
// A zero Config would mean 0 workers and verify_tls=false — i.e. TLS
// verification silently switched OFF on a fresh install, which is the
// failure mode AGENTS.md calls out by name.
func TestResolveConfigDefaults(t *testing.T) {
	dataDir := t.TempDir()
	cfg := ResolveConfig(map[string]string{}, dataDir)

	if want := filepath.Join(dataDir, "downloads"); cfg.OutputDir != want {
		t.Errorf("OutputDir = %q, want %q", cfg.OutputDir, want)
	}
	if cfg.Workers != 4 {
		t.Errorf("Workers = %d, want 4", cfg.Workers)
	}
	if !cfg.VerifyTLS {
		t.Error("VerifyTLS = false, want true: certificate verification is opted OUT of, never defaulted off")
	}
	if cfg.ProxyMode != ProxySystem {
		t.Errorf("ProxyMode = %q, want %q", cfg.ProxyMode, ProxySystem)
	}
	if cfg.ProxyURL != "" {
		t.Errorf("ProxyURL = %q, want empty (system mode follows the environment)", cfg.ProxyURL)
	}
	if cfg.DefaultMirror != registry.DefaultMirror().ID {
		t.Errorf("DefaultMirror = %q, want the shipped default %q", cfg.DefaultMirror, registry.DefaultMirror().ID)
	}
	if cfg.DefaultSearchSource != registry.DefaultSearchSourceID {
		t.Errorf("DefaultSearchSource = %q, want %q", cfg.DefaultSearchSource, registry.DefaultSearchSourceID)
	}
	if cfg.ChunkThresholdMB != 50 {
		t.Errorf("ChunkThresholdMB = %d, want 50", cfg.ChunkThresholdMB)
	}
	if cfg.MaxRetries != 10 {
		t.Errorf("MaxRetries = %d, want 10", cfg.MaxRetries)
	}

	// A nil map is what a caller with zero setting rows actually has in
	// hand (listSettings → make(map…) only when len(rows) > 0 is not
	// guaranteed by Go), so it must behave exactly like an empty one.
	if nilCfg := ResolveConfig(nil, dataDir); !reflect.DeepEqual(nilCfg, cfg) {
		t.Errorf("ResolveConfig(nil) = %+v, want the same as an empty map: %+v", nilCfg, cfg)
	}
}

// TestConfigDefaults protects the boot contract: EnsureConfig writes
// ConfigDefaults, and the Settings page renders whatever is stored. A key
// with no default leaves a blank field in the GUI and an unclamped value in
// the resolver; a default for a key the resolver does not know is a value
// nobody will ever read.
func TestConfigDefaults(t *testing.T) {
	dataDir := t.TempDir()
	def := ConfigDefaults(dataDir)

	if got, want := def[KeyOutputDir], filepath.Join(dataDir, "downloads"); got != want {
		t.Errorf("default output_dir = %q, want %q (the artifact dir lives under the data dir)", got, want)
	}
	for _, key := range SettingKeys() {
		if _, ok := def[key]; !ok {
			t.Errorf("ConfigDefaults has no value for the setting %q: EnsureConfig would leave it unset", key)
		}
	}
	for key := range def {
		if !IsSettingKey(key) {
			t.Errorf("ConfigDefaults defines %q, which is not in SettingKeys(): nothing reads it", key)
		}
	}
}

// TestConfigDefaultsEmptyDataDir documents what a blank data dir produces.
//
// ConfigDefaults substitutes "." for a blank dataDir, and filepath.Join then
// CLEANS the "./" prefix away, so the result is the RELATIVE path
// "downloads" rather than an absolute one. That is the code's actual
// behaviour and it is only reachable by calling ConfigDefaults directly —
// internal/app.Boot always passes a real data dir — so this is a documented
// fallback, not a bug. Asserting it keeps a future "fix" honest.
func TestConfigDefaultsEmptyDataDir(t *testing.T) {
	for _, dataDir := range []string{"", "   "} {
		got := ConfigDefaults(dataDir)[KeyOutputDir]
		if got != "downloads" {
			t.Errorf("ConfigDefaults(%q)[output_dir] = %q, want the relative %q", dataDir, got, "downloads")
		}
		if filepath.IsAbs(got) {
			t.Errorf("ConfigDefaults(%q)[output_dir] = %q, unexpectedly absolute", dataDir, got)
		}
	}
}

// TestSettingKeysShape locks the list the GUI's bulk update validates
// against: sorted (the settings page renders it in order), unique (a
// duplicate would make IsSettingKey untestable) and non-empty names.
func TestSettingKeysShape(t *testing.T) {
	keys := SettingKeys()
	if len(keys) == 0 {
		t.Fatal("SettingKeys() is empty: SaveConfig would reject every patch")
	}
	if !sort.StringsAreSorted(keys) {
		t.Errorf("SettingKeys() = %v, want sorted order", keys)
	}
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if key == "" {
			t.Error("SettingKeys() contains an empty key")
		}
		if seen[key] {
			t.Errorf("SettingKeys() lists %q twice", key)
		}
		seen[key] = true
	}
}

// TestResolveConfigHonoursEverySettingKey is the exhaustive round trip: set
// one key, read the field back. Every entry of SettingKeys() is exercised,
// and the per-key value map means a newly added key with no test value —
// and therefore no default and no coverage — fails this test instead of
// passing by accident.
func TestResolveConfigHonoursEverySettingKey(t *testing.T) {
	dataDir := t.TempDir()
	values := distinctSettingValues(dataDir)

	for _, key := range SettingKeys() {
		want, ok := values[key]
		if !ok {
			t.Fatalf("SettingKeys() has %q but the test has no distinct value for it: add one, and a Config field to read it", key)
		}
		t.Run(key, func(t *testing.T) {
			patch := map[string]string{key: want}
			for k, v := range settingValueCompanions[key] {
				patch[k] = v
			}
			cfg := ResolveConfig(patch, dataDir)
			if got := settingValue(cfg, key); got != want {
				t.Errorf("ResolveConfig(%v).%s = %q, want %q", patch, key, got, want)
			}
		})
	}
}

// TestResolveConfigAllSettingsAtOnce resolves every key together. The
// per-key test proves each field is fed by its own key; this one proves the
// keys do not interfere — a copy-paste in ResolveConfig's struct literal
// (two fields reading the same key) passes the per-key test and fails here.
func TestResolveConfigAllSettingsAtOnce(t *testing.T) {
	dataDir := t.TempDir()
	values := distinctSettingValues(dataDir)

	cfg := ResolveConfig(values, dataDir)
	for _, key := range SettingKeys() {
		if got := settingValue(cfg, key); got != values[key] {
			t.Errorf("with every key set, %s = %q, want %q", key, got, values[key])
		}
	}
}

// TestResolveConfigClampsNumbers protects the socket budget: a user typing
// 0 or 100000 workers in the Settings page must not be able to open 100000
// connections, and a garbage value must fall back to the default rather than
// to the minimum (0 workers would stall the download pool).
func TestResolveConfigClampsNumbers(t *testing.T) {
	dataDir := t.TempDir()

	cases := []struct {
		name  string
		key   string
		value string
		want  int
	}{
		{"workers zero clamps to the minimum", KeyWorkers, "0", minWorkers},
		{"workers negative clamps to the minimum", KeyWorkers, "-5", minWorkers},
		{"workers above the ceiling clamps", KeyWorkers, "9999", maxWorkers},
		{"workers garbage falls back to the default", KeyWorkers, "abc", 4},
		{"workers fractional falls back to the default", KeyWorkers, "4.5", 4},
		{"workers with spaces is parsed", KeyWorkers, "  6  ", 6},
		{"max_retries negative clamps to zero", KeyMaxRetries, "-1", minRetries},
		{"max_retries above the ceiling clamps", KeyMaxRetries, "999", maxRetries},
		{"max_retries garbage falls back to the default", KeyMaxRetries, "abc", 10},
		{"max_retries zero is a legal budget", KeyMaxRetries, "0", 0},
		{"chunk_threshold_mb zero clamps to the minimum", KeyChunkThresholdMB, "0", minChunkMB},
		{"chunk_threshold_mb above the ceiling clamps", KeyChunkThresholdMB, "99999", maxChunkMB},
		{"chunk_threshold_mb garbage falls back to the default", KeyChunkThresholdMB, "abc", 50},
		{"chunk_threshold_mb negative clamps to the minimum", KeyChunkThresholdMB, "-3", minChunkMB},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ResolveConfig(map[string]string{tc.key: tc.value}, dataDir)
			if got := settingValue(cfg, tc.key); got != strconv.Itoa(tc.want) {
				t.Errorf("ResolveConfig({%s: %q}) → %s = %s, want %d", tc.key, tc.value, tc.key, got, tc.want)
			}
		})
	}

	// The bounds themselves must stay sane, or the clamps above are
	// meaningless: a zero or inverted range would clamp everything.
	if minWorkers < 1 || maxWorkers < minWorkers {
		t.Errorf("worker bounds [%d,%d] are not a usable range", minWorkers, maxWorkers)
	}
	if minRetries > maxRetries {
		t.Errorf("retry bounds [%d,%d] are inverted", minRetries, maxRetries)
	}
	if minChunkMB < 1 || maxChunkMB < minChunkMB {
		t.Errorf("chunk bounds [%d,%d] are not a usable range", minChunkMB, maxChunkMB)
	}
}

// TestResolveConfigBoolParsing covers the spelling zoo a settings file or a
// hand-edited database value can contain, including the Chinese forms the
// GUI's own labels use. An unrecognised value must fall back to the default
// (true) — never to false, because false means "stop checking certificates".
func TestResolveConfigBoolParsing(t *testing.T) {
	dataDir := t.TempDir()

	cases := []struct {
		value string
		want  bool
		why   string
	}{
		{"true", true, "canonical"},
		{"1", true, "numeric"},
		{"yes", true, "affirmative"},
		{"on", true, "toggle spelling"},
		{"是", true, "Chinese affirmative"},
		{"TRUE", true, "case-insensitive"},
		{"  Yes  ", true, "trimmed and case-insensitive"},
		{"false", false, "canonical"},
		{"0", false, "numeric"},
		{"no", false, "negative"},
		{"off", false, "toggle spelling"},
		{"否", false, "Chinese negative"},
		{"FALSE", false, "case-insensitive"},
		{"maybe", true, "unrecognised falls back to the default (true)"},
		{"2", true, "unrecognised falls back to the default (true)"},
		{"", true, "blank falls back to the default (true)"},
	}

	for _, tc := range cases {
		t.Run(tc.value+"/"+tc.why, func(t *testing.T) {
			cfg := ResolveConfig(map[string]string{KeyVerifyTLS: tc.value}, dataDir)
			if cfg.VerifyTLS != tc.want {
				t.Errorf("ResolveConfig({verify_tls: %q}).VerifyTLS = %v, want %v (%s)", tc.value, cfg.VerifyTLS, tc.want, tc.why)
			}
		})
	}
}

// TestResolveConfigProxyMode protects two rules at once: the mode is
// normalised to one of the three known values (an unknown string becomes
// "system", never a mode that reaches the registry client verbatim), and a
// proxy_url stored by an earlier custom configuration is CLEARED for every
// other mode — a leftover URL leaking into "none" would send traffic
// through a proxy the user just switched off.
func TestResolveConfigProxyMode(t *testing.T) {
	dataDir := t.TempDir()
	const storedURL = "http://leftover.invalid:8080"

	cases := []struct {
		name     string
		stored   string
		wantMode string
		wantURL  string
	}{
		{"none clears the leftover url", ProxyNone, ProxyNone, ""},
		{"system clears the leftover url", ProxySystem, ProxySystem, ""},
		{"custom keeps the url", ProxyCustom, ProxyCustom, storedURL},
		{"NONE is normalised", "NONE", ProxyNone, ""},
		{"padded and mixed case is normalised", "  Custom ", ProxyCustom, storedURL},
		{"an unknown mode falls back to system", "tor", ProxySystem, ""},
		{"an empty mode falls back to system", "", ProxySystem, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ResolveConfig(map[string]string{
				KeyProxyMode: tc.stored,
				KeyProxyURL:  storedURL,
			}, dataDir)
			if cfg.ProxyMode != tc.wantMode {
				t.Errorf("ProxyMode = %q, want %q", cfg.ProxyMode, tc.wantMode)
			}
			if cfg.ProxyURL != tc.wantURL {
				t.Errorf("ProxyURL = %q, want %q", cfg.ProxyURL, tc.wantURL)
			}
		})
	}
}

// TestResolveConfigFallsBackFromInvalidReferences protects against an
// edited-away (or hand-typed) mirror/source id: the resolver must fall back
// to the shipped default instead of handing the registry client an id it
// cannot resolve, which would fail every subsequent pull.
func TestResolveConfigFallsBackFromInvalidReferences(t *testing.T) {
	dataDir := t.TempDir()
	shippedMirror := registry.DefaultMirror().ID

	t.Run("unknown mirror id falls back to the shipped default", func(t *testing.T) {
		cfg := ResolveConfig(map[string]string{KeyDefaultMirror: "mirror-that-was-deleted"}, dataDir)
		if cfg.DefaultMirror != shippedMirror {
			t.Errorf("DefaultMirror = %q, want the shipped default %q", cfg.DefaultMirror, shippedMirror)
		}
	})

	t.Run("a known non-default mirror is kept", func(t *testing.T) {
		if _, ok := registry.MirrorByID("nju"); !ok {
			t.Skip("the shipped mirror list no longer contains nju")
		}
		cfg := ResolveConfig(map[string]string{KeyDefaultMirror: "nju"}, dataDir)
		if cfg.DefaultMirror != "nju" {
			t.Errorf("DefaultMirror = %q, want %q", cfg.DefaultMirror, "nju")
		}
	})

	t.Run("unknown search source falls back to dockerhub", func(t *testing.T) {
		cfg := ResolveConfig(map[string]string{KeyDefaultSearchSource: "some-typo"}, dataDir)
		if cfg.DefaultSearchSource != registry.DefaultSearchSourceID {
			t.Errorf("DefaultSearchSource = %q, want %q", cfg.DefaultSearchSource, registry.DefaultSearchSourceID)
		}
	})

	t.Run("every shipped search source is accepted", func(t *testing.T) {
		// The built-in list is what SeedSources writes into the store; if
		// the resolver's idea of "built-in" ever drifts from it, a source
		// the GUI shows as selectable would be silently reset to dockerhub.
		for _, s := range registry.BuiltinSearchSources {
			cfg := ResolveConfig(map[string]string{KeyDefaultSearchSource: s.ID}, dataDir)
			if cfg.DefaultSearchSource != s.ID {
				t.Errorf("DefaultSearchSource for the built-in %q = %q, want it kept", s.ID, cfg.DefaultSearchSource)
			}
		}
	})
}

// TestResolveConfigIgnoresUnknownKeys: the resolver only reads the keys it
// knows, so a row left behind by an older version, or written by hand, must
// not change anything or produce an error. SaveConfig is what rejects a
// typo; by the time ResolveConfig runs, the data is whatever is in the
// table.
func TestResolveConfigIgnoresUnknownKeys(t *testing.T) {
	dataDir := t.TempDir()
	want := ResolveConfig(map[string]string{}, dataDir)

	got := ResolveConfig(map[string]string{
		"workres":            "99",
		"output-dir":         "/somewhere/else",
		"":                   "nonsense",
		"max_retries_backup": "-1",
	}, dataDir)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unknown keys changed the resolved config:\n got %+v\nwant %+v", got, want)
	}

	// Known keys in the same map are still honoured.
	mixed := ResolveConfig(map[string]string{"not_a_setting": "x", KeyWorkers: "6"}, dataDir)
	if mixed.Workers != 6 {
		t.Errorf("Workers = %d, want 6: an unknown sibling key must not stop the known ones", mixed.Workers)
	}
}

// TestResolveConfigTreatsBlankAsAbsent protects the "clearing a field"
// gesture: the GUI writes an empty string, and that must mean "use the
// default", not "clamp to the minimum" (workers "   " → 4, not 1) and not
// "certificate verification off".
func TestResolveConfigTreatsBlankAsAbsent(t *testing.T) {
	dataDir := t.TempDir()
	defaults := ResolveConfig(map[string]string{}, dataDir)

	cases := []struct {
		name   string
		key    string
		value  string
		expect string
	}{
		{"blank workers uses the default", KeyWorkers, "   ", settingValue(defaults, KeyWorkers)},
		{"blank max_retries uses the default", KeyMaxRetries, "\t\n ", settingValue(defaults, KeyMaxRetries)},
		{"blank chunk threshold uses the default", KeyChunkThresholdMB, " ", settingValue(defaults, KeyChunkThresholdMB)},
		{"blank verify_tls uses the default", KeyVerifyTLS, "  ", settingValue(defaults, KeyVerifyTLS)},
		{"blank proxy mode uses the default", KeyProxyMode, "  ", settingValue(defaults, KeyProxyMode)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ResolveConfig(map[string]string{tc.key: tc.value}, dataDir)
			if got := settingValue(cfg, tc.key); got != tc.expect {
				t.Errorf("ResolveConfig({%s: %q}) → %s = %q, want the default %q", tc.key, tc.value, tc.key, got, tc.expect)
			}
		})
	}
}

// TestProxyURLForPuller protects the translation into the registry client's
// convention: "-" is an explicit direct connection, "" means "honour the
// environment", anything else is the proxy URL itself. The custom-with-no-URL
// case matters — a half-filled form must go direct rather than hand the
// client an empty-but-custom proxy.
func TestProxyURLForPuller(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"none forces a direct connection", Config{ProxyMode: ProxyNone, ProxyURL: "http://p:1"}, "-"},
		{"none ignores a stored url", Config{ProxyMode: ProxyNone}, "-"},
		{"custom passes the url through", Config{ProxyMode: ProxyCustom, ProxyURL: "http://p:8080"}, "http://p:8080"},
		{"custom trims the url", Config{ProxyMode: ProxyCustom, ProxyURL: "  http://p:8080  "}, "http://p:8080"},
		{"custom without a url goes direct", Config{ProxyMode: ProxyCustom, ProxyURL: ""}, "-"},
		{"custom with a blank url goes direct", Config{ProxyMode: ProxyCustom, ProxyURL: "   "}, "-"},
		{"system honours the environment", Config{ProxyMode: ProxySystem, ProxyURL: "http://p:8080"}, ""},
		{"an empty mode honours the environment", Config{ProxyMode: "", ProxyURL: "http://p:8080"}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.ProxyURLForPuller(); got != tc.want {
				t.Errorf("ProxyURLForPuller() = %q, want %q", got, tc.want)
			}
		})
	}

	// End to end through the resolver: the leftover-URL rule must survive
	// the trip, or a pull in "none" mode would still use the old proxy.
	dataDir := t.TempDir()
	resolved := ResolveConfig(map[string]string{
		KeyProxyMode: ProxyNone,
		KeyProxyURL:  "http://leftover.invalid:8080",
	}, dataDir)
	if got := resolved.ProxyURLForPuller(); got != "-" {
		t.Errorf("resolved none-mode ProxyURLForPuller() = %q, want %q", got, "-")
	}
}

// TestIsSettingKey protects the typo guard SaveConfig hangs off: a key that
// is not exactly one of the exported constants must be rejected, because
// accepting it would store a value nothing ever reads.
func TestIsSettingKey(t *testing.T) {
	for _, key := range SettingKeys() {
		if !IsSettingKey(key) {
			t.Errorf("IsSettingKey(%q) = false, want true", key)
		}
	}
	// The exported constants are the vocabulary the CLI and handlers use;
	// every one of them must be a recognised key.
	for _, key := range []string{
		KeyOutputDir, KeyWorkers, KeyVerifyTLS, KeyProxyMode, KeyProxyURL,
		KeyDefaultMirror, KeyDefaultSearchSource, KeyChunkThresholdMB, KeyMaxRetries,
	} {
		if !IsSettingKey(key) {
			t.Errorf("IsSettingKey(%q) = false, want true for an exported Key* constant", key)
		}
	}

	for _, key := range []string{
		"", " ", "workers ", " workers", "Workers", "WORKERS", "output-dir",
		"outputDir", "worker", "default_mirror_id", "not_a_setting", "verify_tls\n",
	} {
		if IsSettingKey(key) {
			t.Errorf("IsSettingKey(%q) = true, want false: a near-miss must not be accepted", key)
		}
	}
}
