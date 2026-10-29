package ccxt

// APIKeyClient is a CCXT exchange authenticated by API key, secret and an
// optional passphrase (the usual centralized exchange).
type APIKeyClient interface {
	Client
	SetApiKey(apiKey any)
	SetSecret(secret any)
	SetPassword(password any)
	SetSandboxMode(enabled any)
}

// APIKeyVenue is the venue of such an exchange: credentials API_KEY and
// API_SECRET, PASSPHRASE when set. newClient must reference only that
// exchange's constructor, so the binary links nothing else. Centralized
// exchanges are basic here: no venue-specific code (except
// coinbaseinternational's SetMargin, owner's decision 29/10), not verified
// per exchange.
func APIKeyVenue(id string, newClient func() APIKeyClient) Venue {
	return Venue{
		ID:          id,
		Credentials: []string{"API_KEY", "API_SECRET"},
		Optional:    []string{"PASSPHRASE"},
		Dial: func(cred map[string]string, sandbox bool, _ []string) (Client, error) {
			x := newClient()
			if sandbox {
				x.SetSandboxMode(true)
			}
			if cred != nil {
				x.SetApiKey(cred["API_KEY"])
				x.SetSecret(cred["API_SECRET"])
				if p := cred["PASSPHRASE"]; p != "" {
					x.SetPassword(p)
				}
			}
			return x, nil
		},
	}
}
