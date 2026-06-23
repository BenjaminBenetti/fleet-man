package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// trigger_logs.go records and serves a per-trigger history of automation
// firings, for debuggability. Each time a trigger fires (the schedule and
// webhook paths both funnel through fireTriggerAgents), the daemon writes the
// firing's event payload — the webhook request body, or the schedule fire-time — to
//
//	~/.fleet/logs/<fleet>/trigger/<trigger>/event-<UTC timestamp>.log
//
// keeping the most recent triggerEventLogKeep files. This is the SAME payload
// that gets copied into each spawned agent's instance (see launchAutomation-
// Command / writeAutomationEventFile), captured durably on the host so a firing
// can be inspected after the instance is reaped — which is the whole point: it
// makes the trigger system debuggable. The TUI's 'L' pager, the `fleet trigger
// logs` CLI, and the fleet_trigger_logs MCP tool all read it back through the
// TriggerLogs RPC, so it works the same whether the daemon is local or remote.

// triggerEventLogKeep bounds how many event logs are retained per trigger. Older
// ones are pruned as new firings arrive, so a busy trigger can't grow the logs
// tree without bound.
const triggerEventLogKeep = 100

// logTriggerEvent records one trigger firing to the trigger's on-host event log
// directory, then prunes to the newest triggerEventLogKeep files. It is a
// package-var seam (overridden in tests) and strictly best-effort: a logging
// failure is warned and never blocks the firing — the log is a debugging aid,
// not part of the automation's critical path.
var logTriggerEvent = func(fleetName string, ev *triggerEvent) {
	dir := state.TriggerLogsDir(fleetName, ev.triggerName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		flog.Warn("automation: trigger log mkdir failed", "fleet", fleetName, "trigger", ev.triggerName, "err", err)
		return
	}
	path := uniqueTriggerLogPath(dir, ev.firedAt)
	if err := os.WriteFile(path, triggerEventLogContent(ev), 0o644); err != nil {
		flog.Warn("automation: trigger log write failed", "fleet", fleetName, "trigger", ev.triggerName, "err", err)
		return
	}
	pruneTriggerLogs(dir, triggerEventLogKeep)
}

// triggerEventLogContent renders one event log: a short header naming the firing
// followed by its payload, so a concatenated dump is self-describing. The
// payload mirrors exactly what the spawned agent received (triggerEvent.payload).
func triggerEventLogContent(ev *triggerEvent) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# trigger: %s\n", ev.triggerName)
	fmt.Fprintf(&b, "# type:    %s\n", ev.kind)
	if ev.kind == fleet.TriggerWebhook && ev.webhookName != "" {
		fmt.Fprintf(&b, "# webhook: %s\n", ev.webhookName)
	}
	fmt.Fprintf(&b, "# fired:   %s\n", ev.firedAt.UTC().Format(time.RFC3339))
	b.WriteByte('\n')
	payload := ev.payload()
	b.Write(payload)
	if len(payload) == 0 || payload[len(payload)-1] != '\n' {
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// uniqueTriggerLogPath returns event-<UTC timestamp>.log in dir, suffixing -N
// when a log with that second-granular name already exists (two webhook events
// can fire within the same second), so no firing overwrites another.
func uniqueTriggerLogPath(dir string, firedAt time.Time) string {
	base := "event-" + firedAt.UTC().Format("20060102T150405Z")
	path := filepath.Join(dir, base+".log")
	for i := 2; i < 100000; i++ {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			return path
		}
		path = filepath.Join(dir, fmt.Sprintf("%s-%d.log", base, i))
	}
	return path
}

// pruneTriggerLogs keeps only the newest keep event logs in dir, deleting the
// rest. Event filenames embed a sortable UTC timestamp, so lexical order is
// (near enough) chronological order.
func pruneTriggerLogs(dir string, keep int) {
	logs := eventLogNames(dir)
	if len(logs) <= keep {
		return
	}
	for _, name := range logs[:len(logs)-keep] {
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// eventLogNames returns the event-*.log filenames in dir, sorted ascending
// (oldest first). A missing or unreadable directory yields nil.
func eventLogNames(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "event-") && strings.HasSuffix(e.Name(), ".log") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// readTriggerLogs concatenates a trigger's event logs (oldest first) for
// viewing, each preceded by a separator naming its file. It returns the text and
// the number of logs included. A trigger that has never fired (no directory)
// yields "" and 0 — not an error — so callers can report "no events" cleanly.
func readTriggerLogs(fleetName, triggerName string) (string, int, error) {
	dir := state.TriggerLogsDir(fleetName, triggerName)
	names := eventLogNames(dir)
	var b strings.Builder
	included := 0
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue // a log removed mid-read just drops out of the dump
		}
		fmt.Fprintf(&b, "===== %s =====\n", name)
		b.Write(data)
		if len(data) == 0 || data[len(data)-1] != '\n' {
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
		included++
	}
	return b.String(), included, nil
}

// TriggerLogs serves an automation trigger's recorded event logs. It reads the
// daemon-owned log files, so it works identically for a local or remote daemon.
func (s *service) TriggerLogs(_ context.Context, req *fleetgrpc.TriggerLogsRequest) (*fleetgrpc.TriggerLogsReply, error) {
	if req.GetFleet() == "" || req.GetTrigger() == "" {
		return nil, status.Error(codes.InvalidArgument, "fleet and trigger are required")
	}
	logs, count, err := readTriggerLogs(req.GetFleet(), req.GetTrigger())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read trigger logs: %v", err)
	}
	return &fleetgrpc.TriggerLogsReply{Logs: logs, Count: int32(count)}, nil
}
