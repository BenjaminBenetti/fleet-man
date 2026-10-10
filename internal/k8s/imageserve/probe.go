package imageserve

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
)

// probe.go checks gateway delivery the way a node uses it. The probe image
// is a tiny image (one layer holding one small file) fleetd serves to a
// connection like any image it built: `fleet cluster test` fetches its
// manifest through the gateway's public URL from the daemon host
// (CheckRoute), and the deep test has a throwaway pod pull it. It is built
// in memory, the same bytes every time for a platform, so it is imported
// once and keeps its digest; its pulls are left out of the connection's pull
// statistics.

// probeHolder holds a connection's reference to the probe image. Nothing
// collects it but the end of the connection (Forget): it is a few hundred
// bytes, and the next test needs it.
const probeHolder = "fleet-probe"

// probeFile is the probe image's one file and its command: a text file, so
// a pod that pulled the image fails at its start, not at its pull.
const probeFile = "fleet-probe"

// probeContent is the file's content.
const probeContent = "Pulled through the Fleet gateway: this image only tests the route.\n"

// ProbeImage is the probe image as a connection serves it.
type ProbeImage struct {
	// Ref is what a pod pulls ("<prefix>/devcontainer@<digest>"), Digest
	// its manifest's digest.
	Ref, Digest string
	// PullSecret is the connection's gateway pull Secret, applied when the
	// Target had Secrets ("" otherwise).
	PullSecret string
}

// probeDigests are the digests of every probe image built in this process
// (manifests, configs and layers), whose pulls the handler does not count.
var probeDigests sync.Map

func isProbeDigest(d string) bool {
	_, ok := probeDigests.Load(d)
	return ok
}

// probeArchive is the probe image for p as a `docker save` archive (an OCI
// layout), its image ID (the config's digest) and its manifest's digest.
func probeArchive(p platform) (archive []byte, imageID, manifestDigest string, err error) {
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	hdr := &tar.Header{Name: probeFile, Mode: 0o644, Size: int64(len(probeContent)), ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}
	if err := tw.WriteHeader(hdr); err != nil {
		return nil, "", "", err
	}
	if _, err := io.WriteString(tw, probeContent); err != nil {
		return nil, "", "", err
	}
	if err := tw.Close(); err != nil {
		return nil, "", "", err
	}
	// Stored, not compressed: the bytes, and so the digest, cannot depend
	// on the compressor's version.
	var gz bytes.Buffer
	zw, err := gzip.NewWriterLevel(&gz, gzip.NoCompression)
	if err != nil {
		return nil, "", "", err
	}
	if _, err := zw.Write(layer.Bytes()); err != nil {
		return nil, "", "", err
	}
	if err := zw.Close(); err != nil {
		return nil, "", "", err
	}
	type rootfs struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	}
	type runConfig struct {
		Cmd []string `json:"Cmd"`
	}
	cfg, err := json.Marshal(struct {
		Architecture string    `json:"architecture"`
		OS           string    `json:"os"`
		Variant      string    `json:"variant,omitempty"`
		Config       runConfig `json:"config"`
		RootFS       rootfs    `json:"rootfs"`
	}{p.arch, p.os, p.variant, runConfig{Cmd: []string{"/" + probeFile}}, rootfs{Type: "layers", DiffIDs: []string{sha256Digest(layer.Bytes())}}})
	if err != nil {
		return nil, "", "", err
	}
	man, err := json.Marshal(manifest{
		SchemaVersion: 2,
		MediaType:     mediaOCIManifest,
		Config:        descriptor{MediaType: mediaOCIConfig, Digest: sha256Digest(cfg), Size: int64(len(cfg))},
		Layers:        []descriptor{{MediaType: mediaOCILayerGzip, Digest: sha256Digest(gz.Bytes()), Size: int64(gz.Len())}},
	})
	if err != nil {
		return nil, "", "", err
	}
	idx, err := json.Marshal(index{SchemaVersion: 2, MediaType: mediaOCIIndex, Manifests: []descriptor{{
		MediaType: mediaOCIManifest, Digest: sha256Digest(man), Size: int64(len(man)),
		Platform: &ociPlatform{OS: p.os, Architecture: p.arch, Variant: p.variant},
	}}})
	if err != nil {
		return nil, "", "", err
	}
	var out bytes.Buffer
	aw := tar.NewWriter(&out)
	files := []struct {
		name string
		body []byte
	}{
		{"oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)},
		{"index.json", idx},
		{"blobs/sha256/" + digestHex(sha256Digest(cfg)), cfg},
		{"blobs/sha256/" + digestHex(sha256Digest(gz.Bytes())), gz.Bytes()},
		{"blobs/sha256/" + digestHex(sha256Digest(man)), man},
	}
	for _, f := range files {
		if err := aw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
			return nil, "", "", err
		}
		if _, err := aw.Write(f.body); err != nil {
			return nil, "", "", err
		}
	}
	if err := aw.Close(); err != nil {
		return nil, "", "", err
	}
	for _, d := range []string{sha256Digest(man), sha256Digest(cfg), sha256Digest(gz.Bytes())} {
		probeDigests.Store(d, true)
	}
	return out.Bytes(), sha256Digest(cfg), sha256Digest(man), nil
}

// Probe makes the connection serve the probe image for the target's
// platform, as Deliver does an image it built, and returns its reference.
// With t.Secrets set the connection's gateway pull Secret is applied too (a
// pod is about to pull it); without, nothing is written to the cluster.
func (g *Gateway) Probe(ctx context.Context, t kubeimage.Target) (ProbeImage, error) {
	if ok, why := g.Available(t); !ok {
		return ProbeImage{}, unavailable(why)
	}
	if t.Cluster == nil || t.Cluster.ID == "" {
		return ProbeImage{}, errors.New("gateway delivery: the probe image needs a saved connection")
	}
	cred, err := g.Clusters.EnsureCredential(t.Cluster.ID)
	if err != nil {
		return ProbeImage{}, fmt.Errorf("gateway delivery: the connection's pull credential: %w", err)
	}
	prep, err := g.prepareProbe(ctx, t.Platform)
	if err != nil {
		return ProbeImage{}, err
	}
	ref, secret, err := g.serve(ctx, t, prep.Digest, probeHolder, cred)
	if err != nil {
		return ProbeImage{}, err
	}
	return ProbeImage{Ref: ref, Digest: prep.Digest, PullSecret: secret}, nil
}

// ProbeDigest is the probe image's manifest digest for plat ("" is the
// daemon's own os/arch), without serving it: an unsaved connection's route
// check asks for it without a credential.
func ProbeDigest(plat string) (string, error) {
	p, err := probePlatform(plat)
	if err != nil {
		return "", err
	}
	_, _, d, err := probeArchive(p)
	return d, err
}

func probePlatform(plat string) (platform, error) {
	if plat == "" {
		plat = "linux/" + runtime.GOARCH
	}
	return parsePlatform(plat)
}

// prepareProbe puts the probe image for plat in the cache, unless it is
// there already.
func (g *Gateway) prepareProbe(ctx context.Context, plat string) (Prepared, error) {
	p, err := probePlatform(plat)
	if err != nil {
		return Prepared{}, err
	}
	archive, id, _, err := probeArchive(p)
	if err != nil {
		return Prepared{}, err
	}
	if got, ok, err := g.Store.cached(id, p.String()); err != nil || ok {
		return got, err
	}
	return g.Store.Import(ctx, bytes.NewReader(archive), id, p.String())
}

// The steps a route check or a probe pull names when it fails.
const (
	// StepRoute: the request never reached fleetd's image server (the
	// gateway is down or unreachable, does not know this daemon's registry
	// id, or cannot reach fleetd over the tunnel).
	StepRoute = "route"
	// StepTLS: the gateway's certificate is not trusted.
	StepTLS = "tls"
	// StepCredential: the connection's pull credential was refused.
	StepCredential = "credential"
	// StepDigest: fleetd does not serve the image to the connection.
	StepDigest = "digest"
)

// RouteCheck is what fetching a manifest through the gateway's public URL
// found.
type RouteCheck struct {
	// Step is what failed ("" when the manifest came back).
	Step string
	// Message says what happened, Hint what to change.
	Message, Hint string
	// Host is the gateway host[:port] checked.
	Host string
}

// routeCheckTimeout bounds one check.
const routeCheckTimeout = 20 * time.Second

// CheckRoute fetches a manifest the connection serves through the
// gateway's public URL from the daemon host, with the connection's pull
// credential, as a node pulling it would: the route, the TLS certificate
// (against the daemon host's trust store, which may hold a CA the nodes
// lack: the deep test's pod is the real proof) and the credential. An empty
// clusterID checks route and TLS only (an unsaved connection has no
// credential): fleetd's image server must answer with its challenge.
func (g *Gateway) CheckRoute(ctx context.Context, clusterID, digest string) RouteCheck {
	if g.Tunnel == nil {
		return RouteCheck{Step: StepRoute, Message: "gateway delivery is not available in this daemon"}
	}
	prefix, why := g.Tunnel.Registry()
	if prefix == "" {
		return RouteCheck{Step: StepRoute, Message: why, Hint: unavailableHint}
	}
	host, rid, _ := strings.Cut(prefix, "/")
	rc := RouteCheck{Host: host}
	user, pass := "", ""
	if clusterID != "" {
		c, ok := g.Clusters.Lookup(clusterID)
		if !ok || c.Credential == "" {
			rc.Step, rc.Message = StepCredential, "the connection has no gateway pull credential"
			return rc
		}
		user, pass = clusterID, c.Credential
	}
	ctx, cancel := context.WithTimeout(ctx, routeCheckTimeout)
	defer cancel()
	// GET, not HEAD: a 404's body tells the gateway's (an unknown registry
	// id) from fleetd's (an unknown manifest). The probe's pulls are not
	// counted, and the manifest is a few hundred bytes.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/v2/"+rid+"/"+repoName+"/manifests/"+digest, nil)
	if err != nil {
		rc.Step, rc.Message = StepRoute, err.Error()
		return rc
	}
	req.Header.Set("Accept", mediaOCIManifest+", "+mediaDockerV2)
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	client := &http.Client{Transport: g.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		if isCertificateError(err) {
			rc.Step = StepTLS
			rc.Message = "the daemon host does not trust the TLS certificate of the gateway at " + host + ": " + err.Error()
			rc.Hint = "serve the gateway with a publicly trusted certificate (the nodes need to trust it too)"
			return rc
		}
		rc.Step = StepRoute
		rc.Message = "the daemon host cannot reach the gateway at " + host + ": " + err.Error()
		rc.Hint = "check that the gateway's --public-url names an address the daemon host and the cluster's nodes reach"
		return rc
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK && user != "" && sha256Digest(body) == digest:
		return rc
	case resp.StatusCode == http.StatusUnauthorized && user == "" && strings.Contains(resp.Header.Get("WWW-Authenticate"), `realm="`+realm+`"`):
		return rc
	case resp.StatusCode == http.StatusUnauthorized:
		rc.Step, rc.Message = StepCredential, "the gateway pull credential was refused"
		rc.Hint = "the credential lives on the connection's record; re-adding the connection makes a new one"
	case resp.StatusCode == http.StatusNotFound && user != "" && ociErrorCode(body) == "MANIFEST_UNKNOWN":
		rc.Step, rc.Message = StepDigest, "fleetd's image server does not serve the probe image to this connection (HTTP 404)"
	case resp.StatusCode == http.StatusNotFound:
		// The gateway's NAME_UNKNOWN (or, without a credential, anything
		// but fleetd's challenge): the registry id is not routed to fleetd.
		rc.Step, rc.Message = StepRoute, "the gateway at "+host+" does not route this daemon's registry id to it (HTTP 404)"
		rc.Hint = "the gateway forgot the session (it restarted without its --session-key, or fleetd was offline for more than 10 minutes); fleetd registers again on its own"
	case resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout:
		rc.Step, rc.Message = StepRoute, fmt.Sprintf("the gateway at %s could not reach fleetd over the tunnel (HTTP %d)", host, resp.StatusCode)
	default:
		rc.Step, rc.Message = StepRoute, fmt.Sprintf("the gateway at %s answered HTTP %d", host, resp.StatusCode)
	}
	return rc
}

// ociErrorCode is the first error code of an OCI distribution error body.
func ociErrorCode(body []byte) string {
	var e struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &e) != nil || len(e.Errors) == 0 {
		return ""
	}
	return e.Errors[0].Code
}

func isCertificateError(err error) bool {
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verify *tls.CertificateVerificationError
	return errors.As(err, &unknown) || errors.As(err, &hostname) || errors.As(err, &invalid) || errors.As(err, &verify)
}

// PullStep is the step a classified pull failure broke.
func PullStep(kind PullFailureKind) string {
	switch kind {
	case PullCertificate:
		return StepTLS
	case PullCredential:
		return StepCredential
	case PullDaemonOffline, PullGatewayUnreachable, PullRegistryOff:
		return StepRoute
	case PullStaleReference, PullDigestUnknown:
		return StepDigest
	}
	return ""
}
