package ccxt

import (
	"strings"
	"testing"

	lib "github.com/rosaldo/ccxt/v5"
)

// noSandbox is an exchange the fork knows no testnet URL for: its
// SetSandboxMode panics, as the fork's does.
type noSandbox struct {
	APIKeyClient
	keyed bool
}

func (*noSandbox) SetSandboxMode(any) { panic(lib.NotSupported("x does not have a sandbox URL")) }
func (n *noSandbox) SetApiKey(any)    { n.keyed = true }

// QA T12 NIT: a venue without sandbox fails the testnet Dial with an error,
// not a panic, and never receives the credential.
func TestDialWithoutSandboxIsAnError(t *testing.T) {
	x := &noSandbox{}
	v := APIKeyVenue("x", func() APIKeyClient { return x })
	c, err := v.Dial(map[string]string{"API_KEY": "k", "API_SECRET": "s"}, true, nil)
	if err == nil || c != nil || !strings.Contains(err.Error(), "no testnet") {
		t.Fatalf("dial = %v %v; want a no-testnet error", c, err)
	}
	if x.keyed {
		t.Fatal("credential set on a client that has no testnet")
	}
}
