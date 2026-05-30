package launchtui

import (
	"fmt"
	"io"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/appstart"
	"github.com/BenjaminBenetti/fleet-man/internal/control"
)

// ===========================================
// Headless entry points
// ===========================================
//
// Besides the interactive grid (Run), `fleet launch` supports two non-TUI
// modes that share the same configuration loading and activation logic:
//
//   - List writes the configured links and apps (so a user — or a script — can
//     see what names are available) and exits.
//   - LaunchByName opens a single named link or app exactly as clicking it in
//     the grid would, then exits.

// List writes the configured links and apps to w, grouped into a "Links"
// section and an "Apps" section, each entry showing the title used to launch it
// and its target. It needs no host connection. Backs `fleet launch list`.
func List(cfg Config, w io.Writer) error {
	fl, err := loadCustomizations(cfg.ConfigPath)
	if err != nil {
		return err
	}
	items := buildItems(fl)
	links := items[:linkCount(items)]
	apps := items[linkCount(items):]

	fmt.Fprintln(w, "Links:")
	writeItemList(w, links)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Apps:")
	writeItemList(w, apps)
	return nil
}

// writeItemList writes one indented line per item ("  <title> — <target>"), or
// a single "(none)" line when the section is empty.
func writeItemList(w io.Writer, items []item) {
	if len(items) == 0 {
		fmt.Fprintln(w, "  (none)")
		return
	}
	for _, it := range items {
		fmt.Fprintf(w, "  %s — %s\n", itemTitle(it), itemTarget(it))
	}
}

// itemTarget describes where an item points, for the listing: a link's URL, or
// an app's localhost:<port>.
func itemTarget(it item) string {
	if it.kind == kindLink {
		return it.url
	}
	return appstart.LocalURL(it.port)
}

// LaunchByName opens the link or app matching name exactly as activating it in
// the grid would: a link opens its URL on the host browser; an app is started
// on its port (if not already up) and then opened. Backs `fleet launch <name>`.
//
// name is matched case-insensitively against item titles (see resolveItem): a
// unique prefix is enough, so a few leading characters usually suffice. An
// ambiguous prefix lists the candidates so the user can type more; an unknown
// name points at `fleet launch list`.
//
// out receives human feedback: a spinner while an app boots (on a terminal) and
// an "Opened <name>" confirmation on success. Pass a CLI's stderr; a nil writer
// is treated as no output.
//
// It dials the host control socket directly (there is no UI to fall back to), so
// a missing host connection is a hard error rather than the grid's degraded
// mode.
func LaunchByName(cfg Config, name string, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}

	fl, err := loadCustomizations(cfg.ConfigPath)
	if err != nil {
		return err
	}
	items := buildItems(fl)
	idx, matches := resolveItem(items, name)
	if idx < 0 {
		if len(matches) == 0 {
			return fmt.Errorf("no link or app matching %q (run `fleet launch list` to see the options)", name)
		}
		return fmt.Errorf("%q matches multiple options: %s — type more of the name to pick one",
			name, strings.Join(matches, ", "))
	}

	client, err := control.Dial(cfg.socketPath())
	if err != nil {
		return fmt.Errorf("not connected to host fleet — is the host `fleet` TUI running? (%w)", err)
	}
	defer client.Close()

	it := items[idx]
	if it.kind == kindApp {
		// Starting an app can block (the command runs, then we wait for its port
		// to answer), so show a spinner for feedback while it boots.
		stop := startSpinner(out, fmt.Sprintf("Starting %s…", itemTitle(it)))
		err := openApp(client, it)
		stop()
		if err != nil {
			return err
		}
	} else if err := openLink(client, it); err != nil {
		return err
	}

	fmt.Fprintf(out, "Opened %s\n", itemTitle(it))
	return nil
}

// resolveItem matches name against item titles, case-insensitively, to support
// launching by a leading fragment of a name:
//
//   - An exact title match always wins (so "Logs" picks "Logs" even when
//     "LogsViewer" also exists).
//   - Otherwise a unique prefix match is used (so "graf" picks "Grafana").
//
// It returns the resolved index and a nil match list on success. When the name
// resolves to no item it returns (-1, nil); when it is ambiguous it returns
// (-1, titles) listing every candidate so the caller can ask the user to type
// more. The candidate titles are returned in item (links-then-apps) order.
func resolveItem(items []item, name string) (int, []string) {
	nameLower := strings.ToLower(name)

	var exact, prefix []int
	for i, it := range items {
		title := strings.ToLower(itemTitle(it))
		switch {
		case title == nameLower:
			exact = append(exact, i)
		case strings.HasPrefix(title, nameLower):
			prefix = append(prefix, i)
		}
	}

	// A single exact match wins outright, regardless of how many other titles
	// merely start with the same characters.
	if len(exact) == 1 {
		return exact[0], nil
	}

	// Otherwise the candidates are the exact matches (rare duplicate titles)
	// plus the prefix matches; the switch above keeps the two sets disjoint.
	candidates := append(append([]int{}, exact...), prefix...)
	if len(candidates) == 1 {
		return candidates[0], nil
	}

	titles := make([]string, len(candidates))
	for i, candidateIdx := range candidates {
		titles[i] = itemTitle(items[candidateIdx])
	}
	return -1, titles
}
