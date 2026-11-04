package inbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Graph sends inbox replies through the Graph API (v26.0), per
// docs/integrations/social-inbox-apis.md:
//   - a Messenger or Page-linked Instagram DM: POST /{page-id}/messages with messaging_type
//     RESPONSE (inside the 24 hour window);
//   - a Facebook comment: POST /{comment-id}/comments;
//   - an Instagram comment: POST /{ig-comment-id}/replies.
type Graph struct {
	Base string // https://graph.facebook.com/v26.0
	HTTP *http.Client
}

func NewGraph(base string) *Graph {
	return &Graph{Base: strings.TrimSuffix(base, "/"), HTTP: &http.Client{Timeout: 20 * time.Second}}
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
		if json.Unmarshal(b, &ge) == nil && ge.Error.Message != "" {
			return fmt.Errorf("%d: %s", resp.StatusCode, ge.Error.Message)
		}
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.Unmarshal(b, out)
}
