package main

import (
	"runtime/debug"
	"testing"
)

// Production announced itself as "tasks dev" because the Docker build stamps
// no version. When the linker did not inject one, the binary falls back to
// the VCS revision Go embeds at build time.
func TestResolveVersion(t *testing.T) {
	t.Parallel()

	buildInfo := func(settings ...debug.BuildSetting) *debug.BuildInfo {
		return &debug.BuildInfo{Settings: settings}
	}

	cases := []struct {
		name   string
		linked string
		info   *debug.BuildInfo
		ok     bool
		want   string
	}{
		{
			name:   "linker version wins",
			linked: "v1.2.3",
			info:   buildInfo(debug.BuildSetting{Key: "vcs.revision", Value: "abcdef1234567890"}),
			ok:     true,
			want:   "v1.2.3",
		},
		{
			name:   "a full linked commit SHA is shortened",
			linked: "0123456789abcdef0123456789abcdef01234567",
			info:   nil,
			ok:     false,
			want:   "0123456789ab",
		},
		{
			name:   "a release tag that is not a bare SHA stays whole",
			linked: "v1.2.3-rc.1",
			info:   nil,
			ok:     false,
			want:   "v1.2.3-rc.1",
		},
		{
			name:   "dev falls back to the short revision",
			linked: "dev",
			info: buildInfo(
				debug.BuildSetting{Key: "vcs.revision", Value: "abcdef1234567890"},
				debug.BuildSetting{Key: "vcs.modified", Value: "false"},
			),
			ok:   true,
			want: "abcdef123456",
		},
		{
			name:   "dirty builds say so",
			linked: "dev",
			info: buildInfo(
				debug.BuildSetting{Key: "vcs.revision", Value: "abcdef1234567890"},
				debug.BuildSetting{Key: "vcs.modified", Value: "true"},
			),
			ok:   true,
			want: "abcdef123456-dirty",
		},
		{
			name:   "no build info stays dev",
			linked: "dev",
			info:   nil,
			ok:     false,
			want:   "dev",
		},
		{
			name:   "build info without vcs stays dev",
			linked: "dev",
			info:   buildInfo(),
			ok:     true,
			want:   "dev",
		},
	}
	for _, testCase := range cases {
		if got := resolveVersion(testCase.linked, testCase.info, testCase.ok); got != testCase.want {
			t.Errorf("%s: resolveVersion = %q, want %q", testCase.name, got, testCase.want)
		}
	}
}
