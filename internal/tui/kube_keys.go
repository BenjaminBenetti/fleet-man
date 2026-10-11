package tui

import "strings"

// kube_keys.go lists every key the Kubernetes views act on. The keybindings
// list and each view's footer are built from these tables, and a test feeds
// every key to each view to check that nothing it acts on is missing from
// them. Keys that mean the same in every dialog (j/k, enter to confirm,
// q/esc) are "shared": the keybindings list's Dialogs group already names
// them, so the "Kubernetes (Settings)" group lists only the rest.

// kubeKey is one key (or key pair) a Kubernetes view acts on.
type kubeKey struct {
	keys   []string // the tea key strings the view matches
	label  string   // the keybindings list's key column
	hint   string   // the footer's short form
	desc   string   // the keybindings list's description
	shared bool     // listed by the Dialogs group (label and meaning as there)
}

// kubeRowKeys act on the Kubernetes rows of Settings.
var kubeRowKeys = []kubeKey{
	{keys: []string{"enter", " "}, label: "enter", hint: "enter: edit",
		desc: "Edit a cluster (+ row: connect one)"},
	{keys: []string{"right", "l", "left", "h"}, label: "→ / l, ← / h", hint: "l: [ test ] [ delete ]",
		desc: "Select a cluster's [ test ] / [ delete ]"},
}

// kubeDialogMove and kubeDialogClose are the shared keys of the dialogs.
var (
	kubeDialogMove  = kubeKey{keys: []string{"j", "k", "up", "down"}, label: "j / k", hint: "[j/k]", shared: true}
	kubeDialogClose = kubeKey{keys: []string{"esc", "q"}, label: "q / esc", hint: "[esc] close", shared: true}
)

// kubePickerConnect is the picker's enter (a re-point's footer words it
// differently).
var kubePickerConnect = kubeKey{keys: []string{"enter"}, label: "enter", hint: "[enter] test & connect", desc: "Picker: test & connect (re-point: move there)"}

// kubePickerKeys act on the context picker.
var kubePickerKeys = []kubeKey{
	kubeDialogMove,
	{keys: []string{"/"}, label: "/", hint: "[/] filter", desc: "Picker: filter contexts (esc leaves the filter)"},
	kubePickerConnect,
	{keys: []string{"n"}, label: "n", hint: "[n] namespace", desc: "Picker: choose the namespace"},
	{keys: []string{"c"}, label: "c", hint: "[c] create namespace", desc: "Create the missing namespace (shows labels)"},
	{keys: []string{"ctrl+o"}, label: "ctrl+o", hint: "[ctrl+o] other kubeconfig", desc: "Picker: read another kubeconfig on fleetd's host"},
	{keys: []string{"L"}, label: "L", hint: "[L] login here", desc: "Run the login plugin here (local fleetd only)"},
	kubeDialogClose,
}

// kubeEditorKeys act on the cluster editor.
var kubeEditorKeys = []kubeKey{
	kubeDialogMove,
	{keys: []string{"enter", " "}, label: "enter (editor)", hint: "[enter] edit", desc: "Edit or toggle a field, fold a group"},
	{keys: []string{"left", "right", "h", "l"}, label: "← / → (editor)", hint: "[←/→] change", desc: "Cycle a choice, step to a row's buttons"},
	{keys: []string{"c"}, label: "c", hint: "[c] create namespace", desc: "Create the missing namespace (shows labels)"},
	{keys: []string{"L"}, label: "L", hint: "[L] login here", desc: "Run the login plugin here (local fleetd only)"},
	{keys: []string{"y"}, label: "y (editor)", hint: "[y] yes", desc: "Editor: test a StorageClass whose volumes are large"},
	{keys: []string{"n"}, label: "n (editor)", hint: "[n] no", desc: "Editor: do not test it"},
	kubeDialogClose,
}

// kubeNamespaceKeys act on the namespace Create panel.
var kubeNamespaceKeys = []kubeKey{
	{keys: []string{"enter"}, label: "enter", hint: "[enter] create", shared: true},
	{keys: []string{"y"}, label: "y", hint: "[y] copy manifest", desc: "Copy the namespace manifest to apply it yourself"},
	{keys: []string{"esc", "q"}, label: "q / esc", hint: "[esc] back", shared: true},
}

// kubeRemoveKeys act on the disconnect dialog.
var kubeRemoveKeys = []kubeKey{
	{keys: []string{"j", "k", "up", "down", "left", "right", "h", "l", "tab", "shift+tab"}, label: "j / k", hint: "[j/k] choose", shared: true},
	{keys: []string{"enter"}, label: "enter", hint: "[enter] confirm", shared: true},
	{keys: []string{"esc", "q"}, label: "q / esc", hint: "[esc] cancel", shared: true},
}

// kubeKeyTables is every table, for the keybindings list and its test.
func kubeKeyTables() [][]kubeKey {
	return [][]kubeKey{kubeRowKeys, kubePickerKeys, kubeEditorKeys, kubeNamespaceKeys, kubeRemoveKeys}
}

// kubeKeybindingEntries is the "Kubernetes (Settings)" group of the
// keybindings list: each table's own keys once (the shared ones are in the
// Dialogs group).
func kubeKeybindingEntries() []keybindingEntry {
	var out []keybindingEntry
	seen := map[string]bool{}
	for _, table := range kubeKeyTables() {
		for _, k := range table {
			if k.shared || seen[k.desc] {
				continue
			}
			seen[k.desc] = true
			out = append(out, keybindingEntry{Key: k.label, Desc: k.desc})
		}
	}
	return out
}

// kubeFooter renders a view's footer from its table; shown leaves out the
// keys that do nothing in the view's current state (nil shows them all).
func kubeFooter(table []kubeKey, shown func(kubeKey) bool) string {
	var parts []string
	for _, k := range table {
		if shown == nil || shown(k) {
			parts = append(parts, k.hint)
		}
	}
	return strings.Join(parts, "  ")
}

// kubeKeyIs reports whether entry k covers key s.
func kubeKeyIs(k kubeKey, s string) bool {
	for _, key := range k.keys {
		if key == s {
			return true
		}
	}
	return false
}
