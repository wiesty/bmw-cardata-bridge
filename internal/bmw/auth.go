package bmw

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	authServer            = "https://customer.bmwgroup.com"
	authScope             = "authenticate_user openid cardata:api:read cardata:streaming:read"
	deviceCodeGrantType   = "urn:ietf:params:oauth:grant-type:device_code"
	refreshTokenGrantType = "refresh_token"
)

type authSession struct {
	ClientID     string    `json:"client_id"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func (s *authSession) isExpired() bool {
	if s == nil {
		return true
	}
	return time.Now().Add(10 * time.Second).After(s.ExpiresAt)
}

type fileSessionStore struct {
	path   string
	cached *authSession
}

func (f *fileSessionStore) get() (*authSession, error) {
	if f.cached != nil {
		return f.cached, nil
	}
	data, err := os.ReadFile(f.path)
	if err != nil {
		return nil, err
	}
	var s authSession
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	f.cached = &s
	return &s, nil
}

func (f *fileSessionStore) save(s *authSession) error {
	f.cached = s
	if err := os.MkdirAll(filepath.Dir(f.path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(f.path, data, 0600)
}

// Auth handles BMW OAuth2 device code flow + PKCE S256 + token refresh.
type Auth struct {
	clientID  string
	promptURI func(verificationURI, userCode string)
	store     *fileSessionStore
	http      *http.Client
}

// NewAuth creates an Auth with file-based session persistence at sessionPath.
func NewAuth(clientID, sessionPath string, promptURI func(string, string)) *Auth {
	return &Auth{
		clientID:  clientID,
		promptURI: promptURI,
		store:     &fileSessionStore{path: sessionPath},
		http:      &http.Client{Timeout: 30 * time.Second},
	}
}

// Token returns a valid access token, refreshing or re-authenticating as needed.
func (a *Auth) Token(ctx context.Context) (string, error) {
	s, err := a.store.get()
	if err == nil && s != nil {
		if s.ClientID != a.clientID {
			// different client ID stored — force fresh auth
		} else if !s.isExpired() {
			return s.AccessToken, nil
		} else {
			refreshed, err := a.refresh(ctx, s.RefreshToken)
			if err == nil {
				return refreshed.AccessToken, nil
			}
		}
	}
	newSession, err := a.deviceCodeFlow(ctx)
	if err != nil {
		return "", err
	}
	return newSession.AccessToken, nil
}

func (a *Auth) deviceCodeFlow(ctx context.Context) (*authSession, error) {
	verifier, challenge, err := pkceS256()
	if err != nil {
		return nil, fmt.Errorf("pkce: %w", err)
	}

	resp, err := a.postForm(ctx, authServer+"/gcdm/oauth/device/code", url.Values{
		"client_id":             {a.clientID},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"response_type":         {"device_code"},
		"scope":                 {authScope},
	})
	if err != nil {
		return nil, fmt.Errorf("device code request: %w", err)
	}
	defer resp.Body.Close()

	var dc struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationUri string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dc); err != nil {
		return nil, fmt.Errorf("decode device code: %w", err)
	}
	if dc.DeviceCode == "" {
		return nil, fmt.Errorf("empty device_code in response (status %s)", resp.Status)
	}

	a.promptURI(dc.VerificationUri, dc.UserCode)

	pollSec := dc.Interval
	if pollSec <= 0 {
		pollSec = 5
	}
	expiresAt := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)

	for time.Now().Before(expiresAt) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(pollSec) * time.Second):
		}

		tokenResp, err := a.postForm(ctx, authServer+"/gcdm/oauth/token", url.Values{
			"client_id":     {a.clientID},
			"code_verifier": {verifier},
			"device_code":   {dc.DeviceCode},
			"grant_type":    {deviceCodeGrantType},
		})
		if err != nil {
			continue
		}
		if tokenResp.StatusCode == http.StatusForbidden {
			tokenResp.Body.Close()
			continue // authorization_pending
		}
		var td struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    int    `json:"expires_in"`
		}
		json.NewDecoder(tokenResp.Body).Decode(&td)
		tokenResp.Body.Close()

		if td.AccessToken != "" {
			s := &authSession{
				ClientID:     a.clientID,
				AccessToken:  td.AccessToken,
				RefreshToken: td.RefreshToken,
				ExpiresAt:    time.Now().Add(time.Duration(td.ExpiresIn) * time.Second),
			}
			_ = a.store.save(s)
			return s, nil
		}
	}
	return nil, fmt.Errorf("device code flow expired")
}

func (a *Auth) refresh(ctx context.Context, refreshToken string) (*authSession, error) {
	resp, err := a.postForm(ctx, authServer+"/gcdm/oauth/token", url.Values{
		"client_id":     {a.clientID},
		"refresh_token": {refreshToken},
		"grant_type":    {refreshTokenGrantType},
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh token: %s", resp.Status)
	}
	var td struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&td); err != nil {
		return nil, err
	}
	s := &authSession{
		ClientID:     a.clientID,
		AccessToken:  td.AccessToken,
		RefreshToken: td.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(td.ExpiresIn) * time.Second),
	}
	_ = a.store.save(s)
	return s, nil
}

func (a *Auth) postForm(ctx context.Context, endpoint string, values url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return a.http.Do(req)
}

func pkceS256() (verifier, challenge string, err error) {
	b := make([]byte, 64)
	if _, err = rand.Read(b); err != nil {
		return
	}
	verifier = strings.TrimRight(base64.URLEncoding.EncodeToString(b), "=")
	sum := sha256.Sum256([]byte(verifier))
	challenge = strings.TrimRight(base64.URLEncoding.EncodeToString(sum[:]), "=")
	return
}
