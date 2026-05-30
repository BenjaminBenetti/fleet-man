package devcontainer

// These types model fleet-man's slice of a devcontainer.json. The
// devcontainer spec lets each tool carve out a namespaced sub-object
// under the top-level "customizations" key (VS Code uses "vscode",
// GitHub uses "codespaces", Coder uses "coder", and so on); fleet-man
// reads its own "fleet" namespace and ignores every other tool's block.
//
//	{
//	  "customizations": {
//	    "fleet": {
//	      "browser": { "initialUrl": "http://localhost:3000" }
//	    }
//	  }
//	}
//
// The whole "fleet" block is decoded into FleetCustomizations rather
// than reaching in for individual keys, so adding a new project-level
// setting is just a new field there — no new parsing code, and the
// on-disk schema and in-memory shape stay in lockstep.

// Customizations mirrors the top-level "customizations" object in a
// devcontainer.json. Only the "fleet" namespace is modelled; sibling
// tool namespaces are left unparsed.
type Customizations struct {
	Fleet FleetCustomizations `json:"fleet"`
}
