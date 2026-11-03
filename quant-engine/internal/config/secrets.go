package config

import (
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Secret holds a credential value; every formatting path prints "***".
type Secret string

func (Secret) String() string               { return "***" }
func (Secret) GoString() string             { return "***" }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"***"`), nil }
func (Secret) MarshalYAML() (any, error)    { return "***", nil }
func (s Secret) Reveal() string             { return string(s) }

// EnvPrefix is the variable prefix of a strategy on a venue, mainnet or
// testnet: ("example-sma-cross", "my-ex", true) -> "EXAMPLE_SMA_CROSS_MY_EX_TESTNET_".
func EnvPrefix(name, venue string, testnet bool) string {
	p := name + "_" + venue
	if testnet {
		p += "_TESTNET"
	}
	return strings.ToUpper(strings.ReplaceAll(p, "-", "_")) + "_"
}

// NewsKeyVar is the variable holding the news classifier's API key of a
// strategy: <STRATEGY>_TYPESAFE_API_KEY, e.g. EXAMPLE_SMA_CROSS_TYPESAFE_API_KEY.
// It has no venue: one key serves every venue and network.
func NewsKeyVar(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_TYPESAFE_API_KEY"
}

// LoadCredentials reads <PREFIX><SUFFIX> for every required and optional
// suffix, with the testnet or the mainnet prefix. A required variable that is
// unset fails; errors name variables, never values. The two networks keep
// separate variables, yet may hold the same key: Hyperliquid signs testnet
// and mainnet actions apart (source "b" / "a"), so one key serves both.
func LoadCredentials(name, venue string, testnet bool, required, optional []string, getenv func(string) string) (map[string]Secret, error) {
	p := EnvPrefix(name, venue, testnet)
	out := map[string]Secret{}
	for i, suffix := range append(slices.Clone(required), optional...) {
		v := getenv(p + suffix)
		switch {
		case v == "" && i < len(required):
			return nil, fmt.Errorf("credentials: environment variable %s is not set", p+suffix)
		case v == "":
			continue
		}
		out[suffix] = Secret(v)
	}
	return out, nil
}

// refuseSecrets rejects a yml carrying a credential-looking key at any depth,
// naming the key and never its value.
func refuseSecrets(b []byte) error {
	var root yaml.Node
	if err := yaml.Unmarshal(b, &root); err != nil {
		return fmt.Errorf("config: %s", oneLine(err.Error()))
	}
	if k := findSecretKey(&root); k != "" {
		return fmt.Errorf("config: key %q looks like a credential; credentials go only in environment variables (<STRATEGY>_<EXCHANGE>[_TESTNET]_<NAME>)", k)
	}
	return nil
}

func findSecretKey(n *yaml.Node) string {
	for i, c := range n.Content {
		if n.Kind == yaml.MappingNode && i%2 == 0 && isSecretKey(c.Value) {
			return c.Value
		}
		if k := findSecretKey(c); k != "" {
			return k
		}
	}
	return ""
}

func isSecretKey(k string) bool {
	k = strings.ToLower(k)
	for _, w := range []string{"secret", "token", "passphrase", "password", "apikey", "privatekey", "mnemonic"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	for _, part := range strings.FieldsFunc(k, func(r rune) bool { return r == '_' || r == '-' }) {
		if part == "key" {
			return true
		}
	}
	return false
}
