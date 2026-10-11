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

// DefaultTimeout bounds the connection to each URL. apt asks once per host
// and run, and a cache in the instance's own cluster answers in
// milliseconds; a dropped connection (a NetworkPolicy, a node gone) costs
// this much once.
const DefaultTimeout = 2 * time.Second

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
// timeout (each), exactly as given, or Direct when none does. Every URL is
// read before any is dialled, so a malformed one is an error whatever the
// others answer.
func First(ctx context.Context, urls []string, timeout time.Duration) (string, error) {
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
	for i, a := range addrs {
		if reachable(ctx, a, timeout) {
			return urls[i], nil
		}
	}
	return Direct, nil
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
