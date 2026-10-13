package aws

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	stsNamespace = "https://sts.amazonaws.com/doc/2011-06-15/"
	// Limits of GetFederationToken.
	minSessionDuration     = 900 * time.Second
	maxSessionDuration     = 129600 * time.Second
	defaultSessionDuration = 12 * time.Hour
	maxSessionPolicyLength = 2048
)

var federatedNamePattern = regexp.MustCompile(`^[\w+=,.@-]{2,32}$`)

// session is a federated user session that GetFederationToken created:
// temporary credentials whose permissions are the session policy.
type session struct {
	Name            string    `json:"name"`
	AccessKeyID     string    `json:"access_key_id"`
	SecretAccessKey string    `json:"secret_access_key"`
	Token           string    `json:"session_token"`
	Policy          policy    `json:"policy"`
	Expires         time.Time `json:"expires"`
}

func (s *session) arn(account string) string {
	return "arn:aws:sts::" + account + ":federated-user/" + s.Name
}

// serveSTS answers STS GetCallerIdentity, which Terraform and Packer call to
// check credentials and learn the account id, and GetFederationToken, which
// the agent calls for temporary credentials limited to the registry. The
// configured access key is the IAM user elprovo-sim.
func (s *Simulation) serveSTS(w http.ResponseWriter, r *http.Request, body []byte, sc scope, failure authFailure, requestID string) {
	if failure != 0 {
		writeSTSError(w, requestID, stsAuthError(failure))
		return
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		writeSTSError(w, requestID, clientError("MalformedQueryString", "The request body is not a valid form."))
		return
	}
	var account string
	s.store.Read(func(wd *world) { account = wd.AccountID })
	action := values.Get("Action")
	var result any
	switch action {
	case "GetCallerIdentity":
		result = s.callerIdentity(account, sc.session)
	case "GetFederationToken":
		result, err = s.getFederationToken(account, sc.session, values)
	default:
		s.log.Warn("unsupported STS action", "action", action)
		err = clientError("InvalidAction", "Could not find operation %s for version %s", action, values.Get("Version"))
	}
	if err != nil {
		var apiErr *apiError
		if !errors.As(err, &apiErr) {
			s.log.Error("STS request failed", "action", action, "error", err)
			apiErr = &apiError{http.StatusInternalServerError, "InternalFailure", "An internal error has occurred"}
		}
		writeSTSError(w, requestID, apiErr)
		return
	}
	type response struct {
		XMLName   xml.Name
		XMLNS     string `xml:"xmlns,attr"`
		Result    any
		RequestID string `xml:"ResponseMetadata>RequestId"`
	}
	w.Header().Set("Content-Type", "text/xml")
	w.Header().Set("X-Amzn-Requestid", requestID)
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(response{XMLName: xml.Name{Local: action + "Response"}, XMLNS: stsNamespace, Result: result, RequestID: requestID})
}

type callerIdentityResult struct {
	XMLName xml.Name `xml:"GetCallerIdentityResult"`
	Arn     string   `xml:"Arn"`
	UserID  string   `xml:"UserId"`
	Account string   `xml:"Account"`
}

// callerIdentity names the IAM user elprovo-sim, or the federated user of a
// session.
func (s *Simulation) callerIdentity(account string, sess *session) callerIdentityResult {
	if sess != nil {
		return callerIdentityResult{Arn: sess.arn(account), UserID: account + ":" + sess.Name, Account: account}
	}
	sum := sha256.Sum256([]byte(s.cfg.accessKeyID))
	return callerIdentityResult{Arn: "arn:aws:iam::" + account + ":user/elprovo-sim", UserID: "AIDA" + strings.ToUpper(hex.EncodeToString(sum[:]))[:17], Account: account}
}

type federationTokenResult struct {
	XMLName     xml.Name `xml:"GetFederationTokenResult"`
	Credentials struct {
		SessionToken    string `xml:"SessionToken"`
		SecretAccessKey string `xml:"SecretAccessKey"`
		Expiration      string `xml:"Expiration"`
		AccessKeyID     string `xml:"AccessKeyId"`
	} `xml:"Credentials"`
	FederatedUser struct {
		Arn             string `xml:"Arn"`
		FederatedUserID string `xml:"FederatedUserId"`
	} `xml:"FederatedUser"`
	PackedPolicySize int `xml:"PackedPolicySize"`
}

// getFederationToken creates a federated session for the IAM user. Its
// permissions are the inline session policy; without one, it may do
// nothing. Session credentials cannot call it, as on AWS.
func (s *Simulation) getFederationToken(account string, caller *session, values url.Values) (any, error) {
	if caller != nil {
		return nil, &apiError{http.StatusForbidden, "AccessDenied", "Cannot call GetFederationToken with session credentials"}
	}
	for name := range values {
		if name != "Action" && name != "Version" && name != "Name" && name != "Policy" && name != "DurationSeconds" {
			return nil, clientError("UnknownParameter", "The parameter %s is not recognized by the simulation", name)
		}
	}
	name, doc := values.Get("Name"), values.Get("Policy")
	if !federatedNamePattern.MatchString(name) {
		return nil, validationError("name", name, `Member must satisfy regular expression pattern: [\w+=,.@-]* and have length between 2 and 32`)
	}
	duration := defaultSessionDuration
	if v := values.Get("DurationSeconds"); v != "" {
		n, err := strconv.Atoi(v)
		duration = time.Duration(n) * time.Second
		if err != nil || duration < minSessionDuration || duration > maxSessionDuration {
			return nil, validationError("durationSeconds", v, "Member must have value between 900 and 129600")
		}
	}
	if utf8.RuneCountInString(doc) > maxSessionPolicyLength {
		return nil, validationError("policy", "...", "Member must have length less than or equal to 2048")
	}
	var p policy
	if doc != "" {
		var err error
		if p, err = parsePolicy(doc); err != nil {
			return nil, clientError("MalformedPolicyDocument", "The policy is not valid: %v", err)
		}
	}
	now := s.now()
	sess := &session{
		Name: name, AccessKeyID: "ASIA" + randomBase32(10), SecretAccessKey: randomBase64(30), Token: randomBase64(96),
		Policy: p, Expires: now.Add(duration).UTC(),
	}
	err := s.store.Update(func(w *world) error {
		for id, old := range w.Sessions {
			if !now.Before(old.Expires) {
				delete(w.Sessions, id)
			}
		}
		w.Sessions[sess.AccessKeyID] = sess
		return nil
	})
	if err != nil {
		return nil, err
	}
	var out federationTokenResult
	out.Credentials.AccessKeyID, out.Credentials.SecretAccessKey, out.Credentials.SessionToken = sess.AccessKeyID, sess.SecretAccessKey, sess.Token
	out.Credentials.Expiration = sess.Expires.Format("2006-01-02T15:04:05.000Z")
	out.FederatedUser.Arn, out.FederatedUser.FederatedUserID = sess.arn(account), account+":"+name
	out.PackedPolicySize = utf8.RuneCountInString(doc) * 100 / maxSessionPolicyLength
	return &out, nil
}

func validationError(member, value, constraint string) *apiError {
	return clientError("ValidationError", "1 validation error detected: Value '%s' at '%s' failed to satisfy constraint: %s", value, member, constraint)
}

func randomBase32(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base32.StdEncoding.EncodeToString(b)
}

func randomBase64(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

func stsAuthError(failure authFailure) *apiError {
	switch failure {
	case authMissing:
		return &apiError{http.StatusForbidden, "MissingAuthenticationToken", "Request is missing Authentication Token"}
	case authIncomplete:
		return &apiError{http.StatusBadRequest, "IncompleteSignature", "The request signature does not conform to AWS standards."}
	case authUnknownKey, authBadToken:
		return &apiError{http.StatusForbidden, "InvalidClientTokenId", "The security token included in the request is invalid."}
	case authTokenExpired:
		return &apiError{http.StatusBadRequest, "ExpiredToken", "The security token included in the request is expired"}
	case authExpired:
		return &apiError{http.StatusForbidden, "SignatureDoesNotMatch", "Signature expired."}
	default:
		return &apiError{http.StatusForbidden, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided. Check your AWS Secret Access Key and signing method."}
	}
}

func writeSTSError(w http.ResponseWriter, requestID string, e *apiError) {
	type xmlError struct {
		Type    string `xml:"Type"`
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	type response struct {
		XMLName   xml.Name `xml:"ErrorResponse"`
		XMLNS     string   `xml:"xmlns,attr"`
		Error     xmlError `xml:"Error"`
		RequestID string   `xml:"RequestId"`
	}
	w.Header().Set("Content-Type", "text/xml")
	w.Header().Set("X-Amzn-Requestid", requestID)
	w.WriteHeader(e.status)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(response{XMLNS: stsNamespace, Error: xmlError{"Sender", e.code, e.message}, RequestID: requestID})
}
