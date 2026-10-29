package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rosaldo/agency-agents/quant-engine/strategy/example"
)

func TestSecretKeysInYAMLAreRefused(t *testing.T) {
	for _, extra := range []string{
		"api:\n  secret: hunter2value",
		"api_key: hunter2value",
		"Token: hunter2value",
		"brokers:\n  - name: x\n    passphrase: hunter2value",
		"key: hunter2value",
	} {
		_, err := Parse([]byte(validYAML+extra+"\n"), example.New())
		if err == nil {
			t.Fatalf("accepted a secret in yml: %q", extra)
		}
		if strings.Contains(err.Error(), "hunter2value") {
			t.Errorf("error leaks the secret value: %v", err)
		}
		if !strings.Contains(err.Error(), "environment") {
			t.Errorf("error should point to environment variables: %v", err)
		}
	}
}

func TestCredentialsFromEnvironment(t *testing.T) {
	env := map[string]string{"EXAMPLE_SMA_CROSS_MY_EX_API_KEY": "k-hunter2", "EXAMPLE_SMA_CROSS_MY_EX_API_SECRET": "s-hunter2"}
	cred, err := LoadCredentials(example.Name, "my-ex", false, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cred.Key.Reveal() != "k-hunter2" || cred.Secret.Reveal() != "s-hunter2" {
		t.Fatalf("credentials not read from the environment")
	}
	for _, out := range []string{fmt.Sprintf("%v", cred), fmt.Sprintf("%+v", cred), fmt.Sprintf("%#v", cred), fmt.Sprintf("%s %q", cred.Key, cred.Secret)} {
		if strings.Contains(out, "hunter2") {
			t.Errorf("formatted credential leaks the secret: %s", out)
		}
	}
}

func TestMissingCredentialNamesTheVariable(t *testing.T) {
	env := map[string]string{"EXAMPLE_SMA_CROSS_MY_EX_API_KEY": "k-hunter2", "EXAMPLE_SMA_CROSS_MY_EX_API_SECRET": "s-hunter2"}
	_, err := LoadCredentials(example.Name, "my-ex", true, func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), "EXAMPLE_SMA_CROSS_MY_EX_PASSPHRASE") || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("missing passphrase: %v", err)
	}
}
