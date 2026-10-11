package aws

import (
	"cmp"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	amzDateFormat  = "20060102T150405Z"
	signAlgorithm  = "AWS4-HMAC-SHA256"
	scopeTerminate = "aws4_request"
)

// Credentials are an AWS access key and, for temporary credentials, the
// session token that goes with it.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// signer signs requests to one AWS service in one region with Signature
// Version 4, the way the AWS SDKs do for the requests the agent makes.
type signer struct {
	creds   Credentials
	region  string
	service string
	now     func() time.Time
}

// sign adds X-Amz-Date, X-Amz-Security-Token when the credentials have a
// session token, and the Authorization header to req, whose body is body.
// It signs the host, content-type, x-amz-date, x-amz-target and
// x-amz-security-token headers that req has.
func (s signer) sign(req *http.Request, body []byte) {
	t := s.now().UTC()
	req.Header.Set("X-Amz-Date", t.Format(amzDateFormat))
	if s.creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", s.creds.SessionToken)
	}
	headers := map[string]string{"host": req.Host}
	if req.Host == "" {
		headers["host"] = req.URL.Host
	}
	for _, name := range []string{"Content-Type", "X-Amz-Date", "X-Amz-Target", "X-Amz-Security-Token"} {
		if v := req.Header.Get(name); v != "" {
			headers[strings.ToLower(name)] = strings.TrimSpace(v)
		}
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	slices.Sort(names)
	var canonicalHeaders strings.Builder
	for _, name := range names {
		canonicalHeaders.WriteString(name + ":" + headers[name] + "\n")
	}
	signedHeaders := strings.Join(names, ";")
	payloadHash := sha256.Sum256(body)
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalPath(req.URL),
		canonicalQuery(req.URL.Query()),
		canonicalHeaders.String(),
		signedHeaders,
		hex.EncodeToString(payloadHash[:]),
	}, "\n")

	date := t.Format("20060102")
	scope := date + "/" + s.region + "/" + s.service + "/" + scopeTerminate
	requestHash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := signAlgorithm + "\n" + t.Format(amzDateFormat) + "\n" + scope + "\n" + hex.EncodeToString(requestHash[:])

	key := hmacSHA256([]byte("AWS4"+s.creds.SecretAccessKey), date)
	for _, part := range []string{s.region, s.service, scopeTerminate} {
		key = hmacSHA256(key, part)
	}
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))
	req.Header.Set("Authorization", signAlgorithm+" Credential="+s.creds.AccessKeyID+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+signature)
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

// canonicalPath is the URI-encoded path, with "/" for an empty one.
func canonicalPath(u *url.URL) string {
	path := u.EscapedPath()
	if path == "" {
		return "/"
	}
	return path
}

// canonicalQuery sorts the query parameters by name and value and encodes
// them as SigV4 requires: spaces as %20, not +.
func canonicalQuery(q url.Values) string {
	type pair struct{ name, value string }
	pairs := make([]pair, 0, len(q))
	for name, values := range q {
		for _, v := range values {
			pairs = append(pairs, pair{uriEncode(name), uriEncode(v)})
		}
	}
	slices.SortFunc(pairs, func(a, b pair) int {
		return cmp.Or(strings.Compare(a.name, b.name), strings.Compare(a.value, b.value))
	})
	encoded := make([]string, len(pairs))
	for i, p := range pairs {
		encoded[i] = p.name + "=" + p.value
	}
	return strings.Join(encoded, "&")
}

func uriEncode(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}
