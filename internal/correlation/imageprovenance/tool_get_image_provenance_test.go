package imageprovenance

import "testing"

func TestParseImage(t *testing.T) {
	tests := []struct {
		name         string
		container    string
		image        string
		wantRegistry string
		wantRepo     string
		wantTag      string
		wantGitSHA   bool
	}{
		{
			name:         "registry + repo + version tag",
			container:    "api",
			image:        "ghcr.io/zuful/rikugan:v1.2.3",
			wantRegistry: "ghcr.io",
			wantRepo:     "zuful/rikugan",
			wantTag:      "v1.2.3",
			wantGitSHA:   false,
		},
		{
			name:         "registry + repo + 7-char git sha tag",
			container:    "api",
			image:        "ghcr.io/zuful/rikugan:a1b2c3d",
			wantRegistry: "ghcr.io",
			wantRepo:     "zuful/rikugan",
			wantTag:      "a1b2c3d",
			wantGitSHA:   true,
		},
		{
			name:         "registry + repo + 40-char git sha tag",
			container:    "api",
			image:        "ghcr.io/zuful/rikugan:da39a3ee5e6b4b0d3255bfef95601890afd80709",
			wantRegistry: "ghcr.io",
			wantRepo:     "zuful/rikugan",
			wantTag:      "da39a3ee5e6b4b0d3255bfef95601890afd80709",
			wantGitSHA:   true,
		},
		{
			name:         "uppercase hex tag is NOT detected as git sha (canonical SHAs are lowercase)",
			container:    "api",
			image:        "ghcr.io/zuful/rikugan:A1B2C3D",
			wantRegistry: "ghcr.io",
			wantRepo:     "zuful/rikugan",
			wantTag:      "A1B2C3D",
			wantGitSHA:   false,
		},
		{
			name:         "6-char hex tag too short to be a git sha (min is 7)",
			container:    "api",
			image:        "ghcr.io/zuful/rikugan:a1b2c3",
			wantRegistry: "ghcr.io",
			wantRepo:     "zuful/rikugan",
			wantTag:      "a1b2c3",
			wantGitSHA:   false,
		},
		{
			name:         "registry with port",
			container:    "api",
			image:        "registry.local:5000/team/svc:v1",
			wantRegistry: "registry.local:5000",
			wantRepo:     "team/svc",
			wantTag:      "v1",
			wantGitSHA:   false,
		},
		{
			name:         "no registry implied (single segment before /)",
			container:    "api",
			image:        "library/nginx:1.27",
			wantRegistry: "",
			wantRepo:     "library/nginx",
			wantTag:      "1.27",
			wantGitSHA:   false,
		},
		{
			name:         "no tag defaults to latest",
			container:    "api",
			image:        "ghcr.io/zuful/rikugan",
			wantRegistry: "ghcr.io",
			wantRepo:     "zuful/rikugan",
			wantTag:      "latest",
			wantGitSHA:   false,
		},
		{
			name:         "digest reference keeps the @sha256:… token as the Tag, not detected as git SHA",
			container:    "api",
			image:        "ghcr.io/zuful/rikugan@sha256:da39a3ee5e6b4b0d3255bfef95601890afd80709da39a3ee5e6b4b0d3255bfef",
			wantRegistry: "ghcr.io",
			wantRepo:     "zuful/rikugan",
			wantTag:      "@sha256:da39a3ee5e6b4b0d3255bfef95601890afd80709da39a3ee5e6b4b0d3255bfef",
			wantGitSHA:   false,
		},
		{
			name:         "bare image name with no slash",
			container:    "api",
			image:        "alpine:3.20",
			wantRegistry: "",
			wantRepo:     "alpine",
			wantTag:      "3.20",
			wantGitSHA:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseImage(tt.container, tt.image)
			if got.Container != tt.container {
				t.Errorf("Container = %q, want %q", got.Container, tt.container)
			}
			if got.Image != tt.image {
				t.Errorf("Image = %q, want %q", got.Image, tt.image)
			}
			if got.Registry != tt.wantRegistry {
				t.Errorf("Registry = %q, want %q", got.Registry, tt.wantRegistry)
			}
			if got.Repo != tt.wantRepo {
				t.Errorf("Repo = %q, want %q", got.Repo, tt.wantRepo)
			}
			if got.Tag != tt.wantTag {
				t.Errorf("Tag = %q, want %q", got.Tag, tt.wantTag)
			}
			if got.IsGitSHA != tt.wantGitSHA {
				t.Errorf("IsGitSHA = %v, want %v", got.IsGitSHA, tt.wantGitSHA)
			}
		})
	}
}
