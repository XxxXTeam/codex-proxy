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
	catalogMu   sync.RWMutex
	rawCatalog  = append([]byte(nil), embeddedCatalog...)
	response    map[string]any
	loadErr     error
	revision    uint64
	modelCount  int
	updatedAt   time.Time
	source      string
	lastChecked time.Time
	lastError   error
)

const refreshInterval = 3 * time.Hour

// Status 描述当前模型目录缓存状态。
type Status struct {
	Revision           uint64    `json:"revision"`
	ModelCount         int       `json:"model_count"`
	UpdatedAt          time.Time `json:"updated_at,omitempty"`
	Source             string    `json:"source,omitempty"`
	RefreshIntervalSec int       `json:"refresh_interval_sec"`
	LastCheckedAt      time.Time `json:"last_checked_at,omitempty"`
	LastError          string    `json:"last_error,omitempty"`
}

var remoteCatalogURLs = []string{
	"https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/codex_client_models.json",
	"https://models.router-for.me/codex_client_models.json",
}

func init() {
	if err := loadFromBytes(embeddedCatalog, "embedded"); err != nil {
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

// CurrentStatus 返回当前模型目录缓存状态的快照。
func CurrentStatus() Status {
	catalogMu.RLock()
	defer catalogMu.RUnlock()
	return Status{
		Revision:           revision,
		ModelCount:         modelCount,
		UpdatedAt:          updatedAt,
		Source:             source,
		RefreshIntervalSec: int(refreshInterval / time.Second),
		LastCheckedAt:      lastChecked,
		LastError:          errorString(lastError),
	}
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
		if err = loadFromBytes(data, endpoint); err != nil {
			lastErr = err
			continue
		}
		return nil
	}

	if lastErr != nil {
		catalogMu.Lock()
		lastChecked = time.Now().UTC()
		lastError = lastErr
		catalogMu.Unlock()
		return lastErr
	}
	catalogMu.Lock()
	lastChecked = time.Now().UTC()
	lastError = fmt.Errorf("codex catalog refresh failed")
	catalogMu.Unlock()
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

func loadFromBytes(data []byte, sourceName string) error {
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	count := 0
	if models, ok := decoded["models"].([]any); ok {
		count = len(models)
	}

	catalogMu.Lock()
	defer catalogMu.Unlock()
	if bytes.Equal(rawCatalog, data) && response != nil {
		loadErr = nil
		lastError = nil
		updatedAt = time.Now().UTC()
		lastChecked = updatedAt
		source = sourceName
		return nil
	}
	rawCatalog = append([]byte(nil), data...)
	response = decoded
	loadErr = nil
	lastError = nil
	revision++
	modelCount = count
	updatedAt = time.Now().UTC()
	lastChecked = updatedAt
	source = sourceName
	return nil
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
