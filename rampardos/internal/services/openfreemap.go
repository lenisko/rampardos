package services

import (
	"bufio"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	openFreeMapFilesURL = "https://btrfs.openfreemap.com/files.txt"
	openFreeMapBaseURL  = "https://btrfs.openfreemap.com/"
	cacheExpiration     = 6 * time.Hour
)

// OpenFreeMapService fetches and caches the latest planet mbtiles URL
type OpenFreeMapService struct {
	mu          sync.RWMutex
	cachedURL   string
	lastFetched time.Time
	httpClient  *http.Client
}

// NewOpenFreeMapService creates a new OpenFreeMapService
func NewOpenFreeMapService() *OpenFreeMapService {
	return &OpenFreeMapService{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// GetLatestPlanetURL returns the latest planet mbtiles URL, using cache if valid
func (s *OpenFreeMapService) GetLatestPlanetURL() (string, error) {
	s.mu.RLock()
	if s.cachedURL != "" && time.Since(s.lastFetched) < cacheExpiration {
		url := s.cachedURL
		s.mu.RUnlock()
		return url, nil
	}
	s.mu.RUnlock()

	// Fetch fresh data
	s.mu.Lock()
	defer s.mu.Unlock()

	// Double-check after acquiring write lock
	if s.cachedURL != "" && time.Since(s.lastFetched) < cacheExpiration {
		return s.cachedURL, nil
	}

	url, err := s.fetchLatestPlanetURL()
	if err != nil {
		// Return cached URL if available, even if expired
		if s.cachedURL != "" {
			return s.cachedURL, nil
		}
		return "", err
	}

	s.cachedURL = url
	s.lastFetched = time.Now()
	return url, nil
}

func (s *OpenFreeMapService) fetchLatestPlanetURL() (string, error) {
	resp, err := s.httpClient.Get(openFreeMapFilesURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var lastMbtilesPath string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasSuffix(line, ".mbtiles") && strings.Contains(line, "areas/planet/") {
			lastMbtilesPath = line
		}
	}

	if err := scanner.Err(); err != nil {
		return "", err
	}

	if lastMbtilesPath == "" {
		return "", nil
	}

	return openFreeMapBaseURL + lastMbtilesPath, nil
}
