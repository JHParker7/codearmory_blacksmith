package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// DefaultTimeout, DefaultRetries and DefaultBackoff are what a client gets when
// it says nothing.
//
// The control plane is on the always-on side and this host is the intermittent
// one, but the network between them is not guaranteed either — so a handful of
// retries, doubling, is the floor rather than an optimisation.
const (
	DefaultTimeout = 30 * time.Second
	DefaultRetries = 3
	DefaultBackoff = 500 * time.Millisecond
)

// Credential supplies the bearer token for a host, and renews it when the host
// says it has expired.
//
// RENEWAL IS DRIVEN BY THE SERVER'S ANSWER, not by a clock. The server is the
// authority on whether a session is still good, and a timer would need a TTL
// that is neither published nor stable.
type Credential interface {
	// Token returns the current bearer.
	Token(ctx context.Context) (string, error)

	// Renew discards the current session and obtains another. A Credential that
	// cannot renew — a static token from configuration — returns an error, and
	// the caller reports the original denial instead.
	Renew(ctx context.Context) (string, error)
}

// Static is a Credential that never changes: a token read from configuration.
type Static string

func (s Static) Token(context.Context) (string, error) { return string(s), nil }

func (s Static) Renew(context.Context) (string, error) {
	return "", errors.New("this credential is a fixed token and cannot be renewed")
}

// Header is one extra request header. An empty value is not sent, so a caller
// can pass a conditional header it may not have a value for without branching.
type Header struct{ Name, Value string }

// Request is one call.
type Request struct {
	Method string

	// Path is appended to the client's base URL, prefix and all.
	Path string

	// Body is encoded as JSON when non-nil; Out is decoded into when non-nil.
	Body any
	Out  any

	Headers []Header

	// OnResponse sees the response headers before the body is decoded, which is
	// what the conditional-write path needs in order to read the ETag.
	OnResponse func(http.Header)
}

// Client talks to one host with one credential.
type Client struct {
	// Name is what this host is called in an error, so a message about a missing
	// base URL can say which one.
	Name string

	BaseURL string
	Cred    Credential

	HTTP       *http.Client
	MaxRetries int
	Backoff    time.Duration
}

// New builds a client with the defaults filled in.
func New(name, baseURL string, cred Credential) *Client {
	return &Client{
		Name:       name,
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Cred:       cred,
		HTTP:       &http.Client{Timeout: DefaultTimeout},
		MaxRetries: DefaultRetries,
		Backoff:    DefaultBackoff,
	}
}

// Configured reports whether this client has a host to talk to.
func (c *Client) Configured() bool { return c != nil && c.BaseURL != "" }

// Do issues one request, retrying transient failures and renewing the
// credential once if the host says the session has expired.
func (c *Client) Do(ctx context.Context, req Request) error {
	// A HOST THAT IS NOT CONFIGURED SAYS SO. Issuing a request against "" reports
	// a URL parse failure, which sends whoever reads it looking for a malformed
	// address rather than a missing one — and on a standalone host the honest
	// answer is that the feature has no local equivalent at all.
	if !c.Configured() {
		return fmt.Errorf("%s %s: no %s is configured on this host", req.Method, req.Path, c.hostName())
	}

	// A CLIENT WITH NO CREDENTIAL IS A WIRING MISTAKE, and it must say so rather
	// than panicking. A nil dereference here surfaces as a stack trace from
	// whichever agent happened to call first, which names the caller and not the
	// thing that was never configured.
	if c.Cred == nil {
		return fmt.Errorf("%s %s: no credential is configured for the %s",
			req.Method, req.Path, c.hostName())
	}

	token, err := c.Cred.Token(ctx)
	if err != nil {
		return fmt.Errorf("%s credential: %w", c.hostName(), err)
	}

	err = c.withRetries(ctx, token, req)
	if !Denied(err) {
		return err
	}

	// RENEW ONCE, no more. A genuine permissions failure must surface as a denial
	// rather than becoming a login loop against a rate-limited endpoint.
	fresh, rerr := c.Cred.Renew(ctx)
	if rerr != nil {
		return err
	}
	return c.withRetries(ctx, fresh, req)
}

func (c *Client) hostName() string {
	if c.Name == "" {
		return "host"
	}
	return c.Name
}

func (c *Client) withRetries(ctx context.Context, token string, req Request) error {
	var last error
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(c.Backoff * time.Duration(1<<(attempt-1))):
			}
		}
		err := c.once(ctx, token, req)
		if err == nil {
			return nil
		}
		if !Transient(err) {
			return err
		}
		last = err
	}
	return last
}

func (c *Client) once(ctx context.Context, token string, req Request) error {
	var body io.Reader
	if req.Body != nil {
		encoded, err := json.Marshal(req.Body)
		if err != nil {
			return fmt.Errorf("encode %s %s: %w", req.Method, req.Path, err)
		}
		body = bytes.NewReader(encoded)
	}

	r, err := http.NewRequestWithContext(ctx, req.Method, c.BaseURL+req.Path, body)
	if err != nil {
		return fmt.Errorf("%s %s: %w", req.Method, req.Path, err)
	}

	// CARRY THE TRACE ACROSS THE CALL. Without this every service blacksmith talks
	// to starts its own unrelated root span: forge, tickets and the gatekeeper all
	// appear in the collector and none of them appears CONNECTED to the agent that
	// called them. The architecture view then shows three services and no
	// blacksmith, because a service with no edges is not part of any flow — which
	// is exactly backwards, since the agent drives all three.
	//
	// It also makes one ticket's work a single trace: stage → sandbox → forge →
	// gatekeeper, in one timeline, which is the view worth having when a stage is
	// slow and it is not obvious which hop is responsible.
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(r.Header))

	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Accept", "application/json")
	if req.Body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	for _, h := range req.Headers {
		if h.Value != "" {
			r.Header.Set(h.Name, h.Value)
		}
	}

	resp, err := c.HTTP.Do(r)
	if err != nil {
		return fmt.Errorf("%s %s: %w: %v", req.Method, req.Path, ErrTransient, err)
	}
	defer resp.Body.Close()

	if req.OnResponse != nil {
		req.OnResponse(resp.Header)
	}

	if err := classify(req, resp); err != nil {
		return err
	}
	if req.Out == nil {
		return nil
	}

	// 204 MEANS THERE IS NOTHING TO DECODE, and that is a success. A store that
	// answers a write with no content has done the write; failing here would turn
	// a completed move into an error and the caller would retry something that
	// already landed.
	//
	// Only 204. An EMPTY 200 still fails, deliberately — that is the shape a
	// misrouted request takes, and quietly handing back a zero-valued struct is
	// how "no work today" gets reported instead of a misconfiguration.
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}

	if err := json.NewDecoder(resp.Body).Decode(req.Out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", req.Method, req.Path, err)
	}
	return nil
}

// classify turns a status code into one of the error classes above.
func classify(req Request, resp *http.Response) error {
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%s %s: %w: %s", req.Method, req.Path, ErrDenied, Snippet(resp.Body))

	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%s %s: %w", req.Method, req.Path, ErrNotFound)

	case resp.StatusCode == http.StatusConflict, resp.StatusCode == http.StatusPreconditionFailed:
		// THE SERVER'S OWN WORDS ARE KEPT.
		//
		// This class used to read "already claimed", which is written for the ticket
		// race and is actively misleading anywhere else: forge returns 409 for
		// "lease is expired, not ready", and that rendered as "POST /executions:
		// already claimed" — a phrase describing a concept forge does not have. It
		// cost a search through the wrong service for a claim mechanism that was
		// never there, while the real message sat in the body being discarded.
		return fmt.Errorf("%s %s: %w: %s", req.Method, req.Path, ErrConflict, Snippet(resp.Body))

	case resp.StatusCode >= 500:
		return fmt.Errorf("%s %s: %w: %s: %s", req.Method, req.Path, ErrTransient, resp.Status, Snippet(resp.Body))

	case resp.StatusCode >= 400:
		// A 4xx that is none of the above is a request this code got wrong, and
		// retrying would repeat it. The body carries the reason.
		return fmt.Errorf("%s %s: %s: %s", req.Method, req.Path, resp.Status, Snippet(resp.Body))
	}
	return nil
}

// SnippetBytes is how much of an error body is kept.
const SnippetBytes = 512

// Snippet bounds an error body: these go into log lines, and a failing service
// can return a very large one.
//
// IT CUTS ON A RUNE BOUNDARY, NOT A BYTE ONE. This is the shape of a mistake
// that bricked a run once already — a hook counted a subject in bytes while the
// text was clipped by runes, and every specification section blocked with a
// message pointing squarely at the network. Slicing UTF-8 at an arbitrary byte
// leaves half a character, and this department's prose is full of em dashes,
// three bytes each, so the odds are not small. A log line that is not valid text
// is a line a reader distrusts entirely.
//
// The empty and unreadable cases are NAMED rather than rendered as nothing: a
// message ending in a bare colon reads as though it were itself truncated, and
// "the server said nothing" is information.
func Snippet(r io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(r, SnippetBytes+1))
	if err != nil {
		return "<unreadable body>"
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return "<empty body>"
	}
	if len(s) <= SnippetBytes {
		return s
	}
	// Back off to the last rune that fits whole.
	cut := SnippetBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
