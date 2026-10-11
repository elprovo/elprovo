package aws

import (
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
	sigV4Algorithm = "AWS4-HMAC-SHA256"
	amzDateFormat  = "20060102T150405Z"
	maxClockSkew   = 15 * time.Minute
)

// authFailure says why a request's signature was refused. Each service
// reports it with its own error code.
type authFailure int

const (
	authMissing authFailure = iota + 1
	authIncomplete
	authUnknownKey
	authBadToken
	authBadSignature
	authExpired
	authTokenExpired
)

func (f authFailure) String() string {
	return map[authFailure]string{
		authMissing: "no signature", authIncomplete: "malformed signature", authUnknownKey: "unknown access key",
		authBadToken: "session token", authBadSignature: "signature mismatch", authExpired: "signing time out of range",
		authTokenExpired: "session expired", authBadScope: "region not served",
	}[f]
}

// scope is what a valid signature tells about a request: the region and the
// service it was signed for, and the federated session whose credentials
// signed it, or nil for the IAM user.
type scope struct {
	region  string
	service string
	session *session
}

// verifySignature checks the request's Signature Version 4 Authorization
// header, with body as the payload, against the configured access key or
// the temporary credentials of a federated session, which also need their
// session token in X-Amz-Security-Token. It follows the signing steps in the
// AWS documentation: canonical request, string to sign, derived signing key.
func (s *Simulation) verifySignature(r *http.Request, body []byte) (scope, authFailure) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return scope{}, authMissing
	}
	auth, ok := parseAuthorization(header)
	if !ok {
		return scope{}, authIncomplete
	}
	secret, sess, failure := s.signingKey(auth.accessKeyID, r.Header.Get("X-Amz-Security-Token"))
	if failure != 0 {
		return scope{}, failure
	}
	date, err := time.Parse(amzDateFormat, r.Header.Get("X-Amz-Date"))
	if err != nil || date.Format("20060102") != auth.date || !slices.Contains(auth.signedHeaders, "host") {
		return scope{}, authIncomplete
	}
	if skew := s.now().Sub(date); skew > maxClockSkew || skew < -maxClockSkew {
		return scope{}, authExpired
	}
	want := signature(secret, auth, canonicalRequest(r, auth.signedHeaders, body), r.Header.Get("X-Amz-Date"))
	if !hmac.Equal([]byte(want), []byte(auth.signature)) {
		return scope{}, authBadSignature
	}
	if sess != nil && !s.now().Before(sess.Expires) {
		return scope{}, authTokenExpired
	}
	return scope{region: auth.region, service: auth.service, session: sess}, 0
}

// signingKey returns the secret access key of the access key id, and its
// session: the configured key takes no session token, and a session's key
// takes its own.
func (s *Simulation) signingKey(accessKeyID, token string) (string, *session, authFailure) {
	if accessKeyID == s.cfg.accessKeyID {
		if token != "" {
			return "", nil, authBadToken
		}
		return s.cfg.secretAccessKey, nil, 0
	}
	var sess *session
	s.store.Read(func(w *world) {
		if found := w.Sessions[accessKeyID]; found != nil {
			copied := *found
			sess = &copied
		}
	})
	if sess == nil {
		return "", nil, authUnknownKey
	}
	if !hmac.Equal([]byte(token), []byte(sess.Token)) {
		return "", nil, authBadToken
	}
	return sess.SecretAccessKey, sess, 0
}

type authorization struct {
	accessKeyID   string
	date          string
	region        string
	service       string
	signedHeaders []string
	signature     string
}

func parseAuthorization(header string) (authorization, bool) {
	rest, ok := strings.CutPrefix(header, sigV4Algorithm+" ")
	if !ok {
		return authorization{}, false
	}
	fields := map[string]string{}
	for _, part := range strings.Split(rest, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return authorization{}, false
		}
		fields[k] = v
	}
	credential := strings.Split(fields["Credential"], "/")
	if len(credential) != 5 || credential[4] != "aws4_request" || fields["SignedHeaders"] == "" || fields["Signature"] == "" {
		return authorization{}, false
	}
	return authorization{
		accessKeyID: credential[0], date: credential[1], region: credential[2], service: credential[3],
		signedHeaders: strings.Split(fields["SignedHeaders"], ";"), signature: fields["Signature"],
	}, true
}

func canonicalRequest(r *http.Request, signedHeaders []string, body []byte) string {
	var headers strings.Builder
	for _, name := range signedHeaders {
		values := r.Header.Values(name)
		if name == "host" {
			values = []string{r.Host}
		}
		trimmed := make([]string, len(values))
		for i, v := range values {
			trimmed[i] = strings.Join(strings.Fields(v), " ")
		}
		headers.WriteString(name + ":" + strings.Join(trimmed, ",") + "\n")
	}
	payload := sha256.Sum256(body)
	return strings.Join([]string{
		r.Method,
		canonicalPath(r.URL),
		canonicalQuery(r.URL.Query()),
		headers.String(),
		strings.Join(signedHeaders, ";"),
		hex.EncodeToString(payload[:]),
	}, "\n")
}

// canonicalPath encodes each segment of the request's escaped path once
// more, as Signature Version 4 asks of every service but Amazon S3.
func canonicalPath(u *url.URL) string {
	path := u.EscapedPath()
	if path == "" {
		return "/"
	}
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		segments[i] = uriEncode(seg)
	}
	return strings.Join(segments, "/")
}

func canonicalQuery(q url.Values) string {
	var pairs []string
	for k, values := range q {
		for _, v := range values {
			pairs = append(pairs, uriEncode(k)+"="+uriEncode(v))
		}
	}
	slices.Sort(pairs)
	return strings.Join(pairs, "&")
}

// uriEncode percent-encodes every byte except the unreserved characters of
// RFC 3986, as Signature Version 4 requires.
func uriEncode(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
	}
	return b.String()
}

func signature(secret string, auth authorization, canonical, amzDate string) string {
	hash := sha256.Sum256([]byte(canonical))
	credentialScope := strings.Join([]string{auth.date, auth.region, auth.service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{sigV4Algorithm, amzDate, credentialScope, hex.EncodeToString(hash[:])}, "\n")
	key := []byte("AWS4" + secret)
	for _, part := range []string{auth.date, auth.region, auth.service, "aws4_request"} {
		key = hmacSHA256(key, part)
	}
	return hex.EncodeToString(hmacSHA256(key, stringToSign))
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}
