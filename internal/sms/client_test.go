package sms

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClient_ListInbound covers the happy path: the client sends Basic
// auth, the URL carries the To and DateSent>= filters, the response is
// decoded, and outbound rows are dropped client-side because Twilio
// doesn't expose a Direction filter.
func TestClient_ListInbound(t *testing.T) {
	var (
		gotPath, gotAuth string
		gotQuery         map[string][]string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.Query()
		_ = json.NewEncoder(w).Encode(listResponse{
			Messages: []Message{
				{SID: "SM1", From: "+15551111111", To: "+15550000000", Body: "hello", Direction: "inbound", DateSent: "Mon, 01 Jan 2024 10:00:00 +0000"},
				{SID: "SM2", From: "+15550000000", To: "+15551111111", Body: "echo", Direction: "outbound-api", DateSent: "Mon, 01 Jan 2024 10:00:30 +0000"},
				{SID: "SM3", From: "+15552222222", To: "+15550000000", Body: "yo", Direction: "inbound", DateSent: "Mon, 01 Jan 2024 10:01:00 +0000"},
			},
		})
	}))
	defer srv.Close()

	c := &Client{AccountSID: "ACtest", AuthToken: "secret", Base: srv.URL}
	since := time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC)
	msgs, err := c.ListInbound(context.Background(), "+15550000000", since)
	require.NoError(t, err)

	assert.Equal(t, "/2010-04-01/Accounts/ACtest/Messages.json", gotPath)
	assert.True(t, strings.HasPrefix(gotAuth, "Basic "), "expected basic auth header, got %q", gotAuth)
	assert.Equal(t, []string{"+15550000000"}, gotQuery["To"])
	require.NotEmpty(t, gotQuery["DateSent>="], "DateSent>= filter must be set")
	assert.Equal(t, "2024-01-01T09:00:00Z", gotQuery["DateSent>="][0])

	require.Len(t, msgs, 2, "outbound row must be filtered out client-side")
	assert.Equal(t, "SM1", msgs[0].SID)
	assert.Equal(t, "SM3", msgs[1].SID)
	// Date parsing should hand back a usable time.
	assert.Equal(t, time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC), msgs[0].ParsedDateSent().UTC())
}

// TestClient_ListInbound_FollowsPagination — when next_page_uri is set
// the client follows it until the chain ends. Capped at a few pages so
// a runaway server can't hang the poll loop.
func TestClient_ListInbound_FollowsPagination(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch hits {
		case 1:
			_ = json.NewEncoder(w).Encode(listResponse{
				Messages:    []Message{{SID: "SM1", Direction: "inbound"}},
				NextPageURI: "/p2",
			})
		case 2:
			_ = json.NewEncoder(w).Encode(listResponse{
				Messages: []Message{{SID: "SM2", Direction: "inbound"}},
			})
		}
	}))
	defer srv.Close()

	c := &Client{AccountSID: "ACtest", AuthToken: "secret", Base: srv.URL}
	msgs, err := c.ListInbound(context.Background(), "", time.Time{})
	require.NoError(t, err)
	assert.Equal(t, 2, hits, "should have followed pagination once")
	require.Len(t, msgs, 2)
	assert.Equal(t, "SM2", msgs[1].SID)
}

// TestClient_Send round-trips a single outbound message through the
// Twilio Messages endpoint. The form encoding is the part we want to
// pin down — Twilio's helper libraries POST application/x-www-form-
// urlencoded with capital-cased keys.
func TestClient_Send(t *testing.T) {
	var gotForm string
	var gotCT, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotForm = string(body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Message{SID: "SMnew"})
	}))
	defer srv.Close()

	c := &Client{AccountSID: "ACtest", AuthToken: "secret", Base: srv.URL}
	msg, err := c.Send(context.Background(), "+15550000000", "+15551111111", "hello there")
	require.NoError(t, err)
	assert.Equal(t, "SMnew", msg.SID)
	assert.True(t, strings.HasPrefix(gotAuth, "Basic "))
	assert.Equal(t, "application/x-www-form-urlencoded", gotCT)
	assert.Contains(t, gotForm, "From=%2B15550000000")
	assert.Contains(t, gotForm, "To=%2B15551111111")
	assert.Contains(t, gotForm, "Body=hello+there")
}

// TestClient_Ping_HappyPath — the dialog's Test button hits the
// account record endpoint; a 200 means the credentials work.
func TestClient_Ping_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/2010-04-01/Accounts/ACtest.json", r.URL.Path)
		_, _ = w.Write([]byte(`{"sid":"ACtest"}`))
	}))
	defer srv.Close()
	c := &Client{AccountSID: "ACtest", AuthToken: "secret", Base: srv.URL}
	assert.NoError(t, c.Ping(context.Background()))
}

// TestClient_Ping_BadCreds — a 401 from Twilio is surfaced as a
// human-readable error the dialog can show next to the Test button.
func TestClient_Ping_BadCreds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Authenticate"}`))
	}))
	defer srv.Close()
	c := &Client{AccountSID: "ACtest", AuthToken: "wrong", Base: srv.URL}
	err := c.Ping(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
}

// TestClient_MissingCreds — both list and send refuse without
// credentials so an empty settings file can't accidentally hit the
// /Accounts// path (which Twilio would 404 obscurely).
func TestClient_MissingCreds(t *testing.T) {
	c := &Client{}
	_, err := c.ListInbound(context.Background(), "+15550000000", time.Time{})
	require.Error(t, err)
	_, err = c.Send(context.Background(), "+15550000000", "+15551111111", "hi")
	require.Error(t, err)
}
