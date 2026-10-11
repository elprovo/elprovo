package azure

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	tokenLifetime = time.Hour
	// graphResource is the Microsoft Graph resource that the metadata
	// document names. Resource Manager refuses tokens issued for it.
	graphResource = "https://graph.microsoft.com"
)

// tokenClaims are the access token claims that Microsoft Entra ID issues to
// a service principal and that clients read, such as the Terraform
// provider, which takes the tenant and object ids from them.
type tokenClaims struct {
	Audience  string `json:"aud"`
	Issuer    string `json:"iss"`
	IssuedAt  int64  `json:"iat"`
	NotBefore int64  `json:"nbf"`
	Expires   int64  `json:"exp"`
	AppID     string `json:"appid"`
	AppIDACR  string `json:"appidacr"`
	IDP       string `json:"idp"`
	IDType    string `json:"idtyp"`
	ObjectID  string `json:"oid"`
	Subject   string `json:"sub"`
	TenantID  string `json:"tid"`
	TokenID   string `json:"uti"`
	Version   string `json:"ver"`
}

// serveToken is the client credentials grant of the Microsoft identity
// platform's v2.0 token endpoint.
func (s *Simulation) serveToken(w http.ResponseWriter, r *http.Request, tenant string) {
	if r.Method != http.MethodPost {
		tokenError(w, http.StatusMethodNotAllowed, "invalid_request", 900561, "The endpoint only accepts POST requests.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := r.ParseForm(); err != nil {
		tokenError(w, http.StatusBadRequest, "invalid_request", 900144, "The request body must contain a form.")
		return
	}
	form := r.PostForm
	switch {
	case !strings.EqualFold(tenant, s.cfg.tenant):
		tokenError(w, http.StatusBadRequest, "invalid_request", 90002, fmt.Sprintf("Tenant '%s' not found.", tenant))
	case form.Get("grant_type") == "":
		tokenError(w, http.StatusBadRequest, "invalid_request", 900144, "The request body must contain the following parameter: 'grant_type'.")
	case form.Get("grant_type") != "client_credentials":
		tokenError(w, http.StatusBadRequest, "unsupported_grant_type", 70003, "The app requested an unsupported grant type; the simulation supports client_credentials.")
	case form.Get("client_id") != s.cfg.clientID:
		tokenError(w, http.StatusBadRequest, "unauthorized_client", 700016, fmt.Sprintf("Application with identifier '%s' was not found in the directory.", form.Get("client_id")))
	case form.Get("client_secret") != s.cfg.clientSecret:
		tokenError(w, http.StatusUnauthorized, "invalid_client", 7000215, "Invalid client secret provided.")
	case !strings.HasSuffix(form.Get("scope"), "/.default") || strings.Contains(form.Get("scope"), " "):
		tokenError(w, http.StatusBadRequest, "invalid_scope", 1002012, fmt.Sprintf("The provided value for scope %s is not valid. Client credential flows must have a scope value with /.default suffixed to the resource identifier (application ID URI).", form.Get("scope")))
	default:
		token := s.issueToken(strings.TrimSuffix(form.Get("scope"), "/.default"))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store, no-cache")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token_type": "Bearer", "expires_in": int(tokenLifetime.Seconds()) - 1, "ext_expires_in": int(tokenLifetime.Seconds()) - 1, "access_token": token,
		})
	}
}

func tokenError(w http.ResponseWriter, status int, code string, number int, description string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": code, "error_description": fmt.Sprintf("AADSTS%d: %s", number, description), "error_codes": []int{number},
		"timestamp": time.Now().UTC().Format("2006-01-02 15:04:05Z"), "trace_id": newUUID(), "correlation_id": newUUID(),
		"error_uri": fmt.Sprintf("https://login.microsoftonline.com/error?code=%d", number),
	})
}

// issueToken signs an access token for the audience with HS256 and the
// subscription's signing key. Real tokens are RS256; clients read the claims
// without checking the signature.
func (s *Simulation) issueToken(audience string) string {
	now := s.now()
	issuer := "https://sts.windows.net/" + s.cfg.tenant + "/"
	claims := tokenClaims{
		Audience: audience, Issuer: issuer, IssuedAt: now.Unix(), NotBefore: now.Unix(), Expires: now.Add(tokenLifetime).Unix(),
		AppID: s.cfg.clientID, AppIDACR: "1", IDP: issuer, IDType: "app", ObjectID: s.servicePrincipalID(), Subject: s.servicePrincipalID(),
		TenantID: s.cfg.tenant, TokenID: newUUID(), Version: "1.0",
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, _ := json.Marshal(claims)
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	return unsigned + "." + s.sign(unsigned)
}

func (s *Simulation) sign(text string) string {
	var key []byte
	s.store.Read(func(sub *subscription) { key = sub.SigningKey })
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(text))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// servicePrincipalID is the object id of the client's service principal:
// a version 4 UUID derived from the client id, so it stays the same.
func (s *Simulation) servicePrincipalID() string {
	b := sha256.Sum256([]byte("service-principal:" + s.cfg.clientID))
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// authenticate checks the bearer token of a Resource Manager request.
func (s *Simulation) authenticate(w http.ResponseWriter, r *http.Request) *armError {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer authorization_uri="%s/%s", error="invalid_token", error_description="The authentication failed because of missing 'Authorization' header."`, baseURL(r), s.cfg.tenant))
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return &armError{http.StatusUnauthorized, "AuthenticationFailed", "Authentication failed. The 'Authorization' header is missing."}
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || !hmac.Equal([]byte(s.sign(parts[0]+"."+parts[1])), []byte(parts[2])) {
		return &armError{http.StatusUnauthorized, "InvalidAuthenticationToken", "The access token is invalid."}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	var claims tokenClaims
	if err == nil {
		err = json.Unmarshal(raw, &claims)
	}
	if err != nil {
		return &armError{http.StatusUnauthorized, "InvalidAuthenticationToken", "The access token is invalid."}
	}
	if now := s.now(); now.Unix() >= claims.Expires {
		return &armError{http.StatusUnauthorized, "ExpiredAuthenticationToken", fmt.Sprintf("The access token expiry UTC time '%s' is earlier than current UTC time '%s'.", time.Unix(claims.Expires, 0).UTC().Format(time.RFC1123), now.UTC().Format(time.RFC1123))}
	}
	if strings.TrimSuffix(claims.Audience, "/") == graphResource {
		return &armError{http.StatusUnauthorized, "InvalidAuthenticationTokenAudience", fmt.Sprintf("The access token has been obtained for wrong audience or resource '%s'.", claims.Audience)}
	}
	w.Header().Del("WWW-Authenticate")
	return nil
}

// serveCloudMetadata is the Resource Manager metadata document that clients
// of a custom cloud read to find its endpoints: the Terraform provider's
// metadata_host and the Packer plugin's metadata_host. Every endpoint is
// this simulation, at the address the client used.
func (s *Simulation) serveCloudMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := checkAPIVersion(r); err != nil {
		writeReply(w, errorReply(err))
		return
	}
	base := baseURL(r)
	doc := map[string]any{
		"name":   "AzureStagelySim",
		"portal": base,
		"authentication": map[string]any{
			"loginEndpoint": base, "audiences": []string{base}, "tenant": "common", "identityProvider": "AAD",
		},
		"resourceManager":          base + "/",
		"microsoftGraphResourceId": graphResource + "/",
		"graph":                    "https://graph.windows.net/",
		"graphAudience":            "https://graph.windows.net/",
		"suffixes": map[string]string{
			"acrLoginServer": s.cfg.registry.domain, "keyVaultDns": "vault.azure.net", "storage": "core.windows.net",
		},
	}
	var body any = doc
	if r.URL.Query().Get("api-version") < "2022-09-01" {
		body = []any{doc}
	}
	writeReply(w, okReply(body))
}
