package codexcatalog

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

//go:embed models/codex_client_models.json
var embeddedCatalog []byte

var (
	catalogMu  sync.RWMutex
	rawCatalog = append([]byte(nil), embeddedCatalog...)
	response   map[string]any
	loadErr    error
	revision   uint64
)

const refreshInterval = 3 * time.Hour

var remoteCatalogURLs = []string{
	"https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/codex_client_models.json",
	"https://models.router-for.me/codex_client_models.json",
}

func init() {
	if err := loadFromBytes(embeddedCatalog); err != nil {
		loadErr = err
	}
}

func Response() map[string]any {
	catalogMu.RLock()
	defer catalogMu.RUnlock()
	if loadErr != nil {
		return nil
	}
	return response
}

func Raw() []byte {
	catalogMu.RLock()
	defer catalogMu.RUnlock()
	return append([]byte(nil), rawCatalog...)
}

func Err() error {
	catalogMu.RLock()
	defer catalogMu.RUnlock()
	return loadErr
}

func Revision() uint64 {
	catalogMu.RLock()
	defer catalogMu.RUnlock()
	return revision
}

func StartAutoRefresh(ctx context.Context) {
	go func() {
		if err := RefreshOnce(ctx, nil); err != nil {
			log.Warnf("codex catalog initial refresh failed: %v", err)
		}

		ticker := time.NewTicker(refreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := RefreshOnce(ctx, nil); err != nil {
					log.Warnf("codex catalog refresh failed: %v", err)
				}
			}
		}
	}()
}

func RefreshOnce(ctx context.Context, client *http.Client) error {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	var lastErr error
	for _, endpoint := range remoteCatalogURLs {
		data, err := fetchCatalog(ctx, client, endpoint)
		if err != nil {
			lastErr = err
			continue
		}
		if err = loadFromBytes(data); err != nil {
			lastErr = err
			continue
		}
		return nil
	}

	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("codex catalog refresh failed")
}

func fetchCatalog(ctx context.Context, client *http.Client, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, endpoint)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	return data, nil
}

func loadFromBytes(data []byte) error {
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}

	catalogMu.Lock()
	defer catalogMu.Unlock()
	if bytes.Equal(rawCatalog, data) && response != nil {
		loadErr = nil
		return nil
	}
	rawCatalog = append([]byte(nil), data...)
	response = decoded
	loadErr = nil
	revision++
	return nil
}
