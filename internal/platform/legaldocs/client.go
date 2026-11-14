// Package legaldocs reads a versioned legal document and the tenants' acceptances of it from
// auth-api, which owns them (owner 2026-11-14: the Autopilot auto-publish terms are an auth-api
// LegalDocument shown on the accounts site's /legal pages).
//
// Answers are cached for the configured TTL. When auth-api cannot be reached the last known answer
// is kept and returned; with no last known answer the call fails, and the caller must treat that
// as unknown (the auto-publish gate then never publishes and never cancels on it).
package legaldocs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ErrNotAcceptable is auth-api refusing a version that is neither in force nor upcoming (a new
// version was published after the page was loaded).
var ErrNotAcceptable = errors.New("this terms version can no longer be accepted")

// Config is where the documents live and how long answers are kept.
type Config struct {
	// BaseURL is auth-api (for example http://auth-api.auth.svc.cluster.local:4101).
	BaseURL string
	// APIKey is the internal service key for auth-api's /api/v1/s2s routes.
	APIKey string
	// DocType is the auth-api doc_type, MARKETFLOW_AUTOPUBLISH_TERMS.
	DocType string
	// TTL is how long an answer is used before auth-api is asked again (default 5 minutes).
	TTL time.Duration
	// RetryAfter is how long a failed read waits before auth-api is tried again (default 30s);
	// in between the last known answer is used.
	RetryAfter time.Duration
	// HTTP is the client; nil uses a 5 second timeout.
	HTTP *http.Client
}

// Version is one published version.
type Version struct {
	Version     string
	EffectiveAt time.Time
}

// Document is what auth-api has published. Published is false when no version exists yet.
// Current is the version in force; Upcoming is the next one, announced before its date.
type Document struct {
	Published bool
	Current   *Version
	Upcoming  *Version
}

// Acceptance is one acceptance by a tenant.
type Acceptance struct {
	Version    string
	AcceptedAt time.Time
	UserID     string
}

type cached[T any] struct {
	val      T
	ok       bool // val is a real answer
	at       time.Time
	failedAt time.Time
}

// Client reads and records through auth-api. Safe for concurrent use; share one per process.
type Client struct {
	cfg  Config
	http *http.Client
	now  func() time.Time

	mu  sync.Mutex
	doc cached[Document]
	acc map[uuid.UUID]*cached[[]Acceptance]
}

// New builds a client; it returns nil when BaseURL or DocType is empty.
func New(cfg Config) *Client {
	if strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.DocType) == "" {
		return nil
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.TTL <= 0 {
		cfg.TTL = 5 * time.Minute
	}
	if cfg.RetryAfter <= 0 {
		cfg.RetryAfter = 30 * time.Second
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	return &Client{cfg: cfg, http: hc, now: time.Now, acc: map[uuid.UUID]*cached[[]Acceptance]{}}
}

// DocType is the auth-api doc_type this client reads.
func (c *Client) DocType() string { return c.cfg.DocType }

// Document returns the version in force and the upcoming one.
func (c *Client) Document(ctx context.Context) (Document, error) {
	c.mu.Lock()
	entry := c.doc
	c.mu.Unlock()
	now := c.now()
	if entry.ok && now.Sub(entry.at) < c.cfg.TTL {
		return entry.val, nil
	}
	if !entry.failedAt.IsZero() && now.Sub(entry.failedAt) < c.cfg.RetryAfter {
		if entry.ok {
			return entry.val, nil
		}
		return Document{}, errors.New("auth-api legal documents unavailable")
	}
	doc, err := c.fetchDocument(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.doc.failedAt = now
		if c.doc.ok {
			return c.doc.val, nil
		}
		return Document{}, err
	}
	c.doc = cached[Document]{val: doc, ok: true, at: now}
	return doc, nil
}

type versionJSON struct {
	Version     string `json:"version"`
	EffectiveAt string `json:"effective_at"`
}

func (v *versionJSON) parse() *Version {
	if v == nil || v.Version == "" {
		return nil
	}
	at, _ := time.Parse(time.RFC3339, v.EffectiveAt)
	return &Version{Version: v.Version, EffectiveAt: at.UTC()}
}

func (c *Client) fetchDocument(ctx context.Context) (Document, error) {
	u := fmt.Sprintf("%s/api/v1/legal/documents/%s", c.cfg.BaseURL, url.PathEscape(c.cfg.DocType))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Document{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Document{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return Document{Published: false}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return Document{}, fmt.Errorf("auth-api legal document: status %d", resp.StatusCode)
	}
	var body struct {
		Current  *versionJSON `json:"current"`
		Upcoming *versionJSON `json:"upcoming"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Document{}, fmt.Errorf("auth-api legal document: %w", err)
	}
	doc := Document{Current: body.Current.parse(), Upcoming: body.Upcoming.parse()}
	doc.Published = doc.Current != nil || doc.Upcoming != nil
	return doc, nil
}

// Accepted returns the tenant's acceptances of the document, newest first.
func (c *Client) Accepted(ctx context.Context, tenantID uuid.UUID) ([]Acceptance, error) {
	c.mu.Lock()
	entry := c.acc[tenantID]
	var snap cached[[]Acceptance]
	if entry != nil {
		snap = *entry
	}
	c.mu.Unlock()
	now := c.now()
	if snap.ok && now.Sub(snap.at) < c.cfg.TTL {
		return snap.val, nil
	}
	if !snap.failedAt.IsZero() && now.Sub(snap.failedAt) < c.cfg.RetryAfter {
		if snap.ok {
			return snap.val, nil
		}
		return nil, errors.New("auth-api legal acceptances unavailable")
	}
	list, err := c.fetchAccepted(ctx, tenantID)
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.acc[tenantID]
	if cur == nil {
		cur = &cached[[]Acceptance]{}
		c.acc[tenantID] = cur
	}
	if err != nil {
		cur.failedAt = now
		if cur.ok {
			return cur.val, nil
		}
		return nil, err
	}
	*cur = cached[[]Acceptance]{val: list, ok: true, at: now}
	return list, nil
}

type acceptanceJSON struct {
	Version          string `json:"version"`
	AcceptedAt       string `json:"accepted_at"`
	AcceptedByUserID string `json:"accepted_by_user_id"`
}

func (a acceptanceJSON) parse() Acceptance {
	at, _ := time.Parse(time.RFC3339, a.AcceptedAt)
	return Acceptance{Version: a.Version, AcceptedAt: at.UTC(), UserID: a.AcceptedByUserID}
}

func (c *Client) fetchAccepted(ctx context.Context, tenantID uuid.UUID) ([]Acceptance, error) {
	u := fmt.Sprintf("%s/api/v1/s2s/tenants/%s/legal/acceptances?doc_type=%s", c.cfg.BaseURL, tenantID, url.QueryEscape(c.cfg.DocType))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", c.cfg.APIKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth-api legal acceptances: status %d", resp.StatusCode)
	}
	var body struct {
		Acceptances []acceptanceJSON `json:"acceptances"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("auth-api legal acceptances: %w", err)
	}
	out := make([]Acceptance, 0, len(body.Acceptances))
	for _, a := range body.Acceptances {
		out = append(out, a.parse())
	}
	return out, nil
}

// AcceptInput is one acceptance taken from a signed-in user MarketFlow has authorised.
type AcceptInput struct {
	Version   string
	UserID    *uuid.UUID
	IP        string
	UserAgent string
}

// Accept records the acceptance in auth-api and adds it to this process's cache at once.
func (c *Client) Accept(ctx context.Context, tenantID uuid.UUID, in AcceptInput) (Acceptance, error) {
	payload := map[string]any{"doc_type": c.cfg.DocType, "doc_version": in.Version, "ip_address": in.IP, "user_agent": in.UserAgent}
	if in.UserID != nil {
		payload["user_id"] = in.UserID.String()
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Acceptance{}, err
	}
	u := fmt.Sprintf("%s/api/v1/s2s/tenants/%s/legal/acceptances", c.cfg.BaseURL, tenantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return Acceptance{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.cfg.APIKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return Acceptance{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return Acceptance{}, ErrNotAcceptable
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return Acceptance{}, fmt.Errorf("auth-api legal accept: status %d", resp.StatusCode)
	}
	var body acceptanceJSON
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Acceptance{}, fmt.Errorf("auth-api legal accept: %w", err)
	}
	a := body.parse()
	c.mu.Lock()
	if cur := c.acc[tenantID]; cur != nil && cur.ok {
		cur.val = append([]Acceptance{a}, cur.val...)
	} else {
		// Unknown before: drop the entry so the next read asks auth-api for the full list.
		delete(c.acc, tenantID)
	}
	c.mu.Unlock()
	return a, nil
}
