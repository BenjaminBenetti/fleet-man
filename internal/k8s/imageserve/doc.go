// Package imageserve is fleetd's pull-only image registry for gateway
// delivery: clusters without a registry pull the images fleetd built for
// them through the Fleet gateway, at <host>/<rid>/devcontainer@<digest>
// (node containerd → gateway /v2/<rid>/… → the dedicated registry tunnel
// connection → Handler).
//
//   - Store is the cache (~/.fleet/kube/images): `docker save` output turned
//     into content-addressed manifests and blobs. Compressed layers (docker's
//     containerd image store) are kept byte for byte, so the node-platform
//     manifest keeps its digest; uncompressed ones (the classic overlay2
//     store) are gzip-compressed once, keyed by diffID. The pod pins the
//     digest of that one node-platform manifest, never the image ID (on the
//     containerd store an index of platforms fleetd does not hold). Blobs are
//     reference-counted through the manifests the connections serve and stay
//     while referenced, even after the host's docker prunes the image.
//   - Handler serves manifests and blobs by digest only, GET and HEAD, to the
//     connection a request's Basic credential names (its id and its
//     Image.GatewayCredential), with explicit Content-Type, Content-Length
//     and Docker-Content-Digest, Range resumes, a Basic challenge without a
//     credential, and never a redirect.
//   - Uplink paces uploads to a share of the measured upload link (80% by
//     default) so the main tunnel's keepalive and latency survive a pull.
//   - Gateway implements kubeimage.Gateway: Deliver prepares the image,
//     serves it to the connection, keeps the connection's credential in one
//     long-lived managed pull Secret (rotated in place, never recreated) and
//     opens the registry connection; Remove is kubeimage's GC; a new registry
//     id or host re-points every gateway-delivered StatefulSet at once
//     (RegistryChanged); ClassifyPull tells a pull that failed because the
//     daemon was offline (the gateway's 502, or its 404 once it forgot the
//     session) from a digest that is really gone.
//
// The package has no client-go dependency: the cluster backend reaches the
// namespace through kubeimage.Secrets and Connections.
package imageserve
