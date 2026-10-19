package provider

import "testing"

func TestRolloutLabel(t *testing.T) {
	for _, tc := range []struct {
		r    Rollout
		want string
	}{
		{Rollout{Sequence: "7", Revision: "6d4b9c8f7"}, "7"},
		{Rollout{Revision: "sha256:0123456789abcdef0123"}, "0123456789ab"},
		{Rollout{Revision: "0123456789abcdef"}, "0123456789ab"},
		{Rollout{Revision: "abc"}, "abc"},
		{Rollout{}, ""},
	} {
		if got := tc.r.Label(); got != tc.want {
			t.Errorf("%+v: label %q, want %q", tc.r, got, tc.want)
		}
	}
}
