package vault

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"agentrm/internal/model"
)

// AWS: the vault holds keys allowed to assume some roles, and each grant is an STS session for
// one of them (15 minutes at the least, by STS's rule). An STS session cannot be ended early
// without changing the role's policy, so a grant revoked early still works until it expires:
// the record says so.

func stsEndpoint(a model.VaultAccount) (string, string) {
	region := or(a.Region, "us-east-1")
	if a.APIURL != "" {
		return strings.TrimRight(a.APIURL, "/") + "/", region
	}
	return "https://sts." + region + ".amazonaws.com/", region
}

var sessionNameRE = regexp.MustCompile(`[^\w+=,.@-]`)

func mintAWS(ctx context.Context, a model.VaultAccount, m Material, ask model.VaultAsk, o Opts) (*Credential, error) {
	akid, secret := strings.TrimSpace(m["access_key_id"]), strings.TrimSpace(m["secret_access_key"])
	if akid == "" || secret == "" {
		return nil, fmt.Errorf("%s has no AWS keys saved in AgentRM", a.Name)
	}
	name := "agentrm-" + sessionNameRE.ReplaceAllString(ask.Session, "")
	if len(name) > 64 {
		name = name[:64]
	}
	form := url.Values{
		"Action": {"AssumeRole"}, "Version": {"2011-06-15"}, "RoleArn": {ask.Scope},
		"RoleSessionName": {name}, "DurationSeconds": {fmt.Sprint(max(ask.Minutes, awsMin) * 60)},
	}
	if a.ExternalID != "" {
		form.Set("ExternalId", a.ExternalID)
	}
	body := form.Encode()
	endpoint, region := stsEndpoint(a)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	if tok := strings.TrimSpace(m["session_token"]); tok != "" {
		req.Header.Set("X-Amz-Security-Token", tok)
	}
	signV4(req, []byte(body), akid, secret, region, "sts", o.Now)
	resp, err := o.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("AWS STS did not answer: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Code    string `xml:"Error>Code"`
			Message string `xml:"Error>Message"`
		}
		_ = xml.Unmarshal(raw, &e)
		return nil, fmt.Errorf("AWS refused the role (%s): %s %s", resp.Status, e.Code, e.Message)
	}
	var out struct {
		Creds struct {
			AccessKeyID     string    `xml:"AccessKeyId"`
			SecretAccessKey string    `xml:"SecretAccessKey"`
			SessionToken    string    `xml:"SessionToken"`
			Expiration      time.Time `xml:"Expiration"`
		} `xml:"AssumeRoleResult>Credentials"`
	}
	if err := xml.Unmarshal(raw, &out); err != nil || out.Creds.SessionToken == "" {
		return nil, fmt.Errorf("AWS's answer had no credentials")
	}
	c := out.Creds
	return &Credential{
		Env: map[string]string{
			"AWS_ACCESS_KEY_ID": c.AccessKeyID, "AWS_SECRET_ACCESS_KEY": c.SecretAccessKey,
			"AWS_SESSION_TOKEN": c.SessionToken, "AWS_REGION": region, "AWS_DEFAULT_REGION": region,
		},
		// A profile chosen in your shell must not win over the session.
		Unset:   []string{"AWS_PROFILE", "AWS_DEFAULT_PROFILE"},
		Expires: c.Expiration,
		Note:    "an AWS session cannot be cut short: it works until " + c.Expiration.Local().Format("15:04") + " even if revoked",
	}, nil
}

// signV4 signs req with AWS Signature Version 4, over host, content-type, x-amz-date and, if
// set, x-amz-security-token.
// https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_sigv-create-signed-request.html
func signV4(req *http.Request, body []byte, akid, secret, region, service string, t time.Time) {
	t = t.UTC()
	amzDate, day := t.Format("20060102T150405Z"), t.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	headers := map[string]string{"host": req.URL.Host}
	for _, h := range []string{"Content-Type", "X-Amz-Date", "X-Amz-Security-Token"} {
		if v := req.Header.Get(h); v != "" {
			headers[strings.ToLower(h)] = strings.TrimSpace(v)
		}
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonical := strings.Join([]string{req.Method, path, canonicalQuery(req.URL.Query()),
		canonHeaders.String(), signed, hexSHA(body)}, "\n")
	scope := day + "/" + region + "/" + service + "/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hexSHA([]byte(canonical))
	k := hmacSHA([]byte("AWS4"+secret), day)
	for _, part := range []string{region, service, "aws4_request"} {
		k = hmacSHA(k, part)
	}
	sig := hex.EncodeToString(hmacSHA(k, toSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", akid, scope, signed, sig))
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, awsEscape(k)+"="+awsEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

// awsEscape is RFC 3986 escaping: everything but unreserved characters, spaces as %20.
func awsEscape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

func hexSHA(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func hmacSHA(key []byte, s string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(s))
	return h.Sum(nil)
}
