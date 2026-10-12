// Package netprobe answers, from inside an instance, which of a few URLs is
// reachable right now: `fleet net-probe`. An instance on a cluster asks it
// before apt uses the fleet's deb cache (apt's Acquire::http::Proxy-Auto-Detect
// runs a command that prints the proxy for a URL, or DIRECT for none), since
// the cache's server there comes up on the cluster's time and is not revived
// with the instance; a cache that does not answer means apt goes direct
// instead of failing.
package netprobe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// Direct is what First answers when no URL is reachable: apt's word for "no
// proxy".
const Direct = "DIRECT"

// DefaultTimeout bounds one try of the connection to each URL. A cache in
// the instance's own cluster answers in milliseconds.
const DefaultTimeout = 2 * time.Second

// DefaultWait is how long First keeps trying while no URL answers. A busy
// node or cluster DNS drops a packet now and then, which costs one try its
// whole timeout (the lookup of the cache's Service name waits out the
// resolver's 5s, cut at the try's 2s): one try alone would send apt direct,
// and the instance would lose the fleet's cache for that run. apt asks once
// per host and run, so a cache that is down (a NetworkPolicy, a node gone,
// its Service deleted) costs this much once.
const DefaultWait = 8 * time.Second

// retryPause is the least time between the starts of two rounds of tries,
// so URLs that refuse at once are not hammered.
var retryPause = 500 * time.Millisecond

// dial is the TCP dial; a package var so tests stand in a refusal.
var dial = func(ctx context.Context, address string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", address)
}

// Address is the host:port a connection to rawURL goes to: its port, else its
// scheme's (http 80, https 443). A URL with no host, or a port fleet cannot
// guess, is an error.
func Address(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("url %q: %w", rawURL, err)
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("url %q has no host", rawURL)
	}
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return "", fmt.Errorf("url %q names no port", rawURL)
		}
	}
	return net.JoinHostPort(host, port), nil
}

// First returns the first of urls whose host accepts a TCP connection within
// timeout, exactly as given, or Direct when none does. It tries each URL in
// turn, and while none answered tries them all again, until wait has passed
// since it began (the first round always runs; wait 0 is one round): a URL
// one round missed under load is taken if the next one reaches it. Every URL
// is read before any is dialled, so a malformed one is an error whatever the
// others answer.
func First(ctx context.Context, urls []string, timeout, wait time.Duration) (string, error) {
	if len(urls) == 0 {
		return "", errors.New("no url to probe")
	}
	addrs := make([]string, len(urls))
	for i, u := range urls {
		a, err := Address(u)
		if err != nil {
			return "", err
		}
		addrs[i] = a
	}
	deadline := time.Now().Add(wait)
	for round := 0; ; round++ {
		started := time.Now()
		for i, a := range addrs {
			try := timeout
			if round > 0 {
				try = min(timeout, time.Until(deadline))
			}
			if try > 0 && reachable(ctx, a, try) {
				return urls[i], nil
			}
		}
		left := time.Until(deadline)
		if left <= 0 || ctx.Err() != nil {
			return Direct, nil
		}
		if pause := min(retryPause-time.Since(started), left); pause > 0 {
			t := time.NewTimer(pause)
			select {
			case <-ctx.Done():
				t.Stop()
				return Direct, nil
			case <-t.C:
			}
		}
	}
}

func reachable(ctx context.Context, address string, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c, err := dial(ctx, address)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}
