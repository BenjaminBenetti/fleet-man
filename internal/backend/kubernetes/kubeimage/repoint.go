package kubeimage

// RepointGateway rewrites the references of the connection's
// gateway-delivered images: ref returns an entry's new reference (the
// gateway's registry id or host changed). It returns each changed reference,
// old → new, for the caller to patch the StatefulSets that name them; Lookup
// then hands out the new ones.
func (m *Manager) RepointGateway(clusterID string, ref func(Entry) string) (map[string]string, error) {
	if err := m.checkIndex(clusterID); err != nil {
		return nil, err
	}
	changed := map[string]string{}
	err := m.files.update(m.IndexPath(clusterID), func(ix *Index) error {
		for i := range ix.Images {
			e := &ix.Images[i]
			if e.Strategy != StrategyGateway {
				continue
			}
			if next := ref(*e); next != "" && next != e.Ref {
				changed[e.Ref] = next
				e.Ref = next
			}
		}
		return nil
	})
	return changed, err
}
