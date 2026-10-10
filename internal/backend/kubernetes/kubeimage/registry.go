package kubeimage

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The manifest media types fleet reads, index types first.
const (
	mediaOCIIndex      = "application/vnd.oci.image.index.v1+json"
	mediaDockerList    = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaOCIManifest   = "application/vnd.oci.image.manifest.v1+json"
	mediaDockerV2      = "application/vnd.docker.distribution.manifest.v2+json"
	manifestAcceptList = mediaOCIIndex + ", " + mediaDockerList + ", " + mediaOCIManifest + ", " + mediaDockerV2
)

// Limits on what the registry client reads into memory.
const (
	maxManifestSize = 4 << 20
	maxConfigSize   = 8 << 20
	maxErrorBody    = 64 << 10
)

// registryTimeout bounds one registry request; a request carries its
// caller's context too.
const registryTimeout = 60 * time.Second

// RegistryError is an HTTP error answer from a registry, with the OCI error
// codes its body named ("MANIFEST_UNKNOWN", "UNSUPPORTED", "DENIED").
type RegistryError struct {
	Method string
	URL    string
	Status int
	Codes  []string
	// Message is the first error's message, or the body's start.
	Message string
}

func (e *RegistryError) Error() string {
	msg := e.Message
	if len(e.Codes) > 0 {
		msg = e.Codes[0] + ": " + msg
	}
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.URL, e.Status, strings.TrimSpace(msg))
}

// has reports whether the registry named code.
func (e *RegistryError) has(code string) bool {
	for _, c := range e.Codes {
		if c == code {
			return true
		}
	}
	return false
}

// isNotFound reports whether err says the manifest or repository is not in
// the registry.
func isNotFound(err error) bool {
	var re *RegistryError
	return errors.As(err, &re) && re.Status == http.StatusNotFound
}

// isDenied reports whether err is the registry refusing the credential (or
// the lack of one).
func isDenied(err error) bool {
	var re *RegistryError
	return errors.As(err, &re) && (re.Status == http.StatusUnauthorized || re.Status == http.StatusForbidden)
}

// registryClient speaks the distribution API to one registry, with docker's
// token handshake: a 401 names a Bearer realm (or Basic), the client gets a
// token there with the daemon host's credential and retries.
type registryClient struct {
	domain   string
	insecure bool
	// cred is the credential; empty for anonymous requests.
	cred  Credential
	httpc *http.Client
	// plain is the insecure client's fallback for a registry on plain HTTP.
	plain *http.Client

	mu     sync.Mutex
	scheme string
	// tokens are bearer tokens by scope; basic is set once a registry asks
	// for Basic.
	tokens map[string]string
	basic  bool
}

// newRegistryClient is a client for domain. insecure allows plain HTTP and
// an untrusted certificate; a loopback registry gets plain HTTP as docker
// gives it. transport overrides the HTTP transport (tests).
func newRegistryClient(domain string, insecure bool, cred Credential, transport http.RoundTripper) *registryClient {
	insecure = insecure || isLoopback(domain)
	if transport == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		if insecure {
			t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // the registry is configured insecure
		}
		transport = t
	}
	c := &registryClient{
		domain:   domain,
		insecure: insecure,
		cred:     cred,
		httpc:    &http.Client{Transport: transport, Timeout: registryTimeout, CheckRedirect: keepAuthOnSameHost},
		tokens:   map[string]string{},
		scheme:   "https",
	}
	return c
}

// keepAuthOnSameHost follows redirects (blob downloads go to storage
// backends), dropping the Authorization header on another host.
func keepAuthOnSameHost(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if req.URL.Host != via[0].URL.Host {
		req.Header.Del("Authorization")
	}
	return nil
}

func (c *registryClient) baseURL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scheme + "://" + apiHost(c.domain)
}

// do sends one request, answering an auth challenge once. scope is the
// token scope to ask for when the challenge names none.
func (c *registryClient) do(ctx context.Context, method, path string, header http.Header, scope string) (*http.Response, error) {
	resp, err := c.send(ctx, method, path, header, scope)
	if err != nil && c.insecure && c.scheme == "https" && looksLikePlainHTTP(err) {
		c.mu.Lock()
		c.scheme = "http"
		c.mu.Unlock()
		resp, err = c.send(ctx, method, path, header, scope)
	}
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	drain(resp)
	scheme, params := parseChallenge(challenge)
	switch scheme {
	case "bearer":
		want := scope
		if params["scope"] != "" {
			want = params["scope"]
		}
		token, err := c.fetchToken(ctx, params["realm"], params["service"], want)
		if err != nil {
			return nil, err
		}
		// Kept under the caller's scope too, so its next request goes
		// straight out with the token.
		c.mu.Lock()
		c.tokens[scope] = token
		c.tokens[want] = token
		c.mu.Unlock()
	case "basic":
		if c.cred.Username == "" {
			return nil, &RegistryError{Method: method, URL: c.baseURL() + path, Status: http.StatusUnauthorized, Codes: []string{"UNAUTHORIZED"}, Message: "the registry needs a login, and the daemon host has none for " + c.domain}
		}
		c.mu.Lock()
		c.basic = true
		c.mu.Unlock()
	default:
		return nil, &RegistryError{Method: method, URL: c.baseURL() + path, Status: http.StatusUnauthorized, Codes: []string{"UNAUTHORIZED"}, Message: "unauthorized, with no usable challenge (" + challenge + ")"}
	}
	return c.send(ctx, method, path, header, scope)
}

func (c *registryClient) send(ctx context.Context, method, path string, header http.Header, scope string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL()+path, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	c.mu.Lock()
	token, basic := c.tokens[scope], c.basic
	c.mu.Unlock()
	switch {
	case token != "":
		req.Header.Set("Authorization", "Bearer "+token)
	case basic:
		req.SetBasicAuth(c.cred.Username, c.cred.Password)
	}
	return c.httpc.Do(req)
}

// looksLikePlainHTTP reports whether an HTTPS request failed because the
// registry speaks plain HTTP.
func looksLikePlainHTTP(err error) bool {
	s := err.Error()
	return strings.Contains(s, "server gave HTTP response to HTTPS client") ||
		strings.Contains(s, "first record does not look like a TLS handshake") ||
		strings.Contains(s, "tls: ")
}

// fetchToken gets a bearer token for scope from realm: GET with Basic auth
// for a username and password, the OAuth2 refresh grant for an identity
// token, anonymous without either.
func (c *registryClient) fetchToken(ctx context.Context, realm, service, scope string) (string, error) {
	if realm == "" {
		return "", fmt.Errorf("registry %s: bearer challenge without a realm", c.domain)
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("registry %s: bad token realm %q: %w", c.domain, realm, err)
	}
	// A login goes to a token server over TLS only, unless the registry
	// itself is reached insecurely.
	if u.Scheme != "https" && !c.insecure && !c.cred.Empty() {
		return "", fmt.Errorf("registry %s: refusing to send the daemon host's login to the plain-HTTP token realm %s", c.domain, u.Redacted())
	}
	var req *http.Request
	if c.cred.IdentityToken != "" {
		form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c.cred.IdentityToken}, "client_id": {"fleet"}}
		if service != "" {
			form.Set("service", service)
		}
		if scope != "" {
			form.Set("scope", scope)
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(form.Encode()))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		q := u.Query()
		if service != "" {
			q.Set("service", service)
		}
		for _, s := range strings.Fields(scope) {
			q.Add("scope", s)
		}
		u.RawQuery = q.Encode()
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return "", err
		}
		if c.cred.Username != "" {
			req.SetBasicAuth(c.cred.Username, c.cred.Password)
		}
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("registry %s: token request: %w", c.domain, err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return "", registryError(resp)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&body); err != nil {
		return "", fmt.Errorf("registry %s: unreadable token reply: %w", c.domain, err)
	}
	if body.Token == "" {
		body.Token = body.AccessToken
	}
	if body.Token == "" {
		return "", fmt.Errorf("registry %s: token reply without a token", c.domain)
	}
	return body.Token, nil
}

// parseChallenge reads a WWW-Authenticate header: its scheme, lowercased,
// and its parameters (quoted values may hold commas).
func parseChallenge(h string) (string, map[string]string) {
	h = strings.TrimSpace(h)
	scheme, rest, _ := strings.Cut(h, " ")
	params := map[string]string{}
	for rest = strings.TrimSpace(rest); rest != ""; {
		key, after, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		var val string
		if strings.HasPrefix(after, `"`) {
			end := 1
			for end < len(after) && after[end] != '"' {
				if after[end] == '\\' {
					end++
				}
				end++
			}
			val = strings.ReplaceAll(after[1:min(end, len(after))], `\"`, `"`)
			rest = after[min(end+1, len(after)):]
		} else {
			val, rest, _ = strings.Cut(after, ",")
			val = strings.TrimSpace(val)
			rest = "," + rest
		}
		params[key] = val
		rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), ","))
	}
	return strings.ToLower(scheme), params
}

// registryError reads an error answer.
func registryError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	e := &RegistryError{Method: resp.Request.Method, URL: resp.Request.URL.Redacted(), Status: resp.StatusCode}
	var oci struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
		// Token servers answer {"details": ...} or {"error": ...}.
		Details string `json:"details"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(body, &oci) == nil {
		for _, x := range oci.Errors {
			e.Codes = append(e.Codes, x.Code)
			if e.Message == "" {
				e.Message = x.Message
			}
		}
		if e.Message == "" {
			e.Message = oci.Details + oci.Error
		}
	}
	if e.Message == "" {
		e.Message = truncate(strings.TrimSpace(string(body)), 200)
	}
	if e.Message == "" {
		e.Message = http.StatusText(resp.StatusCode)
	}
	return e
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	_ = resp.Body.Close()
}

func pullScope(repoPath string) string { return "repository:" + repoPath + ":pull" }

// ping reports whether the registry answers its /v2/ endpoint (an auth
// challenge counts).
func (c *registryClient) ping(ctx context.Context) error {
	resp, err := c.send(ctx, http.MethodGet, "/v2/", nil, "")
	if err != nil && c.insecure && looksLikePlainHTTP(err) {
		c.mu.Lock()
		c.scheme = "http"
		c.mu.Unlock()
		resp, err = c.send(ctx, http.MethodGet, "/v2/", nil, "")
	}
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusUnauthorized {
		return nil
	}
	return registryError(resp)
}

// headManifest returns the digest of a repository's manifest by tag or
// digest. A registry that leaves Docker-Content-Digest out of a HEAD is asked
// with a GET.
func (c *registryClient) headManifest(ctx context.Context, repoPath, ref string) (string, error) {
	path := "/v2/" + repoPath + "/manifests/" + ref
	resp, err := c.do(ctx, http.MethodHead, path, http.Header{"Accept": {manifestAcceptList}}, pullScope(repoPath))
	if err != nil {
		return "", err
	}
	drain(resp)
	if resp.StatusCode != http.StatusOK {
		resp.Body = io.NopCloser(strings.NewReader(""))
		return "", registryError(resp)
	}
	if d := resp.Header.Get("Docker-Content-Digest"); digestPattern.MatchString(d) {
		return d, nil
	}
	_, _, d, err := c.getManifest(ctx, repoPath, ref)
	return d, err
}

// getManifest fetches a manifest; its digest is computed from the bytes and
// must match ref when ref is a digest.
func (c *registryClient) getManifest(ctx context.Context, repoPath, ref string) ([]byte, string, string, error) {
	path := "/v2/" + repoPath + "/manifests/" + ref
	resp, err := c.do(ctx, http.MethodGet, path, http.Header{"Accept": {manifestAcceptList}}, pullScope(repoPath))
	if err != nil {
		return nil, "", "", err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", registryError(resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestSize+1))
	if err != nil {
		return nil, "", "", fmt.Errorf("reading manifest %s: %w", path, err)
	}
	if len(body) > maxManifestSize {
		return nil, "", "", fmt.Errorf("manifest %s is larger than %d bytes", path, maxManifestSize)
	}
	digest := sha256Digest(body)
	if strings.HasPrefix(ref, "sha256:") && ref != digest {
		return nil, "", "", fmt.Errorf("manifest %s has digest %s", path, digest)
	}
	mediaType := resp.Header.Get("Content-Type")
	var probe struct {
		MediaType string `json:"mediaType"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.MediaType != "" {
		mediaType = probe.MediaType
	}
	return body, mediaType, digest, nil
}

// getBlob fetches a blob of at most limit bytes and checks its digest.
func (c *registryClient) getBlob(ctx context.Context, repoPath, digest string, limit int64) ([]byte, error) {
	path := "/v2/" + repoPath + "/blobs/" + digest
	resp, err := c.do(ctx, http.MethodGet, path, nil, pullScope(repoPath))
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, registryError(resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading blob %s: %w", path, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("blob %s is larger than %d bytes", path, limit)
	}
	if got := sha256Digest(body); got != digest {
		return nil, fmt.Errorf("blob %s has digest %s", path, got)
	}
	return body, nil
}

// deleteManifest deletes a manifest by digest. Callers check that repoPath
// is one of the daemon's own (isOwnRepository) first.
func (c *registryClient) deleteManifest(ctx context.Context, repoPath, digest string) error {
	path := "/v2/" + repoPath + "/manifests/" + digest
	resp, err := c.do(ctx, http.MethodDelete, path, nil, "repository:"+repoPath+":delete")
	if err != nil {
		return err
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusAccepted, http.StatusOK, http.StatusNoContent:
		return nil
	}
	return registryError(resp)
}

func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
