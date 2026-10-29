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
		"strategy:\n  private_key: hunter2value",
		"wallet:\n  privateKey: hunter2value",
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

var suffixes = []string{"WALLET_ADDRESS", "PRIVATE_KEY"}

func TestCredentialsFromEnvironment(t *testing.T) {
	env := map[string]string{
		"EXAMPLE_SMA_CROSS_MY_EX_WALLET_ADDRESS": "0xmain", "EXAMPLE_SMA_CROSS_MY_EX_PRIVATE_KEY": "main-hunter2",
		"EXAMPLE_SMA_CROSS_MY_EX_TESTNET_WALLET_ADDRESS": "0xtest", "EXAMPLE_SMA_CROSS_MY_EX_TESTNET_PRIVATE_KEY": "test-hunter2",
		"EXAMPLE_SMA_CROSS_MY_EX_TESTNET_PASSPHRASE": "pass-hunter2",
	}
	get := func(k string) string { return env[k] }
	real, err := LoadCredentials(example.Name, "my-ex", false, suffixes, []string{"PASSPHRASE"}, get)
	if err != nil {
		t.Fatal(err)
	}
	demo, err := LoadCredentials(example.Name, "my-ex", true, suffixes, []string{"PASSPHRASE"}, get)
	if err != nil {
		t.Fatal(err)
	}
	if real["PRIVATE_KEY"].Reveal() != "main-hunter2" || demo["PRIVATE_KEY"].Reveal() != "test-hunter2" ||
		demo["PASSPHRASE"].Reveal() != "pass-hunter2" || real["PASSPHRASE"] != "" {
		t.Fatalf("mainnet and testnet variables not kept apart: real %v demo %v", real, demo)
	}
	for _, out := range []string{fmt.Sprintf("%v", real), fmt.Sprintf("%+v", demo), fmt.Sprintf("%#v", demo), fmt.Sprintf("%s %q", real["PRIVATE_KEY"], demo["PRIVATE_KEY"])} {
		if strings.Contains(out, "hunter2") {
			t.Errorf("formatted credential leaks the secret: %s", out)
		}
	}
}

// The same secret in the testnet and mainnet variables is refused: a real key
// must never serve the demo, nor the other way round.
func TestCrossedCredentialsRefused(t *testing.T) {
	env := map[string]string{
		"EXAMPLE_SMA_CROSS_MY_EX_WALLET_ADDRESS": "0xsame", "EXAMPLE_SMA_CROSS_MY_EX_PRIVATE_KEY": "same-hunter2",
		"EXAMPLE_SMA_CROSS_MY_EX_TESTNET_WALLET_ADDRESS": "0xsame", "EXAMPLE_SMA_CROSS_MY_EX_TESTNET_PRIVATE_KEY": "same-hunter2",
	}
	for _, testnet := range []bool{false, true} {
		_, err := LoadCredentials(example.Name, "my-ex", testnet, suffixes, nil, func(k string) string { return env[k] })
		if err == nil || !strings.Contains(err.Error(), "PRIVATE_KEY") || strings.Contains(err.Error(), "hunter2") {
			t.Fatalf("testnet=%v: crossed credential: %v", testnet, err)
		}
	}
}

func TestMissingCredentialNamesTheVariable(t *testing.T) {
	env := map[string]string{"EXAMPLE_SMA_CROSS_MY_EX_WALLET_ADDRESS": "0xmain", "EXAMPLE_SMA_CROSS_MY_EX_PRIVATE_KEY": "k-hunter2"}
	_, err := LoadCredentials(example.Name, "my-ex", true, suffixes, nil, func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), "EXAMPLE_SMA_CROSS_MY_EX_TESTNET_WALLET_ADDRESS") || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("missing testnet credential: %v", err)
	}
}
