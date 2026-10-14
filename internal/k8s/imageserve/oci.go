package imageserve

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// The OCI and docker media types the image server reads and writes.
const (
	mediaOCIIndex     = "application/vnd.oci.image.index.v1+json"
	mediaDockerList   = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaOCIManifest  = "application/vnd.oci.image.manifest.v1+json"
	mediaDockerV2     = "application/vnd.docker.distribution.manifest.v2+json"
	mediaOCIConfig    = "application/vnd.oci.image.config.v1+json"
	mediaOCILayer     = "application/vnd.oci.image.layer.v1.tar"
	mediaOCILayerGzip = "application/vnd.oci.image.layer.v1.tar+gzip"
	mediaDockerLayer  = "application/vnd.docker.image.rootfs.diff.tar"
	mediaDockerGzip   = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	mediaOctetStream  = "application/octet-stream"
)

// attestationAnnotation marks the attestation manifests (provenance, SBOM)
// buildkit adds to an image index; they are not runnable images.
const attestationAnnotation = "vnd.docker.reference.type"

// descriptor is an OCI content descriptor.
type descriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	URLs         []string          `json:"urls,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	Platform     *ociPlatform      `json:"platform,omitempty"`
	ArtifactType string            `json:"artifactType,omitempty"`
}

// ociPlatform is a descriptor's (or an image config's) platform.
type ociPlatform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

// manifest is an OCI image manifest (or a docker v2 one: the same shape).
type manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Config        descriptor        `json:"config"`
	Layers        []descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// index is an OCI image index (or a docker manifest list).
type index struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType,omitempty"`
	Manifests     []descriptor `json:"manifests"`
}

// imageConfig is the part of an image config the server reads.
type imageConfig struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

func isIndexType(mediaType string) bool {
	return mediaType == mediaOCIIndex || mediaType == mediaDockerList
}

func isManifestType(mediaType string) bool {
	return mediaType == mediaOCIManifest || mediaType == mediaDockerV2
}

// isCompressedLayer reports whether a layer media type names compressed
// content (gzip or zstd, OCI or docker spelling): such layers are served as
// they are.
func isCompressedLayer(mediaType string) bool {
	return strings.HasSuffix(mediaType, "+gzip") || strings.HasSuffix(mediaType, "+zstd") ||
		strings.HasSuffix(mediaType, ".tar.gzip") || strings.HasSuffix(mediaType, ".tar.zstd")
}

// gzipLayerType is the media type of a layer of type mediaType once gzip
// compressed: docker's spelling for a docker layer, OCI's otherwise.
func gzipLayerType(mediaType string) string {
	switch {
	case strings.HasPrefix(mediaType, "application/vnd.docker."):
		return mediaDockerGzip
	case strings.HasPrefix(mediaType, "application/vnd.oci.") && strings.HasSuffix(mediaType, ".tar"):
		return mediaType + "+gzip"
	}
	return mediaOCILayerGzip
}

// The magic bytes of the compressed formats a layer may arrive in.
var (
	gzipMagic = []byte{0x1f, 0x8b}
	zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}
)

// digestPattern is a sha256 digest, the only algorithm docker and the OCI
// image layout use for content.
var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// validDigest reports whether d is a sha256 digest.
func validDigest(d string) bool { return digestPattern.MatchString(d) }

// digestHex is a valid digest's hex part.
func digestHex(d string) string { return strings.TrimPrefix(d, "sha256:") }

func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// short is a digest's first 12 hex digits, for logs.
func short(d string) string {
	h := digestHex(d)
	return h[:min(12, len(h))]
}

// platform is a parsed "os/arch[/variant]".
type platform struct {
	os, arch, variant string
}

func (p platform) String() string {
	s := p.os + "/" + p.arch
	if p.variant != "" {
		s += "/" + p.variant
	}
	return s
}

// parsePlatform parses "os/arch[/variant]", normalizing the architecture
// names uname and some tools use (x86_64, aarch64) to Go's.
func parsePlatform(s string) (platform, error) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(s)), "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return platform{}, fmt.Errorf("invalid platform %q (want os/arch[/variant])", s)
	}
	p := platform{os: parts[0], arch: normalizeArch(parts[1])}
	if len(parts) == 3 {
		p.variant = parts[2]
	}
	return p, nil
}

func normalizeArch(a string) string {
	switch a {
	case "x86_64", "x86-64":
		return "amd64"
	case "aarch64":
		return "arm64"
	}
	return a
}

// matches reports whether an image for (os, arch, variant) runs on p: the
// same os and architecture, and the same variant when both name one (arm64
// without a variant is v8).
func (p platform) matches(os, arch, variant string) bool {
	if !strings.EqualFold(os, p.os) || normalizeArch(strings.ToLower(arch)) != p.arch {
		return false
	}
	want, got := p.variant, strings.ToLower(variant)
	if p.arch == "arm64" {
		if want == "" {
			want = "v8"
		}
		if got == "" {
			got = "v8"
		}
	}
	return want == "" || got == "" || want == got
}
