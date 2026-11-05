package login

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"time"

	"maunium.net/go/mautrix"
)

const (
	MethodAuto     = ""
	MethodPassword = "password"
	MethodSSO      = "sso"
	MethodToken    = "token"
)

type Matrix struct {
	Server   string
	Method   string
	User     string
	Token    string
	Terminal Terminal
}

func (m *Matrix) Name() string { return "matrix" }

func (m *Matrix) Authenticate(ctx context.Context) (*Credentials, error) {
	homeserver, err := ResolveHomeserver(ctx, m.Server)
	if err != nil {
		return nil, err
	}
	cli, err := mautrix.NewClient(homeserver, "", "")
	if err != nil {
		return nil, err
	}
	method := m.Method
	if m.Token != "" {
		method = MethodToken
	}
	if method == MethodAuto {
		if method, err = pickMethod(ctx, cli); err != nil {
			return nil, err
		}
	}
	creds := &Credentials{HomeserverURL: homeserver}
	switch method {
	case MethodPassword:
		creds.Request, err = m.password()
	case MethodToken:
		creds.Request, err = m.token()
	case MethodSSO:
		creds.Request, err = m.sso(ctx, cli)
	default:
		err = fmt.Errorf("unknown login method %q", method)
	}
	return creds, err
}

func pickMethod(ctx context.Context, cli *mautrix.Client) (string, error) {
	flows, err := cli.GetLoginFlows(ctx)
	if err != nil {
		return "", fmt.Errorf("fetch login flows from %s: %w", cli.HomeserverURL, err)
	}
	switch {
	case flows.HasFlow(mautrix.AuthTypePassword):
		return MethodPassword, nil
	case flows.HasFlow(mautrix.AuthTypeSSO):
		return MethodSSO, nil
	}
	return "", fmt.Errorf("%s offers no password or SSO login; pass --token with a login token", cli.HomeserverURL)
}

func (m *Matrix) password() (*mautrix.ReqLogin, error) {
	user := m.User
	var err error
	if user == "" {
		if user, err = m.Terminal.Ask("Username: "); err != nil {
			return nil, err
		}
	}
	password, err := m.Terminal.AskSecret("Password: ")
	if err != nil {
		return nil, err
	}
	return &mautrix.ReqLogin{
		Type:       mautrix.AuthTypePassword,
		Identifier: mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: user},
		Password:   password,
	}, nil
}

func (m *Matrix) token() (*mautrix.ReqLogin, error) {
	token := m.Token
	var err error
	if token == "" {
		if token, err = m.Terminal.AskSecret("Login token: "); err != nil {
			return nil, err
		}
	}
	return &mautrix.ReqLogin{Type: mautrix.AuthTypeToken, Token: token}, nil
}

const ssoTimeout = 5 * time.Minute

// sso runs the browser redirect flow, catching the login token on a
// one-shot listener bound to localhost.
func (m *Matrix) sso(ctx context.Context, cli *mautrix.Client) (*mautrix.ReqLogin, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	tokens := make(chan string, 1)
	server := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := r.URL.Query().Get("loginToken")
			if token == "" {
				http.Error(w, "missing loginToken", http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprintln(w, "matrix-mirror is logged in. You can close this tab.")
			select {
			case tokens <- token:
			default:
			}
		}),
	}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	redirect := fmt.Sprintf("http://%s/", listener.Addr())
	ssoURL := cli.BuildURLWithQuery(mautrix.ClientURLPath{"v3", "login", "sso", "redirect"}, map[string]string{"redirectUrl": redirect})
	m.Terminal.Say("Log in through your browser:\n  %s", ssoURL)
	openBrowser(ssoURL)

	select {
	case token := <-tokens:
		return &mautrix.ReqLogin{Type: mautrix.AuthTypeToken, Token: token}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(ssoTimeout):
		return nil, errors.New("timed out waiting for SSO login")
	}
}

func openBrowser(url string) {
	var opener string
	switch runtime.GOOS {
	case "darwin":
		opener = "open"
	case "linux":
		opener = "xdg-open"
	default:
		return
	}
	_ = exec.Command(opener, url).Start()
}
