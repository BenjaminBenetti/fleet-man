package backend

// ConcurrentStats fetches stats for each id concurrently using fetch and
// collects the successful results into a map keyed by id. Each id is
// fetched in its own goroutine; a buffered channel sized to len(ids)
// ensures senders never block. Results whose fetch returned a non-nil
// error or a nil stats value are skipped. Returns (nil, nil) when ids is
// empty.
func ConcurrentStats(ids []string, fetch func(id string) (*ContainerStats, error)) (map[string]*ContainerStats, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	type statsResult struct {
		id    string
		stats *ContainerStats
		err   error
	}

	ch := make(chan statsResult, len(ids))
	for _, id := range ids {
		go func(id string) {
			stats, err := fetch(id)
			ch <- statsResult{id, stats, err}
		}(id)
	}

	result := make(map[string]*ContainerStats)
	for range ids {
		received := <-ch
		if received.err == nil && received.stats != nil {
			result[received.id] = received.stats
		}
	}

	return result, nil
}
