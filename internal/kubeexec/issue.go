//go:build !nok8s

package kubeexec

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/pkg/apis/clientauthentication"
	"k8s.io/client-go/rest"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// How long an issued plugin credential is reused.
const (
	// issueMargin: a cached credential with less than this left is minted
	// again, so the one handed out still works when its holder dials.
	issueMargin = time.Minute
	// issueMaxAge bounds the reuse of a plugin credential with no expiry
	// (client-go keeps one until the server refuses it; a holder that sees a
	// refusal asks again with refresh).
	issueMaxAge = 10 * time.Minute
	// issueTimeout bounds one run of the plugin for an issue. The run is not
	// tied to any one caller's context, so a caller giving up does not fail
	// the others waiting on the same run.
	issueTimeout = time.Minute
)

// Issuer hands out the complete transport a connection reaches its API
// server with (fleet.KubeExecCredential): the server, its trust, the proxy
// and impersonation, and a live credential. A credential plugin's output is
// cached until shortly before it expires, so a burst of attaches runs the
// plugin once, and concurrent requests share one run. Static credentials
// (a client certificate, a token, a token file) are read from the
// kubeconfig, or the files it names, on every issue, so a rotated file is
// picked up. It is safe for concurrent use.
type Issuer struct {
	conn *Connection
	now  func() time.Time

	mu     sync.Mutex
	cached *mintedCredential
	flight *mintFlight
}

type mintedCredential struct {
	status   clientauthentication.ExecCredentialStatus
	expires  time.Time
	mintedAt time.Time
}

type mintFlight struct {
	done   chan struct{}
	minted mintedCredential
	err    error
}

// NewIssuer issues conn's transport.
func NewIssuer(conn *Connection) *Issuer {
	return &Issuer{conn: conn, now: time.Now}
}

// Issue returns the connection's transport with a credential that works
// now. refresh discards a cached plugin credential first: the holder's
// request was refused with it. A failing plugin returns its *PluginError.
func (i *Issuer) Issue(ctx context.Context, refresh bool) (fleet.KubeExecCredential, error) {
	cfg := i.conn.RESTConfig()
	out := fleet.KubeExecCredential{
		ClusterID:             i.conn.ClusterID,
		Server:                cfg.Host,
		TLSServerName:         cfg.TLSClientConfig.ServerName,
		InsecureSkipTLSVerify: cfg.TLSClientConfig.Insecure,
		Auth:                  string(i.conn.Auth),
		Impersonate: fleet.KubeImpersonation{
			User:   cfg.Impersonate.UserName,
			UID:    cfg.Impersonate.UID,
			Groups: append([]string(nil), cfg.Impersonate.Groups...),
		},
	}
	if len(cfg.Impersonate.Extra) > 0 {
		out.Impersonate.Extra = make(map[string][]string, len(cfg.Impersonate.Extra))
		for k, v := range cfg.Impersonate.Extra {
			out.Impersonate.Extra[k] = append([]string(nil), v...)
		}
	}
	ca, err := fileOrData(cfg.TLSClientConfig.CAData, cfg.TLSClientConfig.CAFile, "certificate authority")
	if err != nil {
		return fleet.KubeExecCredential{}, err
	}
	out.CertificateAuthorityData = ca
	if out.ProxyURL, err = proxyURL(cfg); err != nil {
		return fleet.KubeExecCredential{}, err
	}

	switch i.conn.Auth {
	case AuthExecPlugin:
		m, err := i.minted(ctx, refresh)
		if err != nil {
			return fleet.KubeExecCredential{}, err
		}
		out.BearerToken = m.status.Token
		if m.status.ClientCertificateData != "" {
			out.ClientCertificateData = []byte(m.status.ClientCertificateData)
			out.ClientKeyData = []byte(m.status.ClientKeyData)
		}
		out.ExpiresAt = m.expires
	case AuthClientCert:
		if out.ClientCertificateData, err = fileOrData(cfg.TLSClientConfig.CertData, cfg.TLSClientConfig.CertFile, "client certificate"); err != nil {
			return fleet.KubeExecCredential{}, err
		}
		if out.ClientKeyData, err = fileOrData(cfg.TLSClientConfig.KeyData, cfg.TLSClientConfig.KeyFile, "client key"); err != nil {
			return fleet.KubeExecCredential{}, err
		}
		if out.ExpiresAt, err = certNotAfter(out.ClientCertificateData); err != nil {
			return fleet.KubeExecCredential{}, err
		}
	case AuthToken:
		out.BearerToken = cfg.BearerToken
		out.ExpiresAt = jwtExpiry(out.BearerToken)
	case AuthTokenFile:
		data, err := os.ReadFile(cfg.BearerTokenFile)
		if err != nil {
			return fleet.KubeExecCredential{}, fmt.Errorf("reading token file: %w", err)
		}
		out.BearerToken = strings.TrimSpace(string(data))
		out.ExpiresAt = jwtExpiry(out.BearerToken)
	case AuthBasic:
		out.Username, out.Password = cfg.Username, cfg.Password
	}
	return out, nil
}

// minted is the plugin's cached credential, minting one when there is none
// still good (or refresh asks for a new one).
func (i *Issuer) minted(ctx context.Context, refresh bool) (mintedCredential, error) {
	i.mu.Lock()
	if c := i.cached; c != nil && !refresh && i.fresh(c) {
		got := *c
		i.mu.Unlock()
		return got, nil
	}
	f := i.flight
	if f == nil {
		f = &mintFlight{done: make(chan struct{})}
		i.flight = f
		go i.mint(f)
	}
	i.mu.Unlock()
	select {
	case <-f.done:
	case <-ctx.Done():
		return mintedCredential{}, ctx.Err()
	}
	if f.err != nil {
		return mintedCredential{}, f.err
	}
	// The run this caller started or joined has just minted a credential,
	// which is what a refresh asks for, and as new as there is (a plugin that
	// mints credentials shorter-lived than issueMargin gets one run per
	// issue, never a loop).
	return f.minted, nil
}

func (i *Issuer) mint(f *mintFlight) {
	ctx, cancel := context.WithTimeout(context.Background(), issueTimeout)
	defer cancel()
	status, expires, err := i.conn.Plugin.mint(ctx)
	i.mu.Lock()
	if err == nil {
		f.minted = mintedCredential{status: *status, expires: expires, mintedAt: i.now()}
		i.cached = &f.minted
	}
	f.err = err
	i.flight = nil
	i.mu.Unlock()
	close(f.done)
}

// fresh reports whether c can be handed out now.
func (i *Issuer) fresh(c *mintedCredential) bool {
	now := i.now()
	if c.expires.IsZero() {
		return now.Sub(c.mintedAt) < issueMaxAge
	}
	return c.expires.Sub(now) > issueMargin
}

// fileOrData is a kubeconfig value given inline or as a file.
func fileOrData(data []byte, file, what string) ([]byte, error) {
	if len(data) > 0 || file == "" {
		return append([]byte(nil), data...), nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", what, err)
	}
	return b, nil
}

// proxyURL is the proxy cfg's client reaches the API server through: the
// kubeconfig's proxy-url, else the daemon's proxy environment as client-go
// reads it (NO_PROXY CIDRs included); "" when it goes direct.
func proxyURL(cfg *rest.Config) (string, error) {
	server, _, err := rest.DefaultServerUrlFor(cfg)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrMisconfigured, err)
	}
	proxy := cfg.Proxy
	if proxy == nil {
		proxy = utilnet.NewProxierWithNoProxyCIDR(http.ProxyFromEnvironment)
	}
	u, err := proxy(&http.Request{Method: http.MethodGet, URL: server, Header: http.Header{}})
	if err != nil {
		return "", fmt.Errorf("%w: proxy for %s: %v", ErrMisconfigured, server, err)
	}
	if u == nil {
		return "", nil
	}
	return u.String(), nil
}

// RESTConfigFor is the client configuration of an issued transport: the
// kube-exec shim's direct mode dials with it. The proxy is exactly the one
// fleetd uses (none when it goes direct), never the caller's environment.
func RESTConfigFor(cred fleet.KubeExecCredential) (*rest.Config, error) {
	if cred.Server == "" {
		return nil, errors.New("the issued credential names no API server")
	}
	cfg := &rest.Config{
		Host:        cred.Server,
		BearerToken: cred.BearerToken,
		Username:    cred.Username,
		Password:    cred.Password,
		TLSClientConfig: rest.TLSClientConfig{
			Insecure:   cred.InsecureSkipTLSVerify,
			ServerName: cred.TLSServerName,
			CAData:     append([]byte(nil), cred.CertificateAuthorityData...),
			CertData:   append([]byte(nil), cred.ClientCertificateData...),
			KeyData:    append([]byte(nil), cred.ClientKeyData...),
		},
		Impersonate: rest.ImpersonationConfig{
			UserName: cred.Impersonate.User,
			UID:      cred.Impersonate.UID,
			Groups:   append([]string(nil), cred.Impersonate.Groups...),
		},
		QPS:       QPS,
		Burst:     Burst,
		UserAgent: userAgent(),
	}
	if len(cred.Impersonate.Extra) > 0 {
		cfg.Impersonate.Extra = make(map[string][]string, len(cred.Impersonate.Extra))
		for k, v := range cred.Impersonate.Extra {
			cfg.Impersonate.Extra[k] = append([]string(nil), v...)
		}
	}
	if cred.ProxyURL == "" {
		cfg.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	} else {
		u, err := url.Parse(cred.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("the issued proxy URL does not parse: %v", err)
		}
		cfg.Proxy = http.ProxyURL(u)
	}
	return cfg, nil
}
