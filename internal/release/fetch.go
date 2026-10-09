package release

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	maxMetadata = 1 << 20
	maxArtifact = 256 << 20
	httpTimeout = 30 * time.Second
)

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: httpTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL == nil || req.URL.Scheme != "https" {
				return errors.New("refusing a non-https redirect")
			}
			if len(via) == 0 || via[0].URL == nil {
				return errors.New("redirect is missing the original host")
			}
			if req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("refusing an off-host redirect to %s", req.URL.Host)
			}
			return nil
		},
	}
}

func getCapped(ctx context.Context, client *http.Client, rawURL string, max int64) ([]byte, int, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, 0, unreachable(rawURL, fmt.Errorf("release URL must be https"))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, 0, unreachable(rawURL, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, unreachable(rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, resp.StatusCode, unreachable(rawURL, err)
	}
	if int64(len(body)) > max {
		return nil, resp.StatusCode, unreachable(rawURL, fmt.Errorf("response exceeds the size limit"))
	}
	return body, resp.StatusCode, nil
}

func openHTTPS(ctx context.Context, client *http.Client, rawURL string) (*http.Response, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, unreachable(rawURL, fmt.Errorf("release URL must be https"))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, unreachable(rawURL, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, unreachable(rawURL, err)
	}
	return resp, nil
}
