//go:build !nok8s

package preflight

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/kubernetes"
)

// serverVersion is the API server's version. Discovery's ServerVersion
// takes no context, so it goes through the discovery REST client, which
// does; a client without one (the fake) falls back.
func serverVersion(ctx context.Context, cs kubernetes.Interface) (string, error) {
	d := cs.Discovery()
	if rc := d.RESTClient(); rc != nil {
		body, err := rc.Get().AbsPath("/version").Do(ctx).Raw()
		if err != nil {
			return "", err
		}
		var info version.Info
		if err := json.Unmarshal(body, &info); err != nil {
			return "", fmt.Errorf("decoding the server version: %w", err)
		}
		return info.GitVersion, nil
	}
	info, err := d.ServerVersion()
	if err != nil {
		return "", err
	}
	return info.GitVersion, nil
}

// RawGetter returns a Target.Raw that reads absolute API paths through cs's
// discovery REST client (nil when it has none).
func RawGetter(cs kubernetes.Interface) func(ctx context.Context, path string) ([]byte, error) {
	rc := cs.Discovery().RESTClient()
	if rc == nil {
		return nil
	}
	return func(ctx context.Context, path string) ([]byte, error) {
		return rc.Get().AbsPath(path).Do(ctx).Raw()
	}
}
