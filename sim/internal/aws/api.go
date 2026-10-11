package aws

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

const (
	maxRequestBody = 1 << 20
	ec2Version     = "2016-11-15"
	ec2Namespace   = "http://ec2.amazonaws.com/doc/2016-11-15/"
)

// apiError is an error response. code is the service's error code, such as
// InvalidInstanceID.NotFound.
type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.code + ": " + e.message }

func clientError(code, format string, args ...any) *apiError {
	return &apiError{http.StatusBadRequest, code, fmt.Sprintf(format, args...)}
}

func errNotFound(code, format string, args ...any) *apiError {
	return clientError(code, format, args...)
}

func errInvalidParameter(format string, args ...any) *apiError {
	return clientError("InvalidParameterValue", format, args...)
}

func errMissingParameter(name string) *apiError {
	return clientError("MissingParameter", "The request must contain the parameter %s", name)
}

func errUnsupported(format string, args ...any) *apiError {
	return clientError("UnsupportedOperation", format+" is not supported by the simulation", args...)
}

func (s *Simulation) handler() http.Handler {
	ec2 := s.ec2Actions()
	registryTokens := s.registryTokens()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == registryTokenPath {
			registryTokens.ServeHTTP(w, r)
			return
		}
		requestID := newUUID()
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
		if err != nil {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		sc, failure := s.verifySignature(r, body)
		service := sc.service
		if failure != 0 {
			service = claimedService(r)
		}
		if failure == 0 && !s.servesRegion(sc) {
			failure = authBadScope
		}
		if failure != 0 {
			s.log.Warn("refused a request", "reason", failure.String(), "service", service, "target", r.Header.Get("X-Amz-Target"))
		}
		switch service {
		case "sts":
			s.serveSTS(w, r, body, sc, failure, requestID)
		case "ecr":
			s.serveECR(w, r, body, sc, failure, requestID)
		default:
			s.serveEC2(w, r, body, ec2, sc, failure, requestID)
		}
	})
}

// authBadScope is a valid signature for a region the simulation does not
// serve.
const authBadScope authFailure = 100

func (s *Simulation) servesRegion(sc scope) bool {
	return slices.Contains(s.cfg.regions, sc.region) || (sc.service == "sts" && sc.region == "us-east-1")
}

// claimedService returns the service that a request's Authorization header
// names, so a refused request gets its service's error format.
func claimedService(r *http.Request) string {
	if auth, ok := parseAuthorization(r.Header.Get("Authorization")); ok {
		return auth.service
	}
	if strings.HasPrefix(r.Header.Get("X-Amz-Target"), ecrTargetPrefix) {
		return "ecr"
	}
	return "ec2"
}

// ec2Call is one EC2 Query API request.
type ec2Call struct {
	ctx    context.Context
	region string
	params params
}

// ec2Response is a response document. Each one embeds ec2Meta.
type ec2Response interface {
	setRequestID(id string)
}

type ec2Meta struct {
	RequestID string `xml:"requestId"`
}

func (m *ec2Meta) setRequestID(id string) { m.RequestID = id }

// returnResponse is the answer of actions that report only success.
type returnResponse struct {
	ec2Meta
	Return bool `xml:"return"`
}

func ok() (ec2Response, error) { return &returnResponse{Return: true}, nil }

// ec2Action handles one action. params lists the top-level request
// parameters it understands; a request with any other parameter is refused,
// so an unsupported option never passes silently.
type ec2Action struct {
	handle func(c *ec2Call) (ec2Response, error)
	params []string
}

func (s *Simulation) serveEC2(w http.ResponseWriter, r *http.Request, body []byte, actions map[string]ec2Action, sc scope, failure authFailure, requestID string) {
	values, err := url.ParseQuery(string(body))
	if err != nil {
		writeEC2Error(w, requestID, clientError("MalformedQueryString", "The request body is not a valid form."))
		return
	}
	for k, v := range r.URL.Query() {
		values[k] = append(values[k], v...)
	}
	if failure != 0 {
		writeEC2Error(w, requestID, ec2AuthError(failure, sc))
		return
	}
	name := values.Get("Action")
	action, found := actions[name]
	if !found {
		s.log.Warn("unsupported EC2 action", "action", name)
		writeEC2Error(w, requestID, clientError("InvalidAction", "The action %s is not valid for this web service.", name))
		return
	}
	if sc.session != nil && !sc.session.Policy.allows("ec2:"+name, "*") {
		writeEC2Error(w, requestID, &apiError{http.StatusForbidden, "UnauthorizedOperation", "You are not authorized to perform this operation: no session policy allows ec2:" + name})
		return
	}
	p := params{values: values}
	if unknown := p.unknown(append([]string{"Action", "Version", "DryRun"}, action.params...)); unknown != "" {
		s.log.Warn("unsupported EC2 parameter", "action", name, "parameter", unknown)
		writeEC2Error(w, requestID, clientError("UnknownParameter", "The parameter %s is not recognized by the simulation", unknown))
		return
	}
	if p.get("DryRun") == "true" {
		writeEC2Error(w, requestID, &apiError{http.StatusPreconditionFailed, "DryRunOperation", "Request would have succeeded, but DryRun flag is set."})
		return
	}
	resp, err := action.handle(&ec2Call{ctx: r.Context(), region: sc.region, params: p})
	if err != nil {
		var apiErr *apiError
		if !errors.As(err, &apiErr) {
			s.log.Error("EC2 request failed", "action", name, "error", err)
			apiErr = &apiError{http.StatusInternalServerError, "InternalError", "An internal error has occurred"}
		}
		writeEC2Error(w, requestID, apiErr)
		return
	}
	resp.setRequestID(requestID)
	w.Header().Set("Content-Type", "text/xml;charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, xml.Header)
	start := xml.StartElement{Name: xml.Name{Local: name + "Response"}, Attr: []xml.Attr{{Name: xml.Name{Local: "xmlns"}, Value: ec2Namespace}}}
	if err := xml.NewEncoder(w).EncodeElement(resp, start); err != nil {
		s.log.Error("encode EC2 response", "action", name, "error", err)
	}
}

func ec2AuthError(failure authFailure, sc scope) *apiError {
	switch failure {
	case authMissing:
		return &apiError{http.StatusUnauthorized, "MissingAuthenticationToken", "The request must contain valid credentials."}
	case authIncomplete:
		return &apiError{http.StatusBadRequest, "IncompleteSignature", "The request signature does not conform to AWS standards."}
	case authExpired:
		return &apiError{http.StatusBadRequest, "RequestExpired", "Request has expired."}
	case authBadSignature:
		return &apiError{http.StatusForbidden, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided. Check your AWS Secret Access Key and signing method."}
	case authBadScope:
		return &apiError{http.StatusUnauthorized, "AuthFailure", fmt.Sprintf("Credential should be scoped to a valid region, not '%s'.", sc.region)}
	default:
		return &apiError{http.StatusUnauthorized, "AuthFailure", "AWS was not able to validate the provided access credentials"}
	}
}

func writeEC2Error(w http.ResponseWriter, requestID string, e *apiError) {
	type xmlError struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	type response struct {
		XMLName   xml.Name   `xml:"Response"`
		Errors    []xmlError `xml:"Errors>Error"`
		RequestID string     `xml:"RequestID"`
	}
	w.Header().Set("Content-Type", "text/xml;charset=UTF-8")
	w.WriteHeader(e.status)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(response{Errors: []xmlError{{e.code, e.message}}, RequestID: requestID})
}

func (s *Simulation) ec2Actions() map[string]ec2Action {
	all := map[string]ec2Action{}
	for _, group := range []map[string]ec2Action{
		s.ec2InstanceActions(), s.ec2KeyPairActions(), s.ec2VPCActions(), s.ec2SubnetActions(), s.ec2RouteActions(),
		s.ec2SecurityGroupActions(), s.ec2AddressActions(), s.ec2ImageActions(), s.ec2TagActions(), s.ec2CatalogActions(), s.ec2InterfaceActions(),
	} {
		maps.Copy(all, group)
	}
	return all
}
