package inbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/integrations/meta"
)

// Graph sends inbox replies through the Graph API (v26.0), per
// docs/integrations/social-inbox-apis.md:
//   - a Messenger or Page-linked Instagram DM: POST /{page-id}/messages with messaging_type
//     RESPONSE (inside the 24 hour window);
//   - a Facebook comment: POST /{comment-id}/comments;
//   - an Instagram comment: POST /{ig-comment-id}/replies.
//
// It also hides, unhides and deletes comments (HideComment, DeleteComment).
type Graph struct {
	Base string // https://graph.facebook.com/v26.0
	HTTP *http.Client
}

func NewGraph(base string) *Graph {
	return &Graph{Base: strings.TrimSuffix(base, "/"), HTTP: &http.Client{Timeout: 20 * time.Second}}
}

// WithProof signs every Graph call with appsecret_proof (needed once "Require app secret" is on
// in the Meta app); secret is meta.PlatformSecret in production.
func (g *Graph) WithProof(secret meta.SecretSource) *Graph {
	g.HTTP.Transport = meta.NewProofTransport(g.HTTP.Transport, secret)
	return g
}

// SendDM answers a DM; it returns the sent message id.
func (g *Graph) SendDM(ctx context.Context, pageID, token, recipient, text string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"recipient":      map[string]string{"id": recipient},
		"messaging_type": "RESPONSE",
		"message":        map[string]string{"text": text},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.Base+"/"+url.PathEscape(pageID)+"/messages?access_token="+url.QueryEscape(token), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		MessageID string `json:"message_id"`
	}
	if err := g.do(req, &out); err != nil {
		return "", fmt.Errorf("send dm: %w", err)
	}
	return out.MessageID, nil
}

// ReplyToComment posts a public reply under a comment; it returns the new comment id.
func (g *Graph) ReplyToComment(ctx context.Context, platform, commentID, token, text string) (string, error) {
	if commentID == "" {
		return "", fmt.Errorf("reply: no comment id")
	}
	edge := "/comments"
	if platform == "instagram" {
		edge = "/replies"
	}
	form := url.Values{"message": {text}, "access_token": {token}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.Base+"/"+url.PathEscape(commentID)+edge, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out struct {
		ID string `json:"id"`
	}
	if err := g.do(req, &out); err != nil {
		return "", fmt.Errorf("reply to comment: %w", err)
	}
	return out.ID, nil
}

// HideComment hides or unhides a comment on the business's own post. Facebook:
// POST /{comment-id} is_hidden=true|false; Instagram: POST /{ig-comment-id} hide=true|false.
func (g *Graph) HideComment(ctx context.Context, platform, commentID, token string, hidden bool) error {
	if commentID == "" {
		return fmt.Errorf("hide comment: no comment id")
	}
	field := "is_hidden"
	if platform == "instagram" {
		field = "hide"
	}
	form := url.Values{field: {strconv.FormatBool(hidden)}, "access_token": {token}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.Base+"/"+url.PathEscape(commentID), strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out map[string]any
	if err := g.do(req, &out); err != nil {
		return fmt.Errorf("hide comment: %w", err)
	}
	return nil
}

// DeleteComment removes a comment from the business's own post: DELETE /{comment-id} on Facebook
// and DELETE /{ig-comment-id} on Instagram (the same call; platform is kept for symmetry).
func (g *Graph) DeleteComment(ctx context.Context, _ string, commentID, token string) error {
	if commentID == "" {
		return fmt.Errorf("delete comment: no comment id")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, g.Base+"/"+url.PathEscape(commentID)+"?access_token="+url.QueryEscape(token), nil)
	if err != nil {
		return err
	}
	var out map[string]any
	if err := g.do(req, &out); err != nil {
		return fmt.Errorf("delete comment: %w", err)
	}
	return nil
}

// GraphError is a Graph API refusal with Meta's own message.
type GraphError struct {
	Status  int
	Message string
}

func (e *GraphError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("status %d", e.Status)
	}
	return fmt.Sprintf("%d: %s", e.Status, e.Message)
}

func (g *Graph) do(req *http.Request, out any) error {
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var ge struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(b, &ge)
		return &GraphError{Status: resp.StatusCode, Message: ge.Error.Message}
	}
	return json.Unmarshal(b, out)
}
