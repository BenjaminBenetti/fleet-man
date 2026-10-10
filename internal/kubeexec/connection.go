//go:build !nok8s

package kubeexec

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/version"
)

// The ways Load fails, for mapping to a connection state. Load's errors wrap
// one of them.
var (
	// ErrKubeconfigMissing: none of the pinned kubeconfig files exists.
	ErrKubeconfigMissing = errors.New("kubeconfig not found")
	// ErrContextMissing: the kubeconfig has no such context.
	ErrContextMissing = errors.New("kubeconfig context not found")
	// ErrPluginMissing: the context's exec credential plugin is not
	// installed where the daemon can find it.
	ErrPluginMissing = errors.New("credential plugin not found")
	// ErrMisconfigured: the record or the kubeconfig cannot work as written.
	ErrMisconfigured = errors.New("misconfigured")
)

// Client rate limits for every connection.
const (
	QPS   = 20
	Burst = 50
)

// AuthKind is how a connection authenticates to its API server.
type AuthKind string

const (
	AuthNone       AuthKind = "none"
	AuthExecPlugin AuthKind = "exec-plugin"
	AuthClientCert AuthKind = "client-cert"
	AuthToken      AuthKind = "token"
	AuthTokenFile  AuthKind = "token-file"
	AuthBasic      AuthKind = "basic"
)

// Options are the daemon-side inputs of Load.
type Options struct {
	// Environ is the daemon's environment; nil reads os.Environ.
	Environ []string
}

// Connection is a KubeCluster record resolved against its kubeconfig. It is
// safe for concurrent use.
type Connection struct {
	// ClusterID and Namespace come from the record.
	ClusterID string
	Namespace string
	// Context and Paths are the pinned context and kubeconfig files.
	Context string
	Paths   []string
	// Server is the API server URL the context points at.
	Server string
	// Auth is how the context authenticates; Plugin is set for an exec
	// credential plugin.
	Auth   AuthKind
	Plugin *Plugin

	// configMu serializes copies of config: rest.CopyConfig writes through
	// the exec provider it shares with its source (see RESTConfig).
	configMu sync.Mutex
	config   *rest.Config

	clientOnce sync.Once
	clientset  kubernetes.Interface
	clientErr  error
}

// Load resolves a record. It reads only local files and runs nothing: the
// credential plugin runs on the first request (or Credential).
func Load(c fleet.KubeCluster, opts Options) (*Connection, error) {
	environ := opts.Environ
	if environ == nil {
		environ = os.Environ()
	}
	switch {
	case c.Context == "":
		return nil, fmt.Errorf("%w: no kubeconfig context", ErrMisconfigured)
	case len(c.KubeconfigPaths) == 0:
		return nil, fmt.Errorf("%w: no kubeconfig paths pinned", ErrMisconfigured)
	}
	if err := fleet.ValidateExtraPath(c.ExtraPath); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMisconfigured, err)
	}
	if err := fleet.ValidatePluginEnv(c.PluginEnv); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMisconfigured, err)
	}
	if !anyExists(c.KubeconfigPaths) {
		return nil, fmt.Errorf("%w: none of %q exists on the daemon host", ErrKubeconfigMissing, c.KubeconfigPaths)
	}

	// Explicit precedence, nothing from the environment. Building the client
	// config from the loaded file, rather than through a deferred loader,
	// also rules out client-go's fallback to the in-cluster config.
	rules := &clientcmd.ClientConfigLoadingRules{Precedence: c.KubeconfigPaths}
	raw, err := rules.Load()
	if err != nil {
		return nil, fmt.Errorf("%w: reading kubeconfig: %v", ErrMisconfigured, err)
	}
	kctx, ok := raw.Contexts[c.Context]
	if !ok {
		return nil, fmt.Errorf("%w: no context %q in %q", ErrContextMissing, c.Context, c.KubeconfigPaths)
	}
	cluster, ok := raw.Clusters[kctx.Cluster]
	if !ok || cluster.Server == "" {
		return nil, fmt.Errorf("%w: context %q names cluster %q, which has no server in the kubeconfig", ErrMisconfigured, c.Context, kctx.Cluster)
	}
	user := raw.AuthInfos[kctx.AuthInfo]
	if kctx.AuthInfo != "" && user == nil {
		return nil, fmt.Errorf("%w: context %q names user %q, who is not in the kubeconfig", ErrMisconfigured, c.Context, kctx.AuthInfo)
	}
	if user != nil && user.AuthProvider != nil {
		return nil, fmt.Errorf("%w: user %q uses the %q auth provider, which fleet does not support; use an exec credential plugin (for OIDC, kubelogin)", ErrMisconfigured, kctx.AuthInfo, user.AuthProvider.Name)
	}

	cfg, err := clientcmd.NewNonInteractiveClientConfig(*raw, c.Context, &clientcmd.ConfigOverrides{}, rules).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMisconfigured, err)
	}
	conn := &Connection{
		ClusterID: c.ID,
		Namespace: c.Namespace,
		Context:   c.Context,
		Paths:     append([]string(nil), c.KubeconfigPaths...),
		Server:    cfg.Host,
		Auth:      authKind(cfg),
	}
	if cfg.ExecProvider != nil {
		plugin, err := newPlugin(cfg, c, environ)
		if err != nil {
			return nil, err
		}
		conn.Plugin = plugin
		cfg.ExecProvider = plugin.execConfig()
	}
	cfg.QPS, cfg.Burst = QPS, Burst
	cfg.UserAgent = userAgent()
	conn.config = cfg
	return conn, nil
}

// RESTConfig returns a copy of the pinned client configuration.
// rest.CopyConfig leaves the copy sharing its source's exec provider, and
// replaces that provider's cluster extension as it copies, so copies are
// serialized and each gets an exec provider of its own.
func (c *Connection) RESTConfig() *rest.Config {
	c.configMu.Lock()
	defer c.configMu.Unlock()
	cfg := rest.CopyConfig(c.config)
	if cfg.ExecProvider != nil {
		cfg.ExecProvider = cfg.ExecProvider.DeepCopy()
	}
	return cfg
}

// Clientset returns the connection's typed client, built on first use.
func (c *Connection) Clientset() (kubernetes.Interface, error) {
	c.clientOnce.Do(func() {
		c.clientset, c.clientErr = kubernetes.NewForConfig(c.RESTConfig())
	})
	return c.clientset, c.clientErr
}

func authKind(cfg *rest.Config) AuthKind {
	switch {
	case cfg.ExecProvider != nil:
		return AuthExecPlugin
	case len(cfg.CertData) > 0 || cfg.CertFile != "":
		return AuthClientCert
	case cfg.BearerTokenFile != "":
		return AuthTokenFile
	case cfg.BearerToken != "":
		return AuthToken
	case cfg.Username != "":
		return AuthBasic
	}
	return AuthNone
}

func anyExists(paths []string) bool {
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

func userAgent() string {
	v := version.Version
	if v == "" {
		v = "dev"
	}
	return "fleet/" + v + " (" + runtime.GOOS + "/" + runtime.GOARCH + ")"
}

// DefaultKubeconfigPaths are the kubeconfig files kubectl would read with
// this environment: $KUBECONFIG's entries, else ~/.kube/config. A connection
// pins the paths its context came from when it is added.
func DefaultKubeconfigPaths(environ []string) []string {
	env := envMap(environ)
	if list := env["KUBECONFIG"]; list != "" {
		var paths []string
		seen := map[string]bool{}
		for _, p := range splitList(list) {
			if !seen[p] {
				seen[p] = true
				paths = append(paths, p)
			}
		}
		if len(paths) > 0 {
			return paths
		}
	}
	return []string{clientcmd.RecommendedHomeFile}
}

// KubeContext is one context of a kubeconfig, as the daemon sees it.
type KubeContext struct {
	Name      string
	Cluster   string
	Server    string
	Namespace string
	User      string
	// Auth is how the context authenticates; Plugin, for an exec plugin, is
	// its command as the kubeconfig writes it.
	Auth   AuthKind
	Plugin string
	// Source is the file that defines the context.
	Source string
}

// ListContexts lists the contexts of the kubeconfig files paths (in
// precedence order: a context defined twice comes from the first file).
// Missing files are skipped.
func ListContexts(paths []string) ([]KubeContext, error) {
	rules := &clientcmd.ClientConfigLoadingRules{Precedence: paths}
	merged, err := rules.Load()
	if err != nil {
		return nil, fmt.Errorf("%w: reading kubeconfig: %v", ErrMisconfigured, err)
	}
	var out []KubeContext
	seen := map[string]bool{}
	for _, path := range paths {
		file, err := clientcmd.LoadFromFile(path)
		if err != nil {
			continue // missing (rules.Load reported anything worse)
		}
		for _, name := range sortedKeys(file.Contexts) {
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, describeContext(merged, name, path))
		}
	}
	return out, nil
}

func describeContext(merged *clientcmdapi.Config, name, source string) KubeContext {
	kctx := merged.Contexts[name]
	kc := KubeContext{Name: name, Cluster: kctx.Cluster, Namespace: kctx.Namespace, User: kctx.AuthInfo, Source: source, Auth: AuthNone}
	if cluster := merged.Clusters[kctx.Cluster]; cluster != nil {
		kc.Server = cluster.Server
	}
	if user := merged.AuthInfos[kctx.AuthInfo]; user != nil {
		switch {
		case user.Exec != nil:
			kc.Auth, kc.Plugin = AuthExecPlugin, user.Exec.Command
		case len(user.ClientCertificateData) > 0 || user.ClientCertificate != "":
			kc.Auth = AuthClientCert
		case user.TokenFile != "":
			kc.Auth = AuthTokenFile
		case user.Token != "":
			kc.Auth = AuthToken
		case user.Username != "":
			kc.Auth = AuthBasic
		case user.AuthProvider != nil:
			kc.Auth = AuthKind("auth-provider:" + user.AuthProvider.Name)
		}
	}
	return kc
}
