package login

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"maunium.net/go/mautrix"

	"github.com/beeper/matrix-mirror/internal/session"
)

// Provider obtains a login request for a new device. Provider-specific flows
// (Beeper's email code, SSO, ...) live behind this so others can be added.
type Provider interface {
	Name() string
	Authenticate(ctx context.Context) (*Credentials, error)
}

// Terminal is how a provider asks the user for credentials.
type Terminal interface {
	Ask(question string) (string, error)
	AskSecret(question string) (string, error)
	Say(format string, args ...any)
}

type Credentials struct {
	HomeserverURL string
	Request       *mautrix.ReqLogin
}

// Login always creates a new device: it never sends a device ID, so the
// server allocates one that no other client shares.
func Login(ctx context.Context, provider Provider, deviceName string) (*session.Session, error) {
	creds, err := provider.Authenticate(ctx)
	if err != nil {
		return nil, err
	}
	cli, err := mautrix.NewClient(creds.HomeserverURL, "", "")
	if err != nil {
		return nil, err
	}
	cli.UserAgent = session.UserAgent
	creds.Request.DeviceID = ""
	creds.Request.InitialDeviceDisplayName = deviceName
	resp, err := cli.Login(ctx, creds.Request)
	if err != nil {
		return nil, fmt.Errorf("matrix login failed: %w", err)
	}
	homeserver := creds.HomeserverURL
	if resp.WellKnown != nil && resp.WellKnown.Homeserver.BaseURL != "" {
		homeserver = strings.TrimRight(resp.WellKnown.Homeserver.BaseURL, "/")
	}
	pickleKey, err := session.NewPickleKey()
	if err != nil {
		return nil, err
	}
	return &session.Session{
		Homeserver:  homeserver,
		UserID:      resp.UserID,
		DeviceID:    resp.DeviceID,
		AccessToken: resp.AccessToken,
		PickleKey:   pickleKey,
		Provider:    provider.Name(),
		DeviceName:  deviceName,
		CreatedAt:   time.Now().UTC(),
	}, nil
}

func DefaultDeviceName() string {
	host := computerName()
	if host == "" {
		host, _ = os.Hostname()
		host = strings.TrimSuffix(host, ".local")
	}
	if host == "" {
		return "matrix-mirror"
	}
	return "matrix-mirror on " + host
}

func computerName() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	out, err := exec.Command("scutil", "--get", "ComputerName").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ResolveHomeserver accepts a full URL or a server name, using .well-known
// discovery for the latter.
func ResolveHomeserver(ctx context.Context, server string) (string, error) {
	server = strings.TrimSpace(server)
	if server == "" {
		return "", fmt.Errorf("homeserver is required")
	}
	if strings.Contains(server, "://") {
		return strings.TrimRight(server, "/"), nil
	}
	wellKnown, err := mautrix.DiscoverClientAPI(ctx, server)
	if err == nil && wellKnown != nil && wellKnown.Homeserver.BaseURL != "" {
		return strings.TrimRight(wellKnown.Homeserver.BaseURL, "/"), nil
	}
	return "https://" + server, nil
}
