package coder

import (
	"encoding/json"
	"fmt"
)

// RichParameter represents a template parameter from the Coder API.
type RichParameter struct {
	Name         string        `json:"name"`
	DisplayName  string        `json:"display_name"`
	Description  string        `json:"description"`
	Type         string        `json:"type"` // "string", "number"
	DefaultValue string        `json:"default_value"`
	Options      []ParamOption `json:"options"`
	Required     bool          `json:"required"`
	Mutable      bool          `json:"mutable"`
}

// ParamOption represents an allowed value for a parameter.
type ParamOption struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// FetchRichParameters fetches the rich parameters for a template version via REST API.
func FetchRichParameters(versionID string) ([]RichParameter, error) {
	body, err := coderAPIGet(fmt.Sprintf("api/v2/templateversions/%s/rich-parameters", versionID))
	if err != nil {
		return nil, err
	}

	var params []RichParameter
	if err := json.Unmarshal(body, &params); err != nil {
		return nil, fmt.Errorf("parsing rich parameters: %w", err)
	}
	return params, nil
}
