package registry

import "testing"

func TestParseRef(t *testing.T) {
	cases := []struct {
		input   string
		wantSrc Source
		wantRef string
		wantNS  string
	}{
		{"ubuntu:22.04", SourceRegistry, "ubuntu:22.04", ""},
		{"myacr.azurecr.io/img:1", SourceRegistry, "myacr.azurecr.io/img:1", ""},
		{"docker-daemon:ubuntu:22.04", SourceDockerDaemon, "ubuntu:22.04", ""},
		{"docker-daemon:myregistry.local:5000/img:tag", SourceDockerDaemon, "myregistry.local:5000/img:tag", ""},
		{"containerd:alpine:3.20", SourceContainerd, "alpine:3.20", "default"},
		{"containerd://k8s.io/alpine:3.20", SourceContainerd, "alpine:3.20", "k8s.io"},
		{"containerd://moby/docker.io/library/alpine:3.20", SourceContainerd, "docker.io/library/alpine:3.20", "moby"},
	}
	for _, tc := range cases {
		got := ParseRef(tc.input)
		if got.Source != tc.wantSrc || got.Ref != tc.wantRef || got.Namespace != tc.wantNS {
			t.Errorf("ParseRef(%q) = {%d %q %q}, want {%d %q %q}",
				tc.input, got.Source, got.Ref, got.Namespace,
				tc.wantSrc, tc.wantRef, tc.wantNS)
		}
	}
}
