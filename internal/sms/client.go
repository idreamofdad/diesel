// Package sms wires inbound and outbound text messages over Twilio into a
// hub.Hub. Inbound is poll-based — every PollSeconds we hit Twilio's
// Messages API and dedupe by message SID — because exposing a public
// webhook from a desktop app is a different shape of problem (port
// forwarding, ngrok, TLS) than every other Diesel integration.
//
// Outbound replies are wired through a hub subscription: the manager
// remembers the most-recent SMS sender, and when the next assistant
// reply lands with origin "sms" it POSTs the text back to that number.
// This deliberately shares the single hub conversation with the desktop
// — interleaving is the documented behavior.
package sms

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"diesel/internal/tracing"
	"diesel/internal/util"
)

// twilioBase is the canonical REST host. Kept as a var so tests can
// substitute an httptest server URL.
var twilioBase = "https://api.twilio.com"

// Client talks to the Twilio Messages API. Auth is HTTP Basic with
// AccountSID:AuthToken — the same shape Twilio's official helper
// libraries use.
type Client struct {
	AccountSID string
	AuthToken  string
	// HTTP is the http.Client used for every request. Tests override
	// this to point at a fake; nil falls back to a traced default with
	// a sensible timeout.
	HTTP *http.Client
	// Base overrides the API host. Empty = production. Tests set this
	// to an httptest.Server URL.
	Base string
}

// Message is a single row from Twilio's Messages.json list endpoint.
// Only the fields the poller actually reads are decoded; Twilio sends
// a fat object (price, status, segments, …) we don't need.
type Message struct {
	SID       string `json:"sid"`
	From      string `json:"from"`
	To        string `json:"to"`
	Body      string `json:"body"`
	Direction string `json:"direction"`
	// DateSent comes back as RFC1123 with "+0000" rather than "GMT"
	// — Twilio's docs spell this format out. Decoded as a string and
	// parsed lazily so a format change on the server side doesn't
	// crash unmarshalling.
	DateSent string `json:"date_sent"`
}

// ParsedDateSent returns DateSent as a time.Time, falling back to the
// zero value when the string is empty or unparseable. Twilio uses
// RFC1123Z ("Mon, 02 Jan 2006 15:04:05 +0000") for this field.
func (m Message) ParsedDateSent() time.Time {
	if m.DateSent == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC1123Z, m.DateSent); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC1123, m.DateSent); err == nil {
		return t
	}
	return time.Time{}
}

// listResponse is the JSON envelope Twilio wraps the messages list in.
// next_page_uri is honored only when set; otherwise the poller stops.
type listResponse struct {
	Messages    []Message `json:"messages"`
	NextPageURI string    `json:"next_page_uri"`
}

// client returns the configured http.Client or a traced default. The
// timeout is generous because Twilio's API can be sluggish under load
// and we don't want a transient stall to spuriously stop the poller.
func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return tracing.HTTPClient(30 * time.Second)
}

// base returns the API host, falling back to the production endpoint.
func (c *Client) base() string {
	if c.Base != "" {
		return c.Base
	}
	return twilioBase
}

// ListInbound returns inbound messages addressed to `to` with DateSent
// at or after `since`. Twilio's API supports `DateSent>=` as a query
// param name — the operator is literally part of the key — and the
// caller is expected to handle clock skew + dedupe by SID upstream
// since the time filter has second-level granularity but the polling
// cadence is often finer.
//
// `to` is the Twilio number we own (i.e. the inbox). Passing an empty
// `to` returns every inbound message on the account, which is rarely
// what callers want; the manager always sets it.
func (c *Client) ListInbound(ctx context.Context, to string, since time.Time) ([]Message, error) {
	ctx, span := tracing.StartSpan(ctx, "sms.list")
	defer span.End()

	if c.AccountSID == "" || c.AuthToken == "" {
		return nil, fmt.Errorf("twilio: missing credentials")
	}
	q := url.Values{}
	if to != "" {
		// To-filter narrows the result to messages addressed to our
		// number. Twilio's API has no direct Direction filter, so we
		// also drop non-inbound rows client-side below — that's what
		// keeps our own outgoing replies from re-entering the loop.
		q.Set("To", to)
	}
	// PageSize=50 — generous enough to cover a long poll interval, small
	// enough to bound the response size when the inbox is busy. Pagination
	// is honored via NextPageURI when needed.
	q.Set("PageSize", "50")
	if !since.IsZero() {
		q.Set("DateSent>=", since.UTC().Format(time.RFC3339))
	}

	endpoint := fmt.Sprintf("%s/2010-04-01/Accounts/%s/Messages.json?%s",
		c.base(), url.PathEscape(c.AccountSID), q.Encode())

	var all []Message
	// Cap pagination at a handful of pages so a runaway loop can't hang
	// the poller indefinitely. In practice the first page is always
	// enough at a 10 s cadence.
	for i := 0; i < 5 && endpoint != ""; i++ {
		page, next, err := c.fetchPage(ctx, endpoint)
		if err != nil {
			return nil, err
		}
		for _, m := range page {
			if strings.EqualFold(m.Direction, "inbound") {
				all = append(all, m)
			}
		}
		if next == "" {
			break
		}
		// Twilio returns NextPageURI as a server-relative path.
		endpoint = c.base() + next
	}
	return all, nil
}

// fetchPage performs one GET against the Messages list endpoint and
// returns the decoded page plus the next-page URI (if any).
func (c *Client) fetchPage(ctx context.Context, endpoint string) ([]Message, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, "", err
	}
	req.SetBasicAuth(c.AccountSID, c.AuthToken)
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", util.HTTPStatusError(resp, 512)
	}
	var payload listResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, "", err
	}
	return payload.Messages, payload.NextPageURI, nil
}

// Send posts a single outbound SMS. Returns the created Message (with
// its SID) on success. The "from" number must be one provisioned on the
// account; "to" is the recipient in E.164.
func (c *Client) Send(ctx context.Context, from, to, body string) (Message, error) {
	ctx, span := tracing.StartSpan(ctx, "sms.send")
	defer span.End()

	if c.AccountSID == "" || c.AuthToken == "" {
		return Message{}, fmt.Errorf("twilio: missing credentials")
	}
	if strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" {
		return Message{}, fmt.Errorf("twilio: missing from/to")
	}
	form := url.Values{}
	form.Set("From", from)
	form.Set("To", to)
	form.Set("Body", body)

	endpoint := fmt.Sprintf("%s/2010-04-01/Accounts/%s/Messages.json",
		c.base(), url.PathEscape(c.AccountSID))
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Message{}, err
	}
	req.SetBasicAuth(c.AccountSID, c.AuthToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.client().Do(req)
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()
	// Twilio returns 201 on create. Anything else is a failure.
	if resp.StatusCode/100 != 2 {
		return Message{}, util.HTTPStatusError(resp, 512)
	}
	var msg Message
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		return Message{}, err
	}
	return msg, nil
}

// Ping verifies the credentials by fetching the account record. Used by
// the Settings dialog's Test button so the user gets a useful error
// without having to send a real SMS.
func (c *Client) Ping(ctx context.Context) error {
	if c.AccountSID == "" || c.AuthToken == "" {
		return fmt.Errorf("twilio: missing credentials")
	}
	endpoint := fmt.Sprintf("%s/2010-04-01/Accounts/%s.json",
		c.base(), url.PathEscape(c.AccountSID))
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.AccountSID, c.AuthToken)
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return util.HTTPStatusError(resp, 512)
	}
	return nil
}
