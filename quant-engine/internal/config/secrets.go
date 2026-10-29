package config

import (
	"fmt"
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

type Credentials struct {
	Key, Secret, Passphrase Secret
}

// EnvPrefix is the variable prefix of a strategy on an adapter:
// ("example-sma-cross", "my-ex") -> "EXAMPLE_SMA_CROSS_MY_EX_".
func EnvPrefix(name, adapter string) string {
	return strings.ToUpper(strings.ReplaceAll(name+"_"+adapter, "-", "_")) + "_"
}

// LoadCredentials reads <NAME>_<ADAPTER>_API_KEY, _API_SECRET and, when the
// adapter needs one, _PASSPHRASE. Errors name the variable, never a value.
func LoadCredentials(name, adapter string, needPassphrase bool, getenv func(string) string) (Credentials, error) {
	p := EnvPrefix(name, adapter)
	names := []string{p + "API_KEY", p + "API_SECRET"}
	if needPassphrase {
		names = append(names, p+"PASSPHRASE")
	}
	vals := make([]Secret, 3)
	for i, n := range names {
		if vals[i] = Secret(getenv(n)); vals[i] == "" {
			return Credentials{}, fmt.Errorf("credentials: environment variable %s is not set", n)
		}
	}
	return Credentials{Key: vals[0], Secret: vals[1], Passphrase: vals[2]}, nil
}

// refuseSecrets rejects a yml carrying a credential-looking key at any depth,
// naming the key and never its value.
func refuseSecrets(b []byte) error {
	var root yaml.Node
	if err := yaml.Unmarshal(b, &root); err != nil {
		return fmt.Errorf("config: %s", oneLine(err.Error()))
	}
	if k := findSecretKey(&root); k != "" {
		return fmt.Errorf("config: key %q looks like a credential; credentials go only in environment variables (<STRATEGY>_<EXCHANGE>_API_KEY, ...)", k)
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
	for _, w := range []string{"secret", "token", "passphrase", "password", "apikey"} {
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
