package auth

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strings"
)

type charLU byte

var charLUs [256]charLU

const (
	isSpace charLU = 1 << iota
	isAlphaNum
)

func init() {
	for c := 0; c < 256; c++ {
		charLUs[c] = 0
		if strings.ContainsRune(" \t\r\n", rune(c)) {
			charLUs[c] |= isSpace
		}
		if (rune('a') <= rune(c) && rune(c) <= rune('z')) || (rune('A') <= rune(c) && rune(c) <= rune('Z') || (rune('0') <= rune(c) && rune(c) <= rune('9'))) {
			charLUs[c] |= isAlphaNum
		}
	}
}

// CredsFn is passed to lookup credentials for a given hostname, response is a username and password or empty strings
type CredsFn func(string) (string, string)

// Auth manages authorization requests/responses for http requests
type Auth interface {
	HandleResponse(*http.Response) error
	UpdateRequest(*http.Request) error
}

// Challenge is the extracted contents of the WWW-Authenticate header
type Challenge struct {
	authType string
	params   map[string]string
}

// Handler handles a challenge for a host to return an auth header
type Handler interface {
	ProcessChallenge(Challenge) error
	GenerateAuth() (string, error)
}

// HandlerBuild is used to make a new handler for a specific authType and URL
type HandlerBuild func(client *http.Client, host, user, pass string) Handler

// Opts configures options for NewAuth
type Opts func(*auth)

type auth struct {
	httpClient *http.Client
	credsFn    CredsFn
	hbs        map[string]HandlerBuild       // handler builders based on authType
	hs         map[string]map[string]Handler // handlers based on url and authType
	authTypes  []string
}

// NewAuth creates a new Auth
func NewAuth(opts ...Opts) Auth {
	a := &auth{
		httpClient: &http.Client{},
		credsFn:    DefaultCredsFn,
		hbs:        map[string]HandlerBuild{},
		hs:         map[string]map[string]Handler{},
		authTypes:  []string{},
	}

	for _, opt := range opts {
		opt(a)
	}

	if len(a.authTypes) == 0 {
		a.addDefaultHandlers()
	}

	return a
}

// WithCreds provides a user/pass lookup for a url
func WithCreds(f CredsFn) Opts {
	return func(a *auth) {
		if f != nil {
			a.credsFn = f
		}
	}
}

// WithHTTPClient uses a specific http client with requests
func WithHTTPClient(h *http.Client) Opts {
	return func(a *auth) {
		if h != nil {
			a.httpClient = h
		}
	}
}

// WithHandler includes a handler for a specific auth type
func WithHandler(authType string, hb HandlerBuild) Opts {
	return func(a *auth) {
		lcat := strings.ToLower(authType)
		a.hbs[lcat] = hb
		a.authTypes = append(a.authTypes, lcat)
	}
}

// WithDefaultHandlers includes a Basic and Bearer handler, this is automatically added with "WithHandler" is not called
func WithDefaultHandlers() Opts {
	return func(a *auth) {
		a.addDefaultHandlers()
	}
}

func (a *auth) HandleResponse(resp *http.Response) error {
	/* 	- HandleResponse: parse 401 response, register/update auth method
	   	- Manage handlers in map based on URL's host field
	   	- Parse Www-Authenticate header
	   	- Switch based on scheme (basic/bearer)
	   	  - If handler doesn't exist, create handler
	   	  - Call handler specific HandleResponse
	*/
	// verify response is an access denied
	if resp.StatusCode != http.StatusUnauthorized {
		return ErrUnsupported
	}

	// identify host for the request
	host := resp.Request.URL.Host
	// parse WWW-Authenticate header
	cl, err := ParseAuthHeaders(resp.Header.Values("WWW-Authenticate"))
	if err != nil {
		return err
	}
	goodChallenge := false
	for _, c := range cl {
		if _, ok := a.hbs[c.authType]; !ok {
			fmt.Fprintf(os.Stderr, "Warning: unsupported auth type seen in challenge: %s\n", c.authType)
			continue
		}
		if _, ok := a.hs[host]; !ok {
			a.hs[host] = map[string]Handler{}
		}
		if _, ok := a.hs[host][c.authType]; !ok {
			user, pass := a.credsFn(host)
			h := a.hbs[c.authType](a.httpClient, host, user, pass)
			a.hs[host][c.authType] = h
		}
		err := a.hs[host][c.authType].ProcessChallenge(c)
		if err == nil {
			goodChallenge = true
		} else if err != ErrNoNewChallenge {
			return err
		}
	}
	if goodChallenge == false {
		return ErrNoNewChallenge
	}

	return nil
}

func (a *auth) UpdateRequest(req *http.Request) error {
	/* 	- UpdateRequest:
	   	- Lookup handler, noop if no handler for URL's host
	   	- Call handler updateRequest func, add returned header
	*/
	host := req.URL.Host
	if a.hs[host] == nil {
		return nil
	}
	for _, at := range a.authTypes {
		if a.hs[host][at] != nil {
			ah, err := a.hs[host][at].GenerateAuth()
			if err != nil {
				continue
			}
			req.Header.Set("Authorization", ah)
			break
		}
	}
	return nil
}

func (a *auth) addDefaultHandlers() {
	if _, ok := a.hbs["basic"]; !ok {
		a.hbs["basic"] = NewBasicHandler
		a.authTypes = append(a.authTypes, "basic")
	}
}

// DefaultCredsFn is used to return no credentials when auth is not configured with a CredsFn
// This avoids the need to check for nil pointers
func DefaultCredsFn(h string) (string, string) {
	return "", ""
}

// ParseAuthHeaders extracts the scheme and realm from WWW-Authenticate headers
func ParseAuthHeaders(ahl []string) ([]Challenge, error) {
	var cl []Challenge
	for _, ah := range ahl {
		c, err := ParseAuthHeader(ah)
		if err != nil {
			return nil, fmt.Errorf("Failed to parse challenge header: %s", ah)
		}
		cl = append(cl, c...)
	}
	return cl, nil
}

// ParseAuthHeader parses a single header line for WWW-Authenticate
// Example values:
// Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:samalba/my-app:pull,push"
// Basic realm="GitHub Package Registry"
func ParseAuthHeader(ah string) ([]Challenge, error) {
	var cl []Challenge
	var c *Challenge
	var eb, atb, kb, vb []byte // eb is element bytes, atb auth type, kb key, vb value
	state := "string"

	for _, b := range []byte(ah) {
		switch state {
		case "string":
			if len(eb) == 0 {
				// beginning of string
				if b == '"' { // TODO: Invalid?
					state = "quoted"
				} else if charLUs[b]&isAlphaNum != 0 {
					// read any alphanum
					eb = append(eb, b)
				} else if charLUs[b]&isSpace != 0 {
					// ignore leading whitespace
				} else {
					// unknown leading char
					return nil, ErrParseFailure
				}
			} else {
				if charLUs[b]&isAlphaNum != 0 {
					// read any alphanum
					eb = append(eb, b)
				} else if b == '=' && len(atb) > 0 {
					// equals when authtype is defined makes this a key
					kb = eb
					eb = []byte{}
					state = "value"
				} else if charLUs[b]&isSpace != 0 {
					// space ends the element
					atb = eb
					eb = []byte{}
					c = &Challenge{authType: strings.ToLower(string(atb)), params: map[string]string{}}
					cl = append(cl, *c)
				} else {
					// unknown char
					return nil, ErrParseFailure
				}
			}

		case "value":
			if charLUs[b]&isAlphaNum != 0 {
				// read any alphanum
				vb = append(vb, b)
			} else if b == '"' && len(vb) == 0 {
				// quoted value
				state = "quoted"
			} else if charLUs[b]&isSpace != 0 || b == ',' {
				// space or comma ends the value
				c.params[strings.ToLower(string(kb))] = string(vb)
				kb = []byte{}
				vb = []byte{}
				if b == ',' {
					state = "string"
				} else {
					state = "endvalue"
				}
			} else {
				// unknown char
				return nil, ErrParseFailure
			}

		case "quoted":
			if b == '"' {
				// end quoted string
				c.params[strings.ToLower(string(kb))] = string(vb)
				kb = []byte{}
				vb = []byte{}
				state = "endvalue"
			} else if b == '\\' {
				state = "escape"
			} else {
				// all other bytes in a quoted string are taken as-is
				vb = append(vb, b)
			}

		case "endvalue":
			if charLUs[b]&isSpace != 0 {
				// ignore leading whitespace
			} else if b == ',' {
				// expect a comma separator, return to start of a string
				state = "string"
			} else {
				// unknown char
				return nil, ErrParseFailure
			}

		case "escape":
			vb = append(vb, b)
			state = "quoted"

		default:
			return nil, ErrParseFailure
		}
	}

	// process any content left at end of string
	switch state {
	case "value":
		if len(vb) != 0 {
			c.params[strings.ToLower(string(kb))] = string(vb)
		}
	}

	return cl, nil
}

// BasicHandler supports Basic auth type requests
type BasicHandler struct {
	realm, user, pass string
}

// NewBasicHandler creates a new BasicHandler
func NewBasicHandler(client *http.Client, host, user, pass string) Handler {
	return &BasicHandler{
		realm: "",
		user:  user,
		pass:  pass,
	}
}

// ProcessChallenge for BasicHandler is a noop
func (b *BasicHandler) ProcessChallenge(c Challenge) error {
	if _, ok := c.params["realm"]; !ok {
		return ErrInvalidChallenge
	}
	if b.realm != c.params["realm"] {
		b.realm = c.params["realm"]
		return nil
	}
	return ErrNoNewChallenge
}

// GenerateAuth for BasicHandler generates base64 encoded user/pass for a host
func (b *BasicHandler) GenerateAuth() (string, error) {
	if b.user == "" || b.pass == "" {
		return "", ErrNotFound
	}
	auth := base64.StdEncoding.EncodeToString([]byte(b.user + ":" + b.pass))
	return fmt.Sprintf("Basic %s", auth), nil
}

// BearerHandler supports Bearer auth type requests
type BearerHandler struct {
	client                     *http.Client
	realm, service, user, pass string
	scopes                     []string
}

// NewBearerHandler creates a new BearerHandler
func NewBearerHandler(client *http.Client, host, user, pass string) Handler {
	return &BearerHandler{
		client:  client,
		user:    user,
		pass:    pass,
		realm:   "",
		service: "",
		scopes:  []string{},
	}
}

// ProcessChallenge for BasicHandler is a noop
// Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:samalba/my-app:pull,push"
func (b *BearerHandler) ProcessChallenge(c Challenge) error {
	if _, ok := c.params["realm"]; !ok {
		return ErrInvalidChallenge
	}
	if _, ok := c.params["service"]; !ok {
		return ErrInvalidChallenge
	}
	if _, ok := c.params["scope"]; !ok {
		return ErrInvalidChallenge
	}

	existingScope := b.scopeExists(c.params["scope"])

	if b.realm == c.params["realm"] && b.service == c.params["service"] && existingScope {
		return ErrNoNewChallenge
	}

	if b.realm == "" {
		b.realm = c.params["realm"]
	} else if b.realm != c.params["realm"] {
		return ErrInvalidChallenge
	}
	if b.service == "" {
		b.service = c.params["service"]
	} else if b.service != c.params["service"] {
		return ErrInvalidChallenge
	}
	if !existingScope {
		b.scopes = append(b.scopes, c.params["scope"])
	}

	// TODO: delete any scope specific token

	return nil
}

// GenerateAuth for BasicHandler generates base64 encoded user/pass for a host
func (b *BearerHandler) GenerateAuth() (string, error) {
	if b.user == "" || b.pass == "" {
		// do anonymous request
	}

	return fmt.Sprintf("Bearer %s", ""), nil
}

// check if the scope already exists within the list of scopes
func (b *BearerHandler) scopeExists(search string) bool {
	for _, scope := range b.scopes {
		if scope == search {
			return true
		}
	}
	return false
}

/*
- (auth) getCreds(url) (string, string, error) returns user/pass for a url, empty if anonymous or unavailable
- Basic HandleResponse
  - Verify scheme is basic
  - Compare encoded cred against last cred, if they match, "unchanged" error
- Basic UpdateRequest:
  - base64 encode user/pass and return
- Bearer HandleResponse:
  - Verify scheme is bearer
  - Compare realm and service
  - Compare scope, add scope if needed
  - Check current token expiration time
  - If nothing changed, error
- Bearer UpdateRequest:
  - Request refresh token if unset
  - Refresh token if needed
  - Parse returned token
  - return token
*/
