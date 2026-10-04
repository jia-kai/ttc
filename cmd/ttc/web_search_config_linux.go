package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"ttc/internal/tool"
)

func webSearchConfigPath() (string, error) {
	root := os.Getenv("XDG_CONFIG_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".config")
	}
	return filepath.Join(root, "ttc", "web-search.json"), nil
}

// Missing optional configuration selects the public keyless endpoint. Credentials
// never enter tool schemas, history or model runtime snapshots.
func loadWebSearchConfig(path string) (tool.WebSearchConfig, error) {
	var config tool.WebSearchConfig
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return config, nil
	}
	if err != nil {
		return config, fmt.Errorf("web search config: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return config, fmt.Errorf("stat web search config: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return config, errors.New("web search config must be a regular file of at most 64 KiB")
	}
	data, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil {
		return config, fmt.Errorf("read web search config: %w", err)
	}
	if len(data) > 64<<10 {
		return config, errors.New("web search config exceeds 64 KiB")
	}
	if err = tool.Strict(data, &config); err != nil {
		return config, fmt.Errorf("decode web search config: %w", err)
	}
	if config.APIKey != "" && info.Mode().Perm()&0077 != 0 {
		return config, errors.New("web search config contains credentials; run chmod 600 on the config file")
	}
	if err = config.Validate(); err != nil {
		return config, fmt.Errorf("web search config: %w", err)
	}
	return config, nil
}
