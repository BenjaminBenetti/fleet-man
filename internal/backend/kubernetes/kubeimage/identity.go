package kubeimage

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

// identity.go is what fleet knows of a delivered image's content. An image's
// config digest is its content identity: the config lists every layer's
// diff_id (the digest of its uncompressed files), so no other image has it.
// The digest a pod pins is not: docker's containerd image store wraps a
// build in an OCI index beside its provenance attestation, whose digest
// changes with every build of the same content, and a node that pulled the
// content under one digest reports that one for a later pull of it under
// another. So a delivery reads back, by digest, which platform manifest and
// config it delivered (Entry.Manifest, Entry.Config), and the index keeps,
// per config, every digest fleet delivered it under (Index.Configs): what a
// pod's verification accepts besides the digests its instance recorded.

// maxConfigDigests bounds the digests the index keeps for one config: each
// build with attestations adds one.
const maxConfigDigests = 32

// identityTimeout bounds reading a delivered image's identity back from its
// registry.
const identityTimeout = 20 * time.Second

// identity returns what the image the registry holds at repoPath@digest is
// on platform ("" is the daemon host's): its manifest digest there (digest
// itself, unless digest is an index's) and that manifest's config digest.
// Every read is by digest and checked against it, so the answer is bound to
// digest's content.
func (c *registryClient) identity(ctx context.Context, repoPath, digest, platform string) (manifest, config string, err error) {
	body, mediaType, _, err := c.getManifest(ctx, repoPath, digest)
	if err != nil {
		return "", "", err
	}
	manifest = digest
	if isIndex(mediaType) {
		if manifest, err = pickPlatform(body, platform); err != nil {
			return "", "", err
		}
		if body, mediaType, _, err = c.getManifest(ctx, repoPath, manifest); err != nil {
			return "", "", err
		}
		if isIndex(mediaType) {
			return "", "", fmt.Errorf("the image for %s is another index", platformOr(platform))
		}
	}
	var m struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := json.Unmarshal(body, &m); err != nil || !digestPattern.MatchString(m.Config.Digest) {
		return "", "", fmt.Errorf("manifest %s names no image config", manifest)
	}
	return manifest, m.Config.Digest, nil
}

// identify fills in e's Manifest and Config for platform from the registry c
// reaches (e.Digest in repoPath). A registry that cannot say leaves them
// unset: the image is then verified by its digests alone, as before fleet
// recorded them.
func identify(ctx context.Context, c *registryClient, repoPath, platform string, e *Entry) {
	if c == nil || !digestPattern.MatchString(e.Digest) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, identityTimeout)
	defer cancel()
	manifest, config, err := c.identity(ctx, repoPath, e.Digest, platform)
	if err != nil {
		flog.Warn("kubernetes image: could not read the delivered image's config back from its registry; its pods are verified by its digest alone", "ref", e.Ref, "err", err)
		return
	}
	e.Manifest, e.Config = manifest, config
}

// noteConfig records under e's config the digests e was delivered under.
func (ix *Index) noteConfig(e *Entry) {
	if !digestPattern.MatchString(e.Config) {
		return
	}
	if ix.Configs == nil {
		ix.Configs = map[string][]string{}
	}
	ds := ix.Configs[e.Config]
	for _, d := range []string{e.Digest, e.Manifest} {
		if digestPattern.MatchString(d) && !slices.Contains(ds, d) {
			ds = append(ds, d)
		}
	}
	if len(ds) > maxConfigDigests {
		ds = ds[len(ds)-maxConfigDigests:]
	}
	ix.Configs[e.Config] = ds
}

// pruneConfigs drops the digests of every config no entry has any more: no
// instance holds that content.
func (ix *Index) pruneConfigs() {
	for c := range ix.Configs {
		if !slices.ContainsFunc(ix.Images, func(e Entry) bool { return e.Config == c }) {
			delete(ix.Configs, c)
		}
	}
	if len(ix.Configs) == 0 {
		ix.Configs = nil
	}
}

// SameContent returns what the connection's index knows of the content of an
// image with config digest config, or delivered under one of digests (bare
// sha256:<hex>): the config digests of that content, config first when it
// is one, and every digest fleet delivered it under. Only fleet's own
// deliveries are recorded, each config read back from the registry by
// digest; a read of the index file is all it does.
func (m *Manager) SameContent(clusterID, config string, digests []string) (configs, delivered []string, err error) {
	if err := m.checkIndex(clusterID); err != nil {
		return nil, nil, err
	}
	ix, err := m.files.read(m.IndexPath(clusterID))
	if err != nil {
		return nil, nil, err
	}
	if digestPattern.MatchString(config) {
		configs = append(configs, config)
	}
	keys := make([]string, 0, len(ix.Configs))
	for c := range ix.Configs {
		keys = append(keys, c)
	}
	slices.Sort(keys)
	for _, c := range keys {
		if !slices.Contains(configs, c) && slices.ContainsFunc(ix.Configs[c], func(d string) bool { return slices.Contains(digests, d) }) {
			configs = append(configs, c)
		}
	}
	for _, c := range configs {
		delivered = append(delivered, ix.Configs[c]...)
	}
	return configs, delivered, nil
}
