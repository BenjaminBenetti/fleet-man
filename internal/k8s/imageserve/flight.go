package imageserve

import (
	"context"
	"errors"
	"sync"
)

// flight runs one import per key at a time and hands its result to every
// caller that asked for the same key meanwhile: N instances created from one
// image at once export it once.
type flight[T any] struct {
	mu    sync.Mutex
	calls map[string]*flightCall[T]
}

type flightCall[T any] struct {
	done chan struct{}
	val  T
	err  error
}

// do runs fn for key unless a run is already in flight, in which case it
// waits for that run's result. A run that ended because its own caller's
// context did says nothing about the work, so a waiter whose context is
// still live runs it again.
func (f *flight[T]) do(ctx context.Context, key string, fn func(context.Context) (T, error)) (T, error) {
	for {
		f.mu.Lock()
		if f.calls == nil {
			f.calls = map[string]*flightCall[T]{}
		}
		if c, ok := f.calls[key]; ok {
			f.mu.Unlock()
			select {
			case <-c.done:
			case <-ctx.Done():
				var zero T
				return zero, ctx.Err()
			}
			if isContextErr(c.err) && ctx.Err() == nil {
				continue
			}
			return c.val, c.err
		}
		c := &flightCall[T]{done: make(chan struct{})}
		f.calls[key] = c
		f.mu.Unlock()

		c.val, c.err = fn(ctx)
		f.mu.Lock()
		delete(f.calls, key)
		f.mu.Unlock()
		close(c.done)
		return c.val, c.err
	}
}

func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
