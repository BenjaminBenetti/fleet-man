//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kclient "k8s.io/client-go/kubernetes"

	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

// scope.go acts on this daemon's objects in one connection's namespace by
// what fleet knows of them, never by guesswork: the objects of an instance
// key (a teardown), and the objects a job journal recorded (an undo, or
// known garbage). Every deletion is preceded by the owner check, so a
// restored copy of ~/.fleet on another machine never deletes what the
// original still uses.

// ErrOwnerConflict is a deletion refused because this daemon does not hold
// the connection's owner Lease.
var ErrOwnerConflict = errors.New("owner_conflict: this daemon does not hold the connection's owner Lease")

// Scope is one connection's namespace as this daemon's objects in it are
// reached: the client, the namespace, the owner and connection labels that
// make an object this daemon's on this connection, and the owner check.
type Scope struct {
	Client    kclient.Interface
	Namespace string
	Owner     string
	Cluster   string
	// Gate runs before every deletion and must return nil: the daemon holds
	// the connection's owner Lease (ErrOwnerConflict, or why it cannot
	// tell). Nil allows every deletion.
	Gate func(ctx context.Context) error
	// Logger is fleet.log (flog) by default.
	Logger *slog.Logger
}

func (s Scope) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return flog.L()
}

func (s Scope) gate(ctx context.Context) error {
	if s.Gate == nil {
		return nil
	}
	return s.Gate(ctx)
}

// keySelector selects the objects of instance key on the connection.
func (s Scope) keySelector(key string) string {
	return names.Selector(s.Owner, s.Cluster) + "," + names.LabelKey + "=" + key
}

// Teardown is what a deletion by key or by journal did.
type Teardown struct {
	// Deleted are the objects whose deletion the API server accepted
	// ("kind/name"); Remaining those still there, being deleted or not.
	Deleted   []string
	Remaining []string
}

// ListByKey lists every object of instance key on the connection, the
// ones being deleted included.
func (s Scope) ListByKey(ctx context.Context, key string) ([]objectMeta, error) {
	if !names.ValidHash(key) {
		return nil, fmt.Errorf("%q is not an instance key", key)
	}
	sel := s.keySelector(key)
	var out []objectMeta
	for _, kind := range teardownKinds {
		k, _ := kindOf(kind)
		objs, err := k.list(ctx, s.Client, s.Namespace, sel)
		if err != nil {
			return nil, fmt.Errorf("listing %s of instance %s: %w", kind, key, err)
		}
		out = append(out, objs...)
	}
	return out, nil
}

// DeleteByKey deletes every object of instance key on the connection (a
// label delete by key and connection, in teardownKinds' order, foreground,
// so a StatefulSet's or Job's pods go before it does). Objects already being
// deleted are left to finish. The owner check runs before each deletion; a
// refusal stops the teardown with what it did so far.
func (s Scope) DeleteByKey(ctx context.Context, key string) (Teardown, error) {
	objs, err := s.ListByKey(ctx, key)
	if err != nil {
		return Teardown{}, err
	}
	return s.deleteAll(ctx, objs)
}

// ListSelected lists the objects of kinds (teardownKinds' names) on the
// connection that selector matches besides the connection's own labels, the
// ones being deleted included, as "kind/name" in kinds' order.
func (s Scope) ListSelected(ctx context.Context, selector string, kinds []string) ([]string, error) {
	objs, err := s.listSelected(ctx, selector, kinds)
	if err != nil {
		return nil, err
	}
	refs := make([]string, 0, len(objs))
	for _, o := range objs {
		refs = append(refs, objectRef(o.Kind, o.Name))
	}
	return refs, nil
}

// DeleteSelected deletes the objects of kinds on the connection that
// selector matches besides the connection's own labels (in kinds' order,
// foreground, under the owner check, as DeleteByKey does an instance's).
func (s Scope) DeleteSelected(ctx context.Context, selector string, kinds []string) (Teardown, error) {
	objs, err := s.listSelected(ctx, selector, kinds)
	if err != nil {
		return Teardown{}, err
	}
	return s.deleteAll(ctx, objs)
}

func (s Scope) listSelected(ctx context.Context, selector string, kinds []string) ([]objectMeta, error) {
	sel := names.Selector(s.Owner, s.Cluster)
	if selector != "" {
		sel += "," + selector
	}
	var out []objectMeta
	for _, kind := range kinds {
		k, ok := kindOf(kind)
		if !ok {
			return nil, fmt.Errorf("listing %q objects: fleet does not know the kind", kind)
		}
		objs, err := k.list(ctx, s.Client, s.Namespace, sel)
		if err != nil {
			return nil, fmt.Errorf("listing %s (%s): %w", kind, selector, err)
		}
		out = append(out, objs...)
	}
	return out, nil
}

func (s Scope) deleteAll(ctx context.Context, objs []objectMeta) (Teardown, error) {
	var t Teardown
	var errs []error
	propagation := metav1.DeletePropagationForeground
	for _, o := range objs {
		ref := objectRef(o.Kind, o.Name)
		if o.Deleting {
			t.Remaining = append(t.Remaining, ref)
			continue
		}
		if err := s.gate(ctx); err != nil {
			return t, err
		}
		k, _ := kindOf(o.Kind)
		opts := metav1.DeleteOptions{PropagationPolicy: &propagation}
		if o.UID != "" {
			uid := types.UID(o.UID)
			opts.Preconditions = &metav1.Preconditions{UID: &uid}
		}
		err := k.delete(ctx, s.Client, s.Namespace, o.Name, opts)
		switch {
		case err == nil:
			t.Deleted = append(t.Deleted, ref)
			s.logger().Info("kube: deleted", "cluster", s.Cluster, "namespace", s.Namespace, "object", ref)
		case gone(err):
		default:
			errs = append(errs, fmt.Errorf("deleting %s: %w", ref, err))
			t.Remaining = append(t.Remaining, ref)
		}
	}
	return t, joinErrors(errs)
}

// JournalCheck is what the live objects say of a journal.
type JournalCheck struct {
	// Current: the anchor still carries the journal's mark under the UID it
	// recorded, and no recorded object has another UID now. Only a current
	// journal may be acted on (rolled back, or its record failed by it).
	Current bool
	// Why the journal is not current, for fleet.log.
	Why string
	// Live are the recorded objects that are the job's: they exist with
	// the UID the journal recorded, or, for one whose write never returned,
	// carry the instance's key. Keyed "kind/name".
	Live map[string]objectMeta
}

// CheckJournal reads the objects j recorded and judges whether j still
// describes them (see JournalCheck). A journal whose anchor was never
// written (the job died before its first write returned) is current unless
// one of its objects says otherwise.
func (s Scope) CheckJournal(ctx context.Context, j Journal) (JournalCheck, error) {
	c := JournalCheck{Live: map[string]objectMeta{}, Current: true}
	stale := func(why string) {
		if c.Current {
			c.Current, c.Why = false, why
		}
	}
	for _, o := range j.Objects {
		k, ok := kindOf(o.Kind)
		if !ok {
			continue
		}
		ref := objectRef(o.Kind, o.Name)
		live, err := k.get(ctx, s.Client, s.Namespace, o.Name)
		switch {
		case gone(err):
			if o.Anchor && o.UID != "" {
				stale(ref + " is gone")
			}
			continue
		case err != nil:
			return JournalCheck{}, fmt.Errorf("reading %s: %w", ref, err)
		case live.Labels[names.LabelKey] != j.Key || live.Labels[names.LabelCluster] != s.Cluster || live.Labels[names.LabelOwner] != s.Owner:
			// Not this instance's object at all: never touched.
			stale(ref + " belongs to something else now")
			continue
		case o.UID != "" && live.UID != o.UID:
			stale(ref + " was made again since the journal (another UID)")
			continue
		}
		if o.Anchor && !j.Current(live.Annotations[names.AnnotationOp]) {
			stale(fmt.Sprintf("%s carries %s=%q, not %q: the job finished", ref, names.AnnotationOp, live.Annotations[names.AnnotationOp], j.Mark()))
		}
		c.Live[ref] = live
	}
	return c, nil
}

// DeleteJournalObjects deletes the objects of j that check found live and
// which picks (all of them when nil). An object is deleted only under the
// UID the journal recorded, or, for one whose write never returned, the one
// found carrying the instance's key.
func (s Scope) DeleteJournalObjects(ctx context.Context, j Journal, check JournalCheck, which func(JournalObject) bool) (Teardown, error) {
	var objs []objectMeta
	for _, kind := range teardownKinds {
		for _, o := range j.Objects {
			if o.Kind != kind || (which != nil && !which(o)) {
				continue
			}
			if live, ok := check.Live[objectRef(o.Kind, o.Name)]; ok {
				objs = append(objs, live)
			}
		}
	}
	return s.deleteAll(ctx, objs)
}

// MirrorStep writes j's mark on its anchor, once the anchor exists.
func (s Scope) MirrorStep(ctx context.Context, j Journal) error {
	a, ok := j.Anchor()
	if !ok || a.UID == "" {
		return nil
	}
	k, ok := kindOf(a.Kind)
	if !ok {
		return fmt.Errorf("anchor kind %q", a.Kind)
	}
	mark := j.Mark()
	if err := k.patch(ctx, s.Client, s.Namespace, a.Name, annotationPatch(names.AnnotationOp, &mark)); err != nil {
		return fmt.Errorf("marking %s %s: %w", objectRef(a.Kind, a.Name), mark, err)
	}
	return nil
}

// ClearMark removes fleet.dev/op from the objects j wrote that are still
// there: the job ended.
func (s Scope) ClearMark(ctx context.Context, j Journal) error {
	var errs []error
	for _, o := range j.Objects {
		k, ok := kindOf(o.Kind)
		if !ok || o.UID == "" {
			continue
		}
		err := k.patch(ctx, s.Client, s.Namespace, o.Name, annotationPatch(names.AnnotationOp, nil))
		if err != nil && !gone(err) {
			errs = append(errs, fmt.Errorf("clearing %s on %s: %w", names.AnnotationOp, objectRef(o.Kind, o.Name), err))
		}
	}
	return joinErrors(errs)
}
