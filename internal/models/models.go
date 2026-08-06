// Package models holds the model list offered in the launcher's dropdown.
//
// Each ID is passed verbatim to `claude --model <id>` in the spawned command —
// and always quoted there, since ids like `claude-opus-4-8[1m]` contain
// shell-special brackets. The entry flagged Default is pre-selected in the UI
// and used when a launch request omits a model.
package models

import (
	"fmt"
	"strings"
)

type Model struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Default bool   `json:"default,omitempty"`
}

var All = []Model{
	{ID: "claude-opus-4-8[1m]", Label: "Opus 4.8 · 1M context", Default: true},
	{ID: "claude-opus-4-8", Label: "Opus 4.8"},
	{ID: "claude-sonnet-4-6", Label: "Sonnet 4.6"},
	{ID: "claude-haiku-4-5-20251001", Label: "Haiku 4.5"},
}

func DefaultModel() Model {
	for _, m := range All {
		if m.Default {
			return m
		}
	}
	return All[0]
}

// Resolve maps a requested model id to a known model, or the default when empty.
func Resolve(id string) (Model, error) {
	want := strings.TrimSpace(id)
	if want == "" {
		return DefaultModel(), nil
	}
	for _, m := range All {
		if m.ID == want {
			return m, nil
		}
	}
	return Model{}, fmt.Errorf("unknown model: %s", want)
}
