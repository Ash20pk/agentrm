package vault

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// Route is what the proxy needs to forward one request for a grant.
type Route struct {
	Upstream *url.URL // scheme and host of the real API
	Header   string   // where the key goes
	Prefix   string   // before the key in it
	Key      string
	ReadOnly bool // GET, HEAD and OPTIONS only
	// Ended is closed when the grant ends; a request still being served (a long stream) is cut
	// then, not left to finish. nil: never.
	Ended <-chan struct{}
}

// Lookup finds the live grant a proxy token belongs to, and counts the use. ok is false for a
// token that is unknown, expired or revoked; why says which, for the agent.
type Lookup func(token string) (r Route, why string, ok bool)

// NewProxy serves http://127.0.0.1:<port>: a tool pointed at it (its base URL variable) sends
// the proxy token where it would send the key, in whatever header it uses; the proxy finds the
// token, removes it, puts the real key in the account's header and forwards the request to
// the real API over HTTPS. The agent never holds the key, and the grant ending ends its access.
func NewProxy(lookup Lookup) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, header := findToken(r.Header)
		if tok == "" {
			proxyError(w, http.StatusUnauthorized, "no AgentRM vault token on this request: run it with `agentrm vault run <account> -- <command>`")
			return
		}
		route, why, ok := lookup(tok)
		if !ok {
			proxyError(w, http.StatusUnauthorized, "AgentRM vault: "+why)
			return
		}
		if route.ReadOnly && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			proxyError(w, http.StatusForbidden, "AgentRM vault: this grant is read-only (GET); ask for --scope full to "+r.Method)
			return
		}
		// "." and ".." are refused, not resolved: the API would resolve them, and /v1/../admin
		// is /admin there. Backslashes too, which some servers take for slashes.
		if dotted(r.URL.Path) {
			proxyError(w, http.StatusBadRequest, "AgentRM vault: a path with . or .. segments or backslashes is not forwarded")
			return
		}
		if base := strings.TrimRight(route.Upstream.Path, "/"); base != "" && r.URL.Path != base && !strings.HasPrefix(r.URL.Path, base+"/") {
			proxyError(w, http.StatusForbidden, "AgentRM vault: this grant is for "+route.Upstream.String()+" only")
			return
		}
		// Every header carrying the token goes, so it never reaches the API.
		for name, vals := range r.Header {
			for _, v := range vals {
				if strings.Contains(v, ProxyTokenPrefix) || name == header {
					r.Header.Del(name)
					break
				}
				if b, ok := strings.CutPrefix(v, "Basic "); ok {
					if raw, err := base64.StdEncoding.DecodeString(b); err == nil && strings.Contains(string(raw), ProxyTokenPrefix) {
						r.Header.Del(name)
						break
					}
				}
			}
		}
		if route.Ended != nil {
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			go func() {
				select {
				case <-route.Ended:
					cancel()
				case <-ctx.Done():
				}
			}()
			r = r.WithContext(ctx)
		}
		rp := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.URL.Scheme, pr.Out.URL.Host = route.Upstream.Scheme, route.Upstream.Host
				pr.Out.Host = route.Upstream.Host
				pr.Out.Header.Set(or(route.Header, "Authorization"), route.Prefix+route.Key)
				pr.Out.Header.Del("X-Forwarded-For")
			},
			FlushInterval: -1, // streamed answers (server-sent events) pass at once
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
				select {
				case <-route.Ended:
					proxyError(w, http.StatusUnauthorized, "AgentRM vault: the grant ended while this request was waiting for the API")
					return
				default:
				}
				proxyError(w, http.StatusBadGateway, "AgentRM vault: the API did not answer: "+err.Error())
			},
		}
		rp.ServeHTTP(w, r)
	})
}

// dotted reports whether a decoded path has a "." or ".." segment, or a backslash.
func dotted(p string) bool {
	if strings.ContainsRune(p, '\\') {
		return true
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

// findToken looks for a proxy token in any header: as the whole value, after "Bearer ", or in
// HTTP basic auth (Stripe's clients send the key as the user name).
func findToken(h http.Header) (tok, header string) {
	for name, vals := range h {
		for _, v := range vals {
			v = strings.TrimSpace(v)
			if t := strings.TrimSpace(strings.TrimPrefix(v, "Bearer ")); strings.HasPrefix(t, ProxyTokenPrefix) {
				return t, name
			}
			if b, ok := strings.CutPrefix(v, "Basic "); ok {
				if raw, err := base64.StdEncoding.DecodeString(b); err == nil {
					user, pass, _ := strings.Cut(string(raw), ":")
					for _, t := range []string{user, pass} {
						if strings.HasPrefix(t, ProxyTokenPrefix) {
							return t, name
						}
					}
				}
			}
		}
	}
	return "", ""
}

func proxyError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": msg, "type": "agentrm_vault"}})
}
