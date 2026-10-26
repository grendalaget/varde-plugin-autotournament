package varde

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxResponseBytes = 1 << 20

type Client struct {
	baseURL string
	token   string
	http    *http.Client
	stream  *http.Client
}

type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("varde API returned %d: %s", e.StatusCode, e.Message)
}

type Member struct {
	UserID      string  `json:"user_id"`
	DisplayName string  `json:"display_name"`
	SteamID     *string `json:"steam_id"`
}

type GamePort struct {
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
}

type Service struct {
	LoopbackIP string     `json:"loopback_ip"`
	Ports      []GamePort `json:"ports"`
}

type Server struct {
	ID      string   `json:"id"`
	GroupID string   `json:"group_id"`
	GameID  string   `json:"game_id"`
	Service *Service `json:"service"`
	Summary struct {
		Address *string `json:"address"`
	} `json:"summary"`
}

type AddonSpec struct {
	URL           string            `json:"url"`
	SHA256        string            `json:"sha256"`
	Archive       string            `json:"archive"`
	Target        string            `json:"target"`
	DriverOptions map[string]any    `json:"driver_options"`
	Files         map[string]string `json:"files"`
}

type Event struct {
	ID       int64          `json:"id"`
	GroupID  string         `json:"group_id"`
	ServerID *string        `json:"server_id"`
	Type     string         `json:"type"`
	Data     map[string]any `json:"data"`
}

func NewClient(baseURL, token string) (*Client, error) {
	normalized, err := normalizeBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{Proxy: nil, ForceAttemptHTTP2: true}
	return &Client{
		baseURL: normalized,
		token:   token,
		http: &http.Client{
			Timeout:   10 * time.Second,
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		stream: &http.Client{
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func normalizeBaseURL(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("Varde URL must be an absolute HTTP or HTTPS URL without credentials, query, or fragment")
	}
	return strings.TrimRight(value, "/"), nil
}

func (c *Client) ListMembers(ctx context.Context, groupID string) ([]Member, error) {
	var response struct {
		Members []Member `json:"members"`
	}
	if err := c.requestJSON(ctx, http.MethodGet, "/v1/groups/"+url.PathEscape(groupID)+"/members", nil, &response); err != nil {
		return nil, err
	}
	return response.Members, nil
}

func (c *Client) ListServers(ctx context.Context, groupID string) ([]Server, error) {
	var response struct {
		Servers []Server `json:"servers"`
	}
	if err := c.requestJSON(ctx, http.MethodGet, "/v1/groups/"+url.PathEscape(groupID)+"/servers", nil, &response); err != nil {
		return nil, err
	}
	return response.Servers, nil
}

func (c *Client) GetServer(ctx context.Context, serverID string) (Server, error) {
	var response Server
	err := c.requestJSON(ctx, http.MethodGet, "/v1/servers/"+url.PathEscape(serverID), nil, &response)
	return response, err
}

func (c *Client) PutAddon(ctx context.Context, serverID, pluginID, addonID string, spec AddonSpec) error {
	return c.requestJSON(ctx, http.MethodPut, addonPath(serverID, pluginID, addonID), spec, nil)
}

func (c *Client) DeleteAddon(ctx context.Context, serverID, pluginID, addonID string) error {
	return c.requestJSON(ctx, http.MethodDelete, addonPath(serverID, pluginID, addonID), nil, nil)
}

func addonPath(serverID, pluginID, addonID string) string {
	return "/v1/servers/" + url.PathEscape(serverID) + "/addons/" +
		url.PathEscape(pluginID) + "/" + url.PathEscape(addonID)
}

func (c *Client) requestJSON(ctx context.Context, method, path string, payload, out any) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode Varde request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("create Varde request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("request Varde API: %w", err)
	}
	defer response.Body.Close()

	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read Varde response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return errors.New("Varde response exceeds 1 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return responseError(response.StatusCode, data)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode Varde response: %w", err)
	}
	return nil
}

func responseError(status int, body []byte) error {
	var message struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &message)
	if message.Error == "" {
		message.Error = message.Message
	}
	if message.Error == "" {
		message.Error = http.StatusText(status)
	}
	return &APIError{StatusCode: status, Message: message.Error}
}

func (c *Client) StreamEvents(ctx context.Context, groupID, lastEventID string, onEvent func(Event)) (string, error) {
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		c.baseURL+"/v1/groups/"+url.PathEscape(groupID)+"/events/stream",
		nil,
	)
	if err != nil {
		return lastEventID, fmt.Errorf("create Varde event request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		request.Header.Set("Last-Event-ID", lastEventID)
	}
	response, err := c.stream.Do(request)
	if err != nil {
		return lastEventID, fmt.Errorf("connect to Varde event stream: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
		if readErr != nil {
			return lastEventID, fmt.Errorf("read Varde event response: %w", readErr)
		}
		return lastEventID, responseError(response.StatusCode, body)
	}

	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), maxResponseBytes)
	eventID := ""
	data := strings.Builder{}
	last := lastEventID
	dispatch := func() error {
		if data.Len() == 0 {
			eventID = ""
			return nil
		}
		var event Event
		if err := json.Unmarshal([]byte(data.String()), &event); err != nil {
			return fmt.Errorf("decode Varde event: %w", err)
		}
		if eventID != "" {
			last = eventID
			if event.ID == 0 {
				event.ID, _ = strconv.ParseInt(eventID, 10, 64)
			}
		}
		if onEvent != nil {
			onEvent(event)
		}
		eventID = ""
		data.Reset()
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return last, err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			value = ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			eventID = value
		case "data":
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return last, fmt.Errorf("read Varde event stream: %w", err)
	}
	if err := dispatch(); err != nil {
		return last, err
	}
	return last, io.EOF
}
