package login

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"maunium.net/go/mautrix"

	"github.com/beeper/matrix-mirror/internal/session"
)

const DefaultBeeperDomain = "beeper.com"

// The same static bearer bbctl uses for the pre-login endpoints.
const beeperLoginAuth = "BEEPER-PRIVATE-API-PLEASE-DONT-USE"

var errInvalidCode = errors.New("invalid login code")

// Beeper runs Beeper's email-and-code login (as bbctl does) and exchanges the
// resulting JWT for a Matrix access token on a new device.
type Beeper struct {
	Domain   string
	Email    string
	Terminal Terminal
	HTTP     *http.Client
}

func (b *Beeper) Name() string { return "beeper" }

func (b *Beeper) Authenticate(ctx context.Context) (*Credentials, error) {
	if b.Domain == "" {
		b.Domain = DefaultBeeperDomain
	}
	if b.HTTP == nil {
		b.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	var start struct {
		RequestID string `json:"request"`
	}
	if err := b.post(ctx, "/user/login", struct{}{}, &start); err != nil {
		return nil, fmt.Errorf("start Beeper login: %w", err)
	}
	email := b.Email
	var err error
	if email == "" {
		if email, err = b.Terminal.Ask("Beeper email: "); err != nil {
			return nil, err
		}
	}
	if err = b.post(ctx, "/user/login/email", map[string]string{"request": start.RequestID, "email": email}, nil); err != nil {
		return nil, fmt.Errorf("send login email: %w", err)
	}
	for {
		code, err := b.Terminal.Ask("Code from the email: ")
		if err != nil {
			return nil, err
		}
		var resp struct {
			Token     string `json:"token"`
			LeadToken string `json:"leadToken"`
		}
		err = b.post(ctx, "/user/login/response", map[string]any{
			"request":               start.RequestID,
			"response":              code,
			"onlyExistingAccounts":  true,
			"reactivationSupported": true,
		}, &resp)
		if errors.Is(err, errInvalidCode) {
			b.Terminal.Say("%v", err)
			continue
		} else if err != nil {
			return nil, fmt.Errorf("submit login code: %w", err)
		}
		if resp.LeadToken != "" {
			return nil, fmt.Errorf("there's no Beeper account for %s", email)
		} else if resp.Token == "" {
			return nil, errors.New("Beeper asked for a login step matrix-mirror doesn't support")
		}
		return &Credentials{
			HomeserverURL: "https://matrix." + b.Domain,
			Request:       &mautrix.ReqLogin{Type: "org.matrix.login.jwt", Token: resp.Token},
		}, nil
	}
}

func (b *Beeper) post(ctx context.Context, path string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api."+b.Domain+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+beeperLoginAuth)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", session.UserAgent)
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errBody struct {
			Error   string   `json:"error"`
			Retries *float64 `json:"retries"`
		}
		_ = json.Unmarshal(data, &errBody)
		if resp.StatusCode == http.StatusForbidden && errBody.Retries != nil {
			if *errBody.Retries <= 0 {
				return errors.New("too many wrong codes; start the login again")
			}
			return fmt.Errorf("%w (%d tries left)", errInvalidCode, int(*errBody.Retries))
		}
		if errBody.Error != "" {
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, errBody.Error)
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}
