package vault

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

// Every template must be an account the vault accepts, with nothing left to fill in but the key.
func TestTemplates(t *testing.T) {
	ts, err := Templates()
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) < 40 {
		t.Fatalf("only %d templates", len(ts))
	}
	// Unknown fields are mistakes (a misspelt "heder" would silently send the key in Authorization).
	dec := json.NewDecoder(bytes.NewReader(templatesJSON))
	dec.DisallowUnknownFields()
	var strict []Template
	if err := dec.Decode(&strict); err != nil {
		t.Fatalf("templates.json: %v", err)
	}
	ids, keys, cats := map[string]bool{}, map[string]string{}, map[string]bool{}
	for i, tp := range ts {
		// One run per category: the app and the CLI print a heading where it changes.
		if i == 0 || ts[i-1].Category != tp.Category {
			if cats[tp.Category] {
				t.Errorf("%s: %s already ended above; keep a category's entries together", tp.ID, tp.Category)
			}
			cats[tp.Category] = true
		}
		a := tp.Account()
		if err := CheckAccount(&a); err != nil {
			t.Errorf("%s: %v", tp.ID, err)
		}
		if ids[tp.ID] {
			t.Errorf("%s: listed twice", tp.ID)
		}
		ids[tp.ID] = true
		if tp.Name == "" || tp.Category == "" || tp.KeyEnv == "" || tp.BaseEnv == "" {
			t.Errorf("%s: needs a name, a category, a key variable and an address variable", tp.ID)
		}
		if other, ok := keys[tp.KeyEnv]; ok {
			t.Errorf("%s and %s both set %s", other, tp.ID, tp.KeyEnv)
		}
		keys[tp.KeyEnv] = tp.ID
		u, err := url.Parse(tp.APIURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || strings.HasSuffix(tp.APIURL, "/") {
			t.Errorf("%s: %q: an https address with no query and no trailing slash", tp.ID, tp.APIURL)
		}
		if tp.KeyURL != "" && !strings.HasPrefix(tp.KeyURL, "https://") {
			t.Errorf("%s: key_url %q is not https", tp.ID, tp.KeyURL)
		}
		if tp.RawKey && (tp.Prefix != "" || (tp.Header != "" && !strings.EqualFold(tp.Header, "Authorization"))) {
			t.Errorf("%s: raw_key is for a bare key in Authorization", tp.ID)
		}
		if tp.Prefix != "" && !strings.HasSuffix(tp.Prefix, " ") && !strings.HasSuffix(tp.Prefix, "=") {
			t.Errorf("%s: prefix %q would run into the key", tp.ID, tp.Prefix)
		}
	}
	for _, must := range []string{"openai", "anthropic", "gemini", "openrouter", "stripe"} {
		if !ids[must] {
			t.Errorf("no %s template", must)
		}
	}
}
