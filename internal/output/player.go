package output

import (
	"context"
	"io"
	"os/exec"
	"sync"
	"time"
)

type player struct {
	frames chan []byte
	done   chan struct{}
	cancel context.CancelFunc
	stdin  io.WriteCloser
	once   sync.Once
}

var startPlayer = func(parent context.Context, argv []string) (*player, error) {
	ctx, cancel := context.WithCancel(parent)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = time.Second
	in, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		in.Close()
		return nil, err
	}
	p := &player{frames: make(chan []byte, 5), done: make(chan struct{}), cancel: cancel, stdin: in}
	go func() {
		defer in.Close()
		for {
			select {
			case pcm := <-p.frames:
				if _, err := in.Write(pcm); err != nil {
					cancel()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { _ = cmd.Wait(); cancel(); close(p.done) }()
	return p, nil
}

func (p *player) write(pcm []byte) {
	select {
	case p.frames <- pcm:
		return
	default:
	}
	select {
	case <-p.frames:
	default:
	}
	select {
	case p.frames <- pcm:
	default:
	}
}

func (p *player) stop() {
	p.once.Do(func() { p.cancel(); p.stdin.Close(); <-p.done })
}
