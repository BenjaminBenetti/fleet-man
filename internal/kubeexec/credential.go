//go:build !nok8s

package kubeexec

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/pkg/apis/clientauthentication"
	"k8s.io/client-go/pkg/apis/clientauthentication/install"
	"k8s.io/client-go/rest"
)

// Credential describes what a connection authenticates with, as far as its
// expiry goes. It never holds the secret itself.
type Credential struct {
	Auth AuthKind
	// ExpiresAt is when the credential stops working; zero when it has no
	// known expiry (an opaque token, basic auth, no auth).
	ExpiresAt time.Time
	// Refreshable: once it expires a new one comes without anyone logging in
	// (the plugin mints one, a token file is rotated in place). The expiry
	// of a credential that is not refreshable is when the connection stops
	// working until its kubeconfig credential is replaced; that of a plugin
	// is when its next run happens, which can still fail when the login
	// behind it has lapsed.
	Refreshable bool
}

// Credential reads the expiry of the connection's credential. For an exec
// plugin that means running it once (as client-go would: same path,
// environment and request) and reading the ExecCredential's
// status.expirationTimestamp; for a client certificate its NotAfter; for a
// token, the exp claim when it is a JWT. A failing plugin returns a
// *PluginError whose message is safe to relay. Each call runs the plugin, so
// callers pace it (a plugin may be slow, or call out to a cloud API).
func (c *Connection) Credential(ctx context.Context) (Credential, error) {
	cfg := c.config
	switch c.Auth {
	case AuthExecPlugin:
		return c.Plugin.credential(ctx)
	case AuthClientCert:
		data := cfg.CertData
		if len(data) == 0 {
			var err error
			if data, err = os.ReadFile(cfg.CertFile); err != nil {
				return Credential{Auth: c.Auth}, fmt.Errorf("reading client certificate: %w", err)
			}
		}
		notAfter, err := certNotAfter(data)
		if err != nil {
			return Credential{Auth: c.Auth}, err
		}
		return Credential{Auth: c.Auth, ExpiresAt: notAfter}, nil
	case AuthToken:
		return Credential{Auth: c.Auth, ExpiresAt: jwtExpiry(cfg.BearerToken)}, nil
	case AuthTokenFile:
		data, err := os.ReadFile(cfg.BearerTokenFile)
		if err != nil {
			return Credential{Auth: c.Auth, Refreshable: true}, fmt.Errorf("reading token file: %w", err)
		}
		return Credential{Auth: c.Auth, ExpiresAt: jwtExpiry(strings.TrimSpace(string(data))), Refreshable: true}, nil
	}
	return Credential{Auth: c.Auth}, nil
}

func (p *Plugin) credential(ctx context.Context) (Credential, error) {
	out, err := p.run(ctx)
	if err != nil {
		return Credential{Auth: AuthExecPlugin, Refreshable: true}, err
	}
	cred, err := p.decode(out)
	if err != nil {
		return Credential{Auth: AuthExecPlugin, Refreshable: true}, &PluginError{Path: p.Path, Err: err}
	}
	got := Credential{Auth: AuthExecPlugin, Refreshable: true}
	if exp := cred.Status.ExpirationTimestamp; exp != nil {
		got.ExpiresAt = exp.Time
	}
	if cred.Status.ClientCertificateData != "" {
		notAfter, err := certNotAfter([]byte(cred.Status.ClientCertificateData))
		if err != nil {
			return got, &PluginError{Path: p.Path, Err: err}
		}
		if got.ExpiresAt.IsZero() || notAfter.Before(got.ExpiresAt) {
			got.ExpiresAt = notAfter
		}
	}
	return got, nil
}

// The client.authentication.k8s.io codecs, built on first use (never at
// init) the way client-go's exec authenticator builds its own.
var (
	authCodecsOnce sync.Once
	authCodecs     serializer.CodecFactory
)

func authCodecFactory() serializer.CodecFactory {
	authCodecsOnce.Do(func() {
		scheme := runtime.NewScheme()
		install.Install(scheme)
		authCodecs = serializer.NewCodecFactory(scheme)
	})
	return authCodecs
}

func (p *Plugin) groupVersion() (schema.GroupVersion, error) {
	gv, err := schema.ParseGroupVersion(p.APIVersion)
	if err != nil || gv.Group != clientauthentication.SchemeGroupVersion.Group || (gv.Version != "v1" && gv.Version != "v1beta1") {
		return schema.GroupVersion{}, fmt.Errorf("%w: exec plugin apiVersion %q is not client.authentication.k8s.io/v1 or v1beta1", ErrMisconfigured, p.APIVersion)
	}
	return gv, nil
}

// execInfo is the KUBERNETES_EXEC_INFO request: non-interactive, with the
// cluster when the kubeconfig asks for it to be provided.
func (p *Plugin) execInfo() ([]byte, error) {
	gv, err := p.groupVersion()
	if err != nil {
		return nil, err
	}
	cred := &clientauthentication.ExecCredential{Spec: clientauthentication.ExecCredentialSpec{Interactive: false}}
	if p.provideClusterInfo {
		cluster, err := rest.ConfigToExecCluster(p.restConfig)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrMisconfigured, err)
		}
		cred.Spec.Cluster = cluster
	}
	return runtime.Encode(authCodecFactory().LegacyCodec(gv), cred)
}

// decode checks a plugin's stdout the way client-go does.
func (p *Plugin) decode(out []byte) (*clientauthentication.ExecCredential, error) {
	gv, err := p.groupVersion()
	if err != nil {
		return nil, err
	}
	cred := &clientauthentication.ExecCredential{}
	_, gvk, err := authCodecFactory().UniversalDecoder(gv).Decode(out, nil, cred)
	switch {
	case err != nil:
		return nil, fmt.Errorf("decoding its output: %v", err)
	case gvk.Group != gv.Group || gvk.Version != gv.Version:
		return nil, fmt.Errorf("it is configured for %s but returned %s", gv, gvk.GroupVersion())
	case cred.Status == nil:
		return nil, errors.New("its output has no status")
	case cred.Status.Token == "" && cred.Status.ClientCertificateData == "":
		return nil, errors.New("its output has neither a token nor a client certificate")
	case (cred.Status.ClientCertificateData == "") != (cred.Status.ClientKeyData == ""):
		return nil, errors.New("its output has a client certificate or key without the other")
	}
	if cred.Status.ClientCertificateData != "" {
		if _, err := tls.X509KeyPair([]byte(cred.Status.ClientCertificateData), []byte(cred.Status.ClientKeyData)); err != nil {
			return nil, fmt.Errorf("its client certificate does not parse: %v", err)
		}
	}
	return cred, nil
}

// certNotAfter is the expiry of the first certificate in a PEM bundle.
func certNotAfter(data []byte) (time.Time, error) {
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return time.Time{}, errors.New("no certificate in the client certificate data")
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return time.Time{}, fmt.Errorf("parsing the client certificate: %w", err)
		}
		return cert.NotAfter, nil
	}
}

// jwtExpiry is a JWT's exp claim, or zero for a token that is not a JWT or
// has none. The signature is not checked: this only predicts expiry.
func jwtExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return time.Time{}
	}
	if exp, err := claims.Exp.Float64(); err == nil && exp > 0 {
		return time.Unix(int64(exp), 0)
	}
	return time.Time{}
}
