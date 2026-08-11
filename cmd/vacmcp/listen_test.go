package main

import (
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/server"
)

// A configured listen address that the binary ignores is worse than one it
// rejects: the user believes the server is bound where they put it. These pin
// the precedence --address > server.address > the built-in default.
func TestListenAddressPrecedence(t *testing.T) {
	configured := &config.Config{Server: config.Server{Address: "127.0.0.1:9000"}}
	empty := &config.Config{}

	for name, tc := range map[string]struct {
		flagValue string
		explicit  bool
		cfg       *config.Config
		want      string
	}{
		"config only": {
			flagValue: server.DefaultAddress, explicit: false, cfg: configured,
			want: "127.0.0.1:9000",
		},
		"flag only": {
			flagValue: "127.0.0.1:9100", explicit: true, cfg: empty,
			want: "127.0.0.1:9100",
		},
		"flag wins over config": {
			flagValue: "127.0.0.1:9100", explicit: true, cfg: configured,
			want: "127.0.0.1:9100",
		},
		"neither": {
			flagValue: server.DefaultAddress, explicit: false, cfg: empty,
			want: server.DefaultAddress,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := listenAddress(tc.flagValue, tc.explicit, tc.cfg); got != tc.want {
				t.Errorf("listenAddress(%q, %t, %+v) = %q, want %q", tc.flagValue, tc.explicit, tc.cfg.Server, got, tc.want)
			}
		})
	}
}

// The default has to stay on loopback: this server is local-first, and a
// regression here would put it on every interface without anyone asking.
func TestDefaultAddressIsLoopback(t *testing.T) {
	if got := listenAddress(server.DefaultAddress, false, &config.Config{}); got != "127.0.0.1:8080" {
		t.Errorf("default listen address = %q, want 127.0.0.1:8080", got)
	}
}
