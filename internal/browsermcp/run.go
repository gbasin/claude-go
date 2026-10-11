package browsermcp

import (
	"fmt"
	"io"
	"os"

	"github.com/gbasin/claude-go/internal/api"
	"github.com/gbasin/claude-go/internal/auth"
)

// run.go — runClaudeInChromeMcpServer (mod_0807.js qe): the entry point
// behind `claude-go --claude-in-chrome-mcp`.
//
// Real's startup order: settings/helper preflight (RWt → settings load +
// helper run — N/A for us), Cloud-gateway sign-in load (akr — N/A),
// storage/credentials pin (dD/Hx — auth.Load), entitlement gate W(),
// context K(), server vUn(), stdio transport INe, exit on stdin EOF.

// OAuthSource adapter over internal/auth.Credentials.
type credsOAuth struct {
	creds *auth.Credentials
	gate  *BearerGate
}

func (o credsOAuth) Token() string {
	if o.creds == nil || o.creds.IsAPIKey {
		return ""
	}
	// Wi() — refresh first when refreshable
	if o.creds.RefreshToken != "" {
		_ = o.creds.Refresh()
	}
	if t, err := o.creds.Token(); err == nil {
		// bearerGate: withhold tokens the org policy denies
		if t != "" && o.gate != nil && !o.gate.Allow(t) {
			return ""
		}
		return t
	}
	return ""
}

func (o credsOAuth) BearerAllowed(token string) bool {
	if o.gate == nil {
		return true
	}
	return o.gate.Allow(token)
}

// RunServer is the `qe` body. It writes the verdict to stderr on denial
// and returns the process exit code (real: exit 1 denied, 0 on EOF).
func RunServer() int {
	return runServer(os.Stdin, os.Stdout)
}

// runServer is the testable core.
func runServer(in io.Reader, out io.Writer) int {
	log := StdLogger{}

	creds, _ := auth.Load()
	deps := GateDeps{
		Provider: string(api.DetectProvider()),
		BaseURL:  os.Getenv("ANTHROPIC_BASE_URL"),
		OAuthToken: func() string {
			if creds == nil || creds.IsAPIKey {
				return ""
			}
			if creds.RefreshToken != "" {
				_ = creds.Refresh()
			}
			t, err := creds.Token()
			if err != nil {
				return ""
			}
			return t
		},
	}
	res := Gate(deps)
	if res.Denied {
		fmt.Fprintln(os.Stderr, ErrOrgDenied)
		return 1
	}
	if res.Unverified != "" {
		log.Warnf("%s", res.UnverifiedMessage())
	}

	gate := NewBearerGate(res.VerifiedBearer, deps, log)
	ctx := NewContext(log, credsOAuth{creds: creds, gate: gate}, PersistPairing)
	tr := pickTransport(ctx)
	log.Infof("Starting MCP server")
	srv := newServer(ctx, tr, in, out)
	srv.serve()
	tr.disconnect()
	return 0
}
