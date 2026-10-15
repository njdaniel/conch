package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestResolve(t *testing.T) {
	settings := func(rev, modified string) []debug.BuildSetting {
		out := []debug.BuildSetting{{Key: "vcs.revision", Value: rev}}
		if modified != "" {
			out = append(out, debug.BuildSetting{Key: "vcs.modified", Value: modified})
		}
		return out
	}
	rev := "0123456789abcdef0123456789abcdef01234567"
	tests := []struct {
		name    string
		stamped string
		info    *debug.BuildInfo
		ok      bool
		want    string
	}{
		{"stamped wins over build info", "v0.1.0-alpha.1", &debug.BuildInfo{Settings: settings(rev, "")}, true, "v0.1.0-alpha.1"},
		{"stamped is trimmed", " v1.2.3\n", nil, false, "v1.2.3"},
		{"unstamped falls back to the commit", "", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: settings(rev, "false")}, true, "dev-0123456789ab"},
		{"unstamped dirty tree", "", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: settings(rev, "true")}, true, "dev-0123456789ab-dirty"},
		{"short revision is kept whole", "", &debug.BuildInfo{Settings: settings("abc123", "")}, true, "dev-abc123"},
		{"go install records the module version", "", &debug.BuildInfo{Main: debug.Module{Version: "v1.4.0"}}, true, "v1.4.0"},
		{"no vcs data", "", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, true, Unknown},
		{"no build info", "", nil, false, Unknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolve(tt.stamped, func() (*debug.BuildInfo, bool) { return tt.info, tt.ok })
			if got != tt.want {
				t.Errorf("resolve(%q) = %q, want %q", tt.stamped, got, tt.want)
			}
		})
	}
}

func TestVersionStampedPassesThrough(t *testing.T) {
	if got := Version("v9.9.9"); got != "v9.9.9" {
		t.Errorf("Version(stamped) = %q, want v9.9.9", got)
	}
}
