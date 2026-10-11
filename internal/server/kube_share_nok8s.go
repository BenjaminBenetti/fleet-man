//go:build nok8s

package server

import "context"

// maintainShares: no cluster shares in a -tags nok8s build.
func maintainShares(context.Context) {}

// shareKick: no cluster shares in a -tags nok8s build.
var shareKick = func(string, string) {}
