package stripe

import (
	"fmt"
	"net/http"
	"strings"
)

type apiError struct {
	status      int
	Type        string `json:"type"`
	Code        string `json:"code,omitempty"`
	DeclineCode string `json:"decline_code,omitempty"`
	DocURL      string `json:"doc_url,omitempty"`
	Message     string `json:"message"`
	Param       string `json:"param,omitempty"`
}

func (e *apiError) Error() string { return e.Message }

func newAPIError(status int, typ, code, param, message string) *apiError {
	e := &apiError{status: status, Type: typ, Code: code, Param: param, Message: message}
	if code != "" {
		e.DocURL = "https://docs.stripe.com/error-codes/" + strings.ReplaceAll(code, "_", "-")
	}
	return e
}

func invalidRequest(param, format string, args ...any) *apiError {
	return newAPIError(http.StatusBadRequest, "invalid_request_error", "", param, fmt.Sprintf(format, args...))
}

func missingParam(param string) *apiError {
	return newAPIError(http.StatusBadRequest, "invalid_request_error", "parameter_missing", param,
		fmt.Sprintf("Missing required param: %s.", param))
}

func invalidInteger(param, value string) *apiError {
	return newAPIError(http.StatusBadRequest, "invalid_request_error", "parameter_invalid_integer", param,
		fmt.Sprintf("Invalid integer: %s", value))
}

func exclusiveParams(a, b string) *apiError {
	return newAPIError(http.StatusBadRequest, "invalid_request_error", "parameters_exclusive", a,
		fmt.Sprintf("You may only specify one of these parameters: %s, %s.", a, b))
}

func noSuch(kind, id, param string) *apiError {
	status := http.StatusBadRequest
	if param == "id" {
		status = http.StatusNotFound
	}
	return newAPIError(status, "invalid_request_error", "resource_missing", param,
		fmt.Sprintf("No such %s: '%s'", kind, id))
}

func cardDeclined(message, declineCode string) *apiError {
	e := newAPIError(http.StatusPaymentRequired, "card_error", "card_declined", "", message)
	e.DeclineCode = declineCode
	return e
}

func idempotencyError(status int, code, message string) *apiError {
	return newAPIError(status, "idempotency_error", code, "", message)
}
