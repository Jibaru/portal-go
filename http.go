package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/Jibaru/portal-go/wire"
)

// httpClient is the HTTP side of the protocol (§3): persistent publish, history
// (backfill, paging and gap-fill), the member directory, and the anonymous token
// mint. All calls authenticate with `Authorization: Bearer <token>` plus the
// x-portal-key header — except the mint, which is apiKey-only.
type httpClient struct {
	baseURL string
	apiKey  string
	token   func(ctx context.Context) (string, error)
	client  *http.Client
}

// publishOutcome is a non-error result: rejections are expected protocol
// outcomes ({code, reason}), not transport failures.
type publishOutcome struct {
	ok     bool
	ack    wire.SendAck
	code   string
	reason string
}

type historyQuery struct {
	before *int64
	limit  *int
	from   *int64
	to     *int64
}

func (h *httpClient) authHeaders(ctx context.Context, req *http.Request) error {
	token, err := h.token(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(wire.APIKeyHeader, h.apiKey)
	return nil
}

// errorBody reads a {code, reason} rejection body, falling back to http_<status>.
func errorBody(resp *http.Response) (code, reason string) {
	code = fmt.Sprintf("http_%d", resp.StatusCode)
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return code, ""
	}
	var parsed wire.PublishErrorBody
	if json.Unmarshal(body, &parsed) == nil {
		if parsed.Code != "" {
			code = parsed.Code
		}
		reason = parsed.Reason
	}
	return code, reason
}

func (h *httpClient) publish(ctx context.Context, channelID string, body wire.PublishBody) (publishOutcome, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return publishOutcome{}, err
	}
	endpoint := h.baseURL + "/v1/channels/" + url.PathEscape(channelID) + "/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return publishOutcome{}, err
	}
	if err := h.authHeaders(ctx, req); err != nil {
		return publishOutcome{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return publishOutcome{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var ack wire.SendAck
		if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
			return publishOutcome{}, err
		}
		return publishOutcome{ok: true, ack: ack}, nil
	}
	code, reason := errorBody(resp)
	return publishOutcome{code: code, reason: reason}, nil
}

func (h *httpClient) history(ctx context.Context, channelID string, query historyQuery) (wire.HistoryResponse, error) {
	endpoint, err := url.Parse(h.baseURL + "/v1/channels/" + url.PathEscape(channelID) + "/history")
	if err != nil {
		return wire.HistoryResponse{}, err
	}
	q := endpoint.Query()
	if query.before != nil {
		q.Set("before", fmt.Sprint(*query.before))
	}
	if query.limit != nil {
		q.Set("limit", fmt.Sprint(*query.limit))
	}
	if query.from != nil {
		q.Set("from", fmt.Sprint(*query.from))
	}
	if query.to != nil {
		q.Set("to", fmt.Sprint(*query.to))
	}
	endpoint.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return wire.HistoryResponse{}, err
	}
	if err := h.authHeaders(ctx, req); err != nil {
		return wire.HistoryResponse{}, err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return wire.HistoryResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return wire.HistoryResponse{}, fmt.Errorf("portal: history request failed with status %d", resp.StatusCode)
	}
	var page wire.HistoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return wire.HistoryResponse{}, err
	}
	return page, nil
}

func (h *httpClient) members(ctx context.Context, channelID, cursor string) (wire.MembersResponse, error) {
	endpoint, err := url.Parse(h.baseURL + "/v1/channels/" + url.PathEscape(channelID) + "/members")
	if err != nil {
		return wire.MembersResponse{}, err
	}
	if cursor != "" {
		q := endpoint.Query()
		q.Set("cursor", cursor)
		endpoint.RawQuery = q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return wire.MembersResponse{}, err
	}
	if err := h.authHeaders(ctx, req); err != nil {
		return wire.MembersResponse{}, err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return wire.MembersResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return wire.MembersResponse{}, fmt.Errorf("portal: members request failed with status %d", resp.StatusCode)
	}
	var page wire.MembersResponse
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return wire.MembersResponse{}, err
	}
	return page, nil
}

type mintOutcome struct {
	ok     bool
	token  string
	code   string
	reason string
}

// mintAnonymousToken calls `POST /v1/tokens/anonymous` on the API host. The mint
// route authenticates by apiKey only and never sends a bearer token.
func (h *httpClient) mintAnonymousToken(ctx context.Context, anonID string) (mintOutcome, error) {
	body := map[string]string{}
	if anonID != "" {
		body["anonId"] = anonID
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return mintOutcome{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL+"/v1/tokens/anonymous", bytes.NewReader(payload))
	if err != nil {
		return mintOutcome{}, err
	}
	req.Header.Set(wire.APIKeyHeader, h.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return mintOutcome{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var out struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return mintOutcome{}, err
		}
		return mintOutcome{ok: true, token: out.Token}, nil
	}
	code, reason := errorBody(resp)
	return mintOutcome{code: code, reason: reason}, nil
}
