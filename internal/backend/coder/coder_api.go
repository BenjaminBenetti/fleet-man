package coder

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// coderAPIGet makes an authenticated GET request to the Coder API.
func coderAPIGet(path string) ([]byte, error) {
	baseURL, token, err := coderCredentials()
	if err != nil {
		return nil, err
	}

	url := strings.TrimRight(baseURL, "/") + "/" + path
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Coder-Session-Token", token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("coder API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("coder API returned %d: %s", resp.StatusCode, body)
	}

	return body, nil
}

// coderCredentials reads the Coder URL and session token from the CLI config.
func coderCredentials() (string, string, error) {
	baseURL := os.Getenv("CODER_URL")
	token := os.Getenv("CODER_SESSION_TOKEN")

	if baseURL == "" || token == "" {
		configDir := coderConfigDir()
		if baseURL == "" {
			data, err := os.ReadFile(filepath.Join(configDir, "url"))
			if err != nil {
				return "", "", fmt.Errorf("coder not authenticated: cannot read URL from %s: %w", configDir, err)
			}
			baseURL = strings.TrimSpace(string(data))
		}
		if token == "" {
			data, err := os.ReadFile(filepath.Join(configDir, "session"))
			if err != nil {
				return "", "", fmt.Errorf("coder not authenticated: cannot read session from %s: %w", configDir, err)
			}
			token = strings.TrimSpace(string(data))
		}
	}

	return baseURL, token, nil
}

// coderConfigDir returns the coder CLI config directory.
func coderConfigDir() string {
	if configDir := os.Getenv("CODER_CONFIG_DIR"); configDir != "" {
		return configDir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "coderv2")
}
