package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type apiClient struct {
	client *http.Client
	base   string
	mode   string
	token  string
}

type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message) }

type sandbox struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedBy string `json:"created_by"`
	EngineID  string `json:"engine_id"`
	Status    string `json:"status"`
	Thermal   string `json:"thermal"`
	IP        string `json:"ip"`
	Image     string `json:"image"`
}
type snapshot struct {
	Name          string  `json:"name"`
	SourceSandbox string  `json:"source_sandbox"`
	SizeMB        float64 `json:"size_mb"`
}
type execResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}
type createSpec struct {
	Name, Tier, Image, From, Network, Init string
	CPUs, Memory                           int
	KeepHot                                bool
}

func newAPI(addr, token string, timeout time.Duration) (*apiClient, error) {
	if token == "" {
		return nil, errors.New("API token required")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = 256
	tr.MaxIdleConnsPerHost = 256
	tr.MaxConnsPerHost = 0
	tr.IdleConnTimeout = 90 * time.Second
	base, mode := addr, "http"
	if strings.HasPrefix(addr, "unix://") {
		sock := strings.TrimPrefix(addr, "unix://")
		if sock == "" || !strings.HasPrefix(sock, "/") {
			return nil, fmt.Errorf("unix API path must be absolute: %q", addr)
		}
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "unix", sock)
		}
		tr.Proxy = nil
		base = "http://unix"
		mode = "unix"
	} else {
		u, err := url.Parse(addr)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("invalid API endpoint (expected unix:///path or http(s)://host): %q", addr)
		}
		mode = u.Scheme
	}
	return &apiClient{client: &http.Client{Transport: tr, Timeout: timeout}, base: base, mode: mode, token: token}, nil
}

func (a *apiClient) request(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var payload struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&payload)
		if payload.Error == "" {
			payload.Error = resp.Status
		}
		return nil, &apiError{Status: resp.StatusCode, Message: payload.Error}
	}
	return resp, nil
}

func (a *apiClient) json(ctx context.Context, method, path string, body, into any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(buf)
	}
	resp, err := a.request(ctx, method, path, reader, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if into == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

func (a *apiClient) version(ctx context.Context) string {
	resp, err := a.request(ctx, http.MethodGet, "/health", nil, "")
	if err != nil {
		return "unavailable: " + err.Error()
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if v := resp.Header.Get("X-Bhatti-Version"); v != "" {
		return v
	}
	return "unavailable (no X-Bhatti-Version header)"
}

func netPolicy(name string) (any, error) {
	switch name {
	case "", "default":
		return nil, nil
	case "none":
		return map[string]string{"default": "none"}, nil
	case "public":
		return map[string]string{"default": "public"}, nil
	case "deny":
		return map[string]string{"default": "deny"}, nil
	case "allow-siblings":
		return map[string]string{"default": "deny", "siblings": "allow"}, nil
	case "public-siblings":
		return map[string]string{"default": "public", "siblings": "allow"}, nil
	default:
		return nil, fmt.Errorf("invalid network policy %q (default|none|public|deny|allow-siblings|public-siblings)", name)
	}
}

func (a *apiClient) create(ctx context.Context, s createSpec) (sandbox, error) {
	policy, err := netPolicy(s.Network)
	if err != nil {
		return sandbox{}, err
	}
	if s.From != "" && policy != nil {
		return sandbox{}, fmt.Errorf("fork inherits network policy; -network %q is not allowed with -from", s.Network)
	}
	image := s.Tier
	if s.Image != "" {
		image = s.Image
	}
	body := map[string]any{"name": s.Name, "image": image, "from": s.From, "cpus": s.CPUs, "memory_mb": s.Memory, "keep_hot": s.KeepHot}
	if s.Init != "" {
		body["init"] = s.Init
	}
	if policy != nil {
		body["net_policy"] = policy
	}
	var sb sandbox
	err = a.json(ctx, http.MethodPost, "/sandboxes", body, &sb)
	return sb, err
}
func (a *apiClient) inspect(ctx context.Context, id string) (sandbox, error) {
	var sb sandbox
	err := a.json(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id), nil, &sb)
	return sb, err
}
func (a *apiClient) list(ctx context.Context) ([]sandbox, error) {
	var list []sandbox
	err := a.json(ctx, http.MethodGet, "/sandboxes", nil, &list)
	return list, err
}
func (a *apiClient) stop(ctx context.Context, id string) error {
	return a.json(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/stop", nil, nil)
}
func (a *apiClient) start(ctx context.Context, id string) error {
	return a.json(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/start", nil, nil)
}
func (a *apiClient) destroy(ctx context.Context, id string) error {
	return a.json(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(id), nil, nil)
}
func (a *apiClient) exec(ctx context.Context, id string, argv ...string) (execResult, error) {
	var result execResult
	err := a.json(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/exec", map[string]any{"cmd": argv}, &result)
	if err == nil && result.ExitCode != 0 {
		err = fmt.Errorf("guest exit %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return result, err
}
func (a *apiClient) execDetached(ctx context.Context, id string, argv ...string) error {
	var result struct {
		PID      int  `json:"pid"`
		Detached bool `json:"detached"`
	}
	err := a.json(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/exec", map[string]any{"cmd": argv, "detach": true}, &result)
	if err != nil {
		return err
	}
	if !result.Detached || result.PID <= 0 {
		return errors.New("daemon did not confirm detached exec")
	}
	return nil
}
func (a *apiClient) fileWrite(ctx context.Context, id, path string, data []byte) error {
	resp, err := a.request(ctx, http.MethodPut, "/sandboxes/"+url.PathEscape(id)+"/files?path="+url.QueryEscape(path), bytes.NewReader(data), "application/octet-stream")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}
func (a *apiClient) fileRead(ctx context.Context, id, path string) ([]byte, error) {
	resp, err := a.request(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id)+"/files?path="+url.QueryEscape(path), nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}
func (a *apiClient) snapshot(ctx context.Context, id, name string) (snapshot, error) {
	var snap snapshot
	err := a.json(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/checkpoint", map[string]string{"name": name}, &snap)
	return snap, err
}
func (a *apiClient) resume(ctx context.Context, snap, name string) (sandbox, error) {
	var sb sandbox
	err := a.json(ctx, http.MethodPost, "/snapshots/"+url.PathEscape(snap)+"/resume", map[string]string{"name": name}, &sb)
	return sb, err
}
func (a *apiClient) listSnapshots(ctx context.Context) ([]snapshot, error) {
	var list []snapshot
	err := a.json(ctx, http.MethodGet, "/snapshots", nil, &list)
	return list, err
}
func (a *apiClient) deleteSnapshot(ctx context.Context, name string) error {
	return a.json(ctx, http.MethodDelete, "/snapshots/"+url.PathEscape(name), nil, nil)
}
func (a *apiClient) publish(ctx context.Context, id, alias string, port int) (string, error) {
	var rule struct {
		URL string `json:"url"`
	}
	err := a.json(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/publish", map[string]any{"port": port, "alias": alias}, &rule)
	return rule.URL, err
}
