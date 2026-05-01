// Package graph is a thin Microsoft Graph client for Dispatch.
// Adapted from the phishing filter's graph.go — same auth pattern (client credentials,
// token caching), scoped down to what Dispatch needs.
package graph

import ( //nolint:gci
	"regexp"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type Config struct {
	TenantID     string `json:"tenantId"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

type Client struct {
	cfg         Config
	http        *http.Client
	accessToken string
	tokenExpiry time.Time
	mu          sync.Mutex
	// sem caps concurrent Graph API calls per Client. Microsoft enforces a
	// per-mailbox-per-app concurrency limit (~4); bursting past it returns
	// 429 ApplicationThrottled. We keep the total in-flight at or below the
	// documented soft cap so our N-worker pool doesn't trip throttling.
	sem chan struct{}
}

// GraphConcurrency is the max concurrent Graph calls one Client allows. Set
// below Microsoft's per-mailbox-per-app limit (docs say 4) with a safety margin.
const GraphConcurrency = 3

func NewClient() (*Client, error) {
	path := os.Getenv("MSGRAPH_CONFIG_PATH")
	if path == "" {
		candidates := []string{
			"../configs/msgraph_config.json",
			"../../configs/msgraph_config.json",
			"/etc/dispatch/msgraph_config.json",
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				path = c
				break
			}
		}
	}
	if path == "" {
		return nil, fmt.Errorf("msgraph_config.json not found (set MSGRAPH_CONFIG_PATH)")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: 60 * time.Second},
		sem:  make(chan struct{}, GraphConcurrency),
	}, nil
}

// acquire/release gate Graph API calls behind a semaphore.
func (c *Client) acquire() { c.sem <- struct{}{} }
func (c *Client) release() { <-c.sem }

// doHTTP wraps c.http.Do with semaphore gating + 429 retry honoring Retry-After.
// Up to 3 attempts on throttle; other errors propagate immediately.
func (c *Client) doHTTP(req *http.Request) (*http.Response, error) {
	c.acquire()
	defer c.release()
	const maxRetries = 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != 429 {
			return resp, nil
		}
		// Throttled. Read + close body, honor Retry-After (seconds), retry.
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		wait := time.Duration(1+attempt*2) * time.Second
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := time.ParseDuration(ra + "s"); err == nil && secs > 0 {
				wait = secs
			}
		}
		time.Sleep(wait)
	}
	// Final attempt — if we get here we've exhausted retries. Try once more
	// and return whatever we get so the caller sees the real error.
	return c.http.Do(req)
}

func (c *Client) token() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.accessToken != "" && time.Now().Add(5*time.Minute).Before(c.tokenExpiry) {
		return c.accessToken, nil
	}
	form := url.Values{}
	form.Set("client_id", c.cfg.ClientID)
	form.Set("client_secret", c.cfg.ClientSecret)
	form.Set("scope", "https://graph.microsoft.com/.default")
	form.Set("grant_type", "client_credentials")
	resp, err := c.http.PostForm(
		fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", c.cfg.TenantID),
		form,
	)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token %d: %s", resp.StatusCode, body)
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("parse token: %w", err)
	}
	c.accessToken = tr.AccessToken
	c.tokenExpiry = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return c.accessToken, nil
}

func (c *Client) do(method, u string, body any, out any) error {
	tok, err := c.token()
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.doHTTP(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, u, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s returned %d: %s", method, u, resp.StatusCode, respBody)
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}
	}
	return nil
}

type EmailAddr struct {
	EmailAddress struct {
		Name    string `json:"name"`
		Address string `json:"address"`
	} `json:"emailAddress"`
}

type MessageBody struct {
	ContentType string `json:"contentType"` // "text" or "html"
	Content     string `json:"content"`
}

type Message struct {
	ID               string      `json:"id"`
	ConversationID   string      `json:"conversationId"`
	Subject          string      `json:"subject"`
	ReceivedDateTime string      `json:"receivedDateTime"`
	Categories       []string    `json:"categories"`
	WebLink          string      `json:"webLink"`
	HasAttachments   bool        `json:"hasAttachments"`
	Body             *MessageBody `json:"body,omitempty"`
	BodyPreview      string      `json:"bodyPreview,omitempty"`
	From             *EmailAddr  `json:"from"`
	Sender           *EmailAddr  `json:"sender"`
	ToRecipients     []EmailAddr `json:"toRecipients,omitempty"`
	CcRecipients     []EmailAddr `json:"ccRecipients,omitempty"`
}

type Attachment struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	Size        int    `json:"size"`
	IsInline    bool   `json:"isInline"`
}

// SenderAddress returns the From email, falling back to Sender if From is missing.
func (m *Message) SenderAddress() string {
	if m.From != nil && m.From.EmailAddress.Address != "" {
		return m.From.EmailAddress.Address
	}
	if m.Sender != nil {
		return m.Sender.EmailAddress.Address
	}
	return ""
}

func (m *Message) SenderName() string {
	if m.From != nil {
		return m.From.EmailAddress.Name
	}
	if m.Sender != nil {
		return m.Sender.EmailAddress.Name
	}
	return ""
}

// validGraphIDRe matches the charset Microsoft Graph uses for its opaque IDs:
// URL-safe base64 with `-`, `_`, `=` padding plus `+`, `/` for some legacy
// variants. Used to gate values that get interpolated into OData $filter
// clauses — a Graph ID will always pass; anything containing quotes, spaces,
// or OData operators won't.
var validGraphIDRe = regexp.MustCompile(`^[A-Za-z0-9_=+/\-]+$`)

func validGraphID(id string) bool {
	if id == "" || len(id) > 1024 {
		return false
	}
	return validGraphIDRe.MatchString(id)
}

// GetMessage fetches a single message by ID (header fields only — fast path).
func (c *Client) GetMessage(mailbox, messageID string) (*Message, error) {
	u := fmt.Sprintf("https://graph.microsoft.com/v1.0/users/%s/messages/%s?$select=id,conversationId,subject,from,sender,receivedDateTime,categories,webLink,hasAttachments",
		url.PathEscape(mailbox), url.PathEscape(messageID))
	var m Message
	if err := c.do("GET", u, nil, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// GetMessageDetail fetches a message with full body + recipients. Heavier than
// GetMessage — use only when rendering the detail pane.
func (c *Client) GetMessageDetail(mailbox, messageID string) (*Message, error) {
	u := fmt.Sprintf("https://graph.microsoft.com/v1.0/users/%s/messages/%s?$select=id,conversationId,subject,from,sender,toRecipients,ccRecipients,receivedDateTime,categories,webLink,hasAttachments,body,bodyPreview",
		url.PathEscape(mailbox), url.PathEscape(messageID))
	var m Message
	if err := c.do("GET", u, nil, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// FetchAttachmentContent streams the raw bytes for an attachment from Graph's
// /$value endpoint. Callers are responsible for closing the returned body.
// The returned contentType is from Graph's Content-Type header and should be
// passed through to the browser so native renderers (PDF, images) work.
//
// This method does NOT write bytes to disk. Callers that persist the stream
// are violating the prototype's no-persist rule — only stream to a short-lived
// response writer.
func (c *Client) FetchAttachmentContent(mailbox, messageID, attachmentID string) (io.ReadCloser, string, int64, error) {
	tok, err := c.token()
	if err != nil {
		return nil, "", 0, err
	}
	u := fmt.Sprintf("https://graph.microsoft.com/v1.0/users/%s/messages/%s/attachments/%s/$value",
		url.PathEscape(mailbox), url.PathEscape(messageID), url.PathEscape(attachmentID))
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.doHTTP(req)
	if err != nil {
		return nil, "", 0, fmt.Errorf("GET %s: %w", u, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, "", 0, fmt.Errorf("GET %s returned %d: %s", u, resp.StatusCode, body)
	}
	return resp.Body, resp.Header.Get("Content-Type"), resp.ContentLength, nil
}

// ListAttachments returns metadata for the message's attachments (name, type,
// size) WITHOUT the content bytes. Lightweight list for the detail view header.
func (c *Client) ListAttachments(mailbox, messageID string) ([]Attachment, error) {
	u := fmt.Sprintf("https://graph.microsoft.com/v1.0/users/%s/messages/%s/attachments?$select=id,name,contentType,size,isInline",
		url.PathEscape(mailbox), url.PathEscape(messageID))
	var page struct {
		Value []Attachment `json:"value"`
	}
	if err := c.do("GET", u, nil, &page); err != nil {
		return nil, err
	}
	return page.Value, nil
}

// ListConversationMessages returns all messages in the mailbox belonging to
// the given conversation, oldest first. Includes full body so the detail view
// can render a Gmail-style thread with per-message cards.
//
// conversationID is interpolated into an OData $filter clause; we reject any
// value outside the well-defined Graph ID charset to prevent OData injection
// in case an untrusted source ever produces the ID. Graph's own IDs always
// pass the regex.
func (c *Client) ListConversationMessages(mailbox, conversationID string) ([]Message, error) {
	if !validGraphID(conversationID) {
		return nil, fmt.Errorf("invalid conversation id %q", conversationID)
	}
	params := url.Values{}
	params.Set("$select", "id,conversationId,subject,from,sender,toRecipients,ccRecipients,receivedDateTime,categories,hasAttachments,bodyPreview,body")
	params.Set("$orderby", "receivedDateTime asc")
	params.Set("$top", "50")
	params.Set("$filter", fmt.Sprintf("conversationId eq '%s'", conversationID))

	u := fmt.Sprintf("https://graph.microsoft.com/v1.0/users/%s/messages?%s",
		url.PathEscape(mailbox), params.Encode())
	var page struct {
		Value []Message `json:"value"`
	}
	if err := c.do("GET", u, nil, &page); err != nil {
		return nil, err
	}
	return page.Value, nil
}

// SetCategories replaces the categories list on a message. Pass the full desired
// list — Graph does not have an "append" semantic for categories.
func (c *Client) SetCategories(mailbox, messageID string, categories []string) error {
	u := fmt.Sprintf("https://graph.microsoft.com/v1.0/users/%s/messages/%s",
		url.PathEscape(mailbox), url.PathEscape(messageID))
	if categories == nil {
		categories = []string{}
	}
	body := map[string]any{"categories": categories}
	return c.do("PATCH", u, body, nil)
}

// ListInboxMessages returns up to `limit` messages from the mailbox Inbox, newest first.
func (c *Client) ListInboxMessages(mailbox string, limit int) ([]Message, error) {
	params := url.Values{}
	params.Set("$select", "id,conversationId,subject,from,sender,receivedDateTime,categories,webLink,bodyPreview,hasAttachments")
	params.Set("$top", "100")
	params.Set("$orderby", "receivedDateTime desc")

	next := fmt.Sprintf(
		"https://graph.microsoft.com/v1.0/users/%s/mailFolders/Inbox/messages?%s",
		url.PathEscape(mailbox), params.Encode(),
	)

	var all []Message
	for next != "" {
		var page struct {
			Value    []Message `json:"value"`
			NextLink string    `json:"@odata.nextLink"`
		}
		if err := c.do("GET", next, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Value...)
		if limit > 0 && len(all) >= limit {
			all = all[:limit]
			break
		}
		next = page.NextLink
	}
	return all, nil
}

// DeltaResult is the outcome of a /delta call: the list of new/changed
// messages, the list of messages Graph says were removed, and the new
// delta link to persist for the next call. Expired (410) means the saved
// link was too old; caller should clear and do a full resync.
type DeltaResult struct {
	Changed    []Message
	RemovedIDs []string
	DeltaLink  string // save this; use it as the next call's deltaLink arg
	Expired    bool   // if true, caller must do a full ListInboxMessages resync
}

// ListInboxDelta does an incremental sync via Graph's /delta endpoint.
// Pass empty deltaLink for a first-time call (returns all messages and
// a fresh link); subsequent calls pass the link saved from the previous
// call and get only what's changed.
//
// Removed messages appear in the response body with just an id + an
// "@removed" annotation — we surface those as RemovedIDs so the caller
// can mark them moved/archived/deleted. Message contents (new or
// modified) come through Changed like any other list response.
//
// Graph's /delta doesn't support $orderby or $top; pagination is driven
// by the embedded nextLink through each page. Final page carries the
// deltaLink for the next call.
func (c *Client) ListInboxDelta(mailbox, deltaLink string) (*DeltaResult, error) {
	next := deltaLink
	if next == "" {
		params := url.Values{}
		params.Set("$select", "id,conversationId,subject,from,sender,receivedDateTime,categories,webLink,bodyPreview,hasAttachments")
		next = fmt.Sprintf(
			"https://graph.microsoft.com/v1.0/users/%s/mailFolders/Inbox/messages/delta?%s",
			url.PathEscape(mailbox), params.Encode(),
		)
	}

	result := &DeltaResult{}
	for next != "" {
		var page struct {
			Value     []json.RawMessage `json:"value"`
			NextLink  string            `json:"@odata.nextLink"`
			DeltaLink string            `json:"@odata.deltaLink"`
		}
		if err := c.do("GET", next, nil, &page); err != nil {
			// 410 Gone means the deltaLink expired. Tell caller to full-resync.
			if strings.Contains(err.Error(), "410") {
				result.Expired = true
				return result, nil
			}
			return nil, err
		}
		for _, raw := range page.Value {
			// Sniff for @removed without decoding the whole message.
			var probe struct {
				ID      string          `json:"id"`
				Removed json.RawMessage `json:"@removed"`
			}
			if err := json.Unmarshal(raw, &probe); err != nil {
				continue
			}
			if len(probe.Removed) > 0 {
				result.RemovedIDs = append(result.RemovedIDs, probe.ID)
				continue
			}
			var m Message
			if err := json.Unmarshal(raw, &m); err != nil {
				continue
			}
			result.Changed = append(result.Changed, m)
		}
		if page.DeltaLink != "" {
			result.DeltaLink = page.DeltaLink
			break // last page — delta link present means no more pagination
		}
		next = page.NextLink
	}
	return result, nil
}
