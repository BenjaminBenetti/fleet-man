//go:build !nok8s

package kubernetes

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kclient "k8s.io/client-go/kubernetes"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

// PullSecrets keeps a connection's managed pull Secrets in its namespace
// (kubeimage.Secrets): applied server-side under fleet's field manager, so a
// rotated credential rewrites the Secret in place.
type PullSecrets struct {
	Client    kclient.Interface
	Namespace string
}

var _ kubeimage.Secrets = PullSecrets{}

// ApplyPullSecret creates or rewrites ps.
func (s PullSecrets) ApplyPullSecret(ctx context.Context, ps kubeimage.PullSecret) error {
	b, err := ps.Manifest()
	if err != nil {
		return err
	}
	force := true
	_, err = s.Client.CoreV1().Secrets(s.Namespace).Patch(ctx, ps.Name, types.ApplyPatchType, b, metav1.PatchOptions{FieldManager: DefaultFieldManager, Force: &force})
	return err
}

// DeletePullSecret deletes the Secret name; one that is not there is fine.
func (s PullSecrets) DeletePullSecret(ctx context.Context, name string) error {
	err := s.Client.CoreV1().Secrets(s.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// PullSecretLabels are the labels of a connection's managed pull Secrets:
// the connection's (never an instance's key, since they outlive every
// instance) and role pull-secret.
func PullSecretLabels(owner, clusterID string) map[string]string {
	l := names.Base(owner, clusterID)
	l[names.LabelRole] = string(names.RolePullSecret)
	return l
}
