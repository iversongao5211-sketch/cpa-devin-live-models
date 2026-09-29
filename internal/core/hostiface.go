package core

import "encoding/json"

// Host abstracts the plugin->host call surface (host.auth.*, host.log).
// The c-shared shim in package main implements it over the C ABI.
type Host interface {
	// Call invokes a host method with a JSON payload and returns the raw
	// result payload (envelope.result).
	Call(method string, payload []byte) (json.RawMessage, error)
	// Log writes a message into the host log sink.
	Log(level, message string, fields map[string]any)
}

// listAuths returns the host auth file entries.
func listAuths(h Host) ([]hostAuthFileEntry, error) {
	raw, err := h.Call("host.auth.list", []byte(`{}`))
	if err != nil {
		return nil, err
	}
	var resp hostAuthListResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	return resp.Files, nil
}

// getAuth returns the full credential JSON for one auth index.
func getAuth(h Host, authIndex string) (json.RawMessage, error) {
	req, _ := json.Marshal(map[string]string{"auth_index": authIndex})
	raw, err := h.Call("host.auth.get", req)
	if err != nil {
		return nil, err
	}
	var resp hostAuthGetResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	return resp.JSON, nil
}

// saveAuth rewrites an auth file through the host (triggers re-registration
// via the fs watcher when content differs).
func saveAuth(h Host, name string, doc map[string]any) error {
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	req, err := json.Marshal(map[string]any{"name": name, "json": json.RawMessage(raw)})
	if err != nil {
		return err
	}
	_, err = h.Call("host.auth.save", req)
	return err
}

// devinCredentials extracts what a live-catalog fetch needs from an auth file.
type devinCredentials struct {
	SessionToken string
	ProxyURL     string
	BaseURL      string
	DeviceSeed   string
}

func credsFromAuthJSON(raw json.RawMessage) devinCredentials {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return devinCredentials{}
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := doc[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	token := str("api_key")
	if token == "" {
		token = str("session_token")
	}
	return devinCredentials{
		SessionToken: token,
		ProxyURL:     str("proxy_url", "proxy-url"),
		BaseURL:      str("base_url", "base-url"),
		DeviceSeed:   str("device_seed", "device-seed"),
	}
}
