package vault

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"agentrm/internal/model"
)

// Template fills in an API key account for a well-known API, so adding one is picking it and
// pasting the key. The list is templates.json, kept by hand: add an entry there, and the
// tests check it is one the vault would accept.
type Template struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
	APIURL   string `json:"api_url"`
	Header   string `json:"header,omitempty"`
	Prefix   string `json:"prefix,omitempty"`
	RawKey   bool   `json:"raw_key,omitempty"`
	KeyEnv   string `json:"key_env"`
	BaseEnv  string `json:"base_env"`
	KeyURL   string `json:"key_url,omitempty"` // where to make a key
	Note     string `json:"note,omitempty"`    // what to know before using it
}

//go:embed templates.json
var templatesJSON []byte

// Templates returns the API key templates, in the file's order.
func Templates() ([]Template, error) {
	var ts []Template
	if err := json.Unmarshal(templatesJSON, &ts); err != nil {
		return nil, fmt.Errorf("templates.json: %w", err)
	}
	return ts, nil
}

// Account is the API key account the template describes, named after it.
func (t Template) Account() model.VaultAccount {
	return model.VaultAccount{
		Name: t.ID, Kind: model.VaultAPIKey, Note: t.Name, MaxMinutes: defaultMax,
		Scopes: []string{"full", "read"},
		APIURL: t.APIURL, Header: t.Header, Prefix: t.Prefix, RawKey: t.RawKey,
		KeyEnv: t.KeyEnv, BaseEnv: t.BaseEnv,
	}
}
