// Package kubeexec is fleet's client-go plumbing: the one place a KubeCluster
// record becomes a client configuration.
//
// Load pins a connection to exactly what its record names: the kubeconfig
// files on the daemon host and the context, never $KUBECONFIG, the current
// context or an in-cluster fallback, so a later `kubectl config use-context`
// or a daemon started from another shell cannot redirect fleet. A context
// that authenticates through an exec credential plugin runs it by an absolute
// path resolved through the record's extra_path, never interactively, and
// with an environment limited to the process basics, the kubeconfig's own env
// entries and the record's plugin_env allowlist (fleet.ValidatePluginEnv):
// the daemon's other variables reach a plugin set empty, never with their
// values. Credential reads the expiry of the credential the connection
// authenticates with, running the plugin exactly as client-go does.
//
// Clients are built on first use, never at package init, so linking the
// package costs fleet commands that never touch a cluster only client-go's own
// init. Everything here needs client-go and is left out of the -tags nok8s
// pod build; this file alone builds there, so the package always exists.
//
// The exec, port-forward and log streams, and the daemon-issued credentials
// of direct mode, are later additions to this package.
package kubeexec
