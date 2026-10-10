// Package kubeimage delivers the images capture-up built (or pulled) on the
// daemon host to a Kubernetes cluster, keeps each connection's image index,
// and collects images nothing uses any more.
//
// Strategies (Resolve picks one for built images):
//
//   - passthrough: an image the CLI did not build, which its registry holds,
//     is pulled from there by digest (repo@sha256), with the connection's
//     pull Secrets;
//   - node load: on a local cluster (minikube, kind, k3d, Docker Desktop;
//     localcluster.Detect) the image is tagged
//     fleet.invalid/<daemon8>/devcontainer:<id12> and loaded into the nodes
//     with the cluster's own tool, and pods pull it Never; a pod that finds
//     it gone (ErrImageNeverPull) gets it delivered again (NeedsRedelivery,
//     Redeliver);
//   - registry: pushed to <registry>/fleet/<daemon8>/devcontainer:<id12> and
//     pulled by the digest the push reported, with a managed pull Secret
//     holding the daemon host's login to that one registry and nothing else;
//   - gateway: served by fleetd through the Fleet gateway (the Gateway
//     seam, implemented by the image server).
//
// The index (~/.fleet/kube/<cluster id>/images.json) records every delivered
// image, what holds it (instances, seed caches) and what the daemon learnt
// about its registries. Deliveries of one image share one push or load
// (singleflight), and are serialized with its collection. GC deletes an
// image once nothing has held it for the connection's retention (72h by
// default): from the registry by digest, only inside the daemon's own path
// and only where the registry supports DELETE (CheckRegistry finds out; the
// cluster's health shows it), and from minikube's nodes; images the index
// does not know are never touched.
//
// The package has no client-go dependency: the cluster backend lists the
// nodes for localcluster.Detect and applies the managed pull Secret through
// the Secrets interface.
package kubeimage
