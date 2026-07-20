package rulecatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"submux/internal/resourceproxy"
	"submux/internal/store"
)

const (
	repositoryAPI       = "https://api.github.com/repos/MetaCubeX/meta-rules-dat"
	refreshStateSetting = "rule_catalog_refresh_state"
	maxGitHubBodyBytes  = 12 << 20
)

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

type RefreshState struct {
	LastAttemptAt string `json:"last_attempt_at,omitempty"`
	LastSuccessAt string `json:"last_success_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	LastResult    string `json:"last_result,omitempty"`
	Route         string `json:"route,omitempty"`
	CatalogCommit string `json:"catalog_commit,omitempty"`
	ETag          string `json:"etag,omitempty"`
	LastModified  string `json:"last_modified,omitempty"`
	RateRemaining int    `json:"rate_remaining"`
	RateReset     string `json:"rate_reset,omitempty"`
}

type branchResponse struct {
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

type treeResponse struct {
	Tree []struct {
		Path string `json:"path"`
		Type string `json:"type"`
	} `json:"tree"`
	Truncated bool `json:"truncated"`
}

func LoadRefreshState(st *store.Store) RefreshState {
	raw, _ := st.GetSetting(refreshStateSetting)
	var value RefreshState
	_ = json.Unmarshal([]byte(raw), &value)
	return value
}

func Refresh(ctx context.Context, st *store.Store) (Snapshot, RefreshState, error) {
	return refreshFrom(ctx, st, repositoryAPI)
}

func refreshFrom(ctx context.Context, st *store.Store, repository string) (Snapshot, RefreshState, error) {
	state := LoadRefreshState(st)
	state.LastAttemptAt = time.Now().UTC().Format(time.RFC3339)
	config, err := resourceproxy.Load(st)
	if err != nil {
		return refreshFailed(st, state, "platform resource proxy configuration is invalid")
	}
	state.Route = config.Mode
	client, err := resourceproxy.NewClient(config, 30*time.Second)
	if err != nil {
		return refreshFailed(st, state, err.Error())
	}

	var branch branchResponse
	status, headers, err := githubJSON(ctx, client, strings.TrimRight(repository, "/")+"/branches/meta", state.ETag, state.LastModified, &branch)
	updateRateState(&state, headers)
	if err != nil {
		return refreshFailed(st, state, err.Error())
	}
	if status == http.StatusNotModified {
		current, currentErr := ActiveCatalog(st)
		if currentErr != nil {
			return refreshFailed(st, state, currentErr.Error())
		}
		if state.CatalogCommit != "" && state.CatalogCommit == current.Commit {
			state.LastSuccessAt, state.LastError, state.LastResult = time.Now().UTC().Format(time.RFC3339), "", "规则目录没有变化"
			saveRefreshState(st, state)
			return current, state, nil
		}
		status, headers, err = githubJSON(ctx, client, strings.TrimRight(repository, "/")+"/branches/meta", "", "", &branch)
		updateRateState(&state, headers)
		if err != nil || status != http.StatusOK {
			if err == nil {
				err = errors.New("GitHub branch request did not return content")
			}
			return refreshFailed(st, state, err.Error())
		}
	}
	nextETag := headers.Get("ETag")
	nextLastModified := headers.Get("Last-Modified")
	commit := strings.ToLower(strings.TrimSpace(branch.Commit.SHA))
	if !commitPattern.MatchString(commit) {
		return refreshFailed(st, state, "GitHub returned an invalid rule catalog commit")
	}
	if current, currentErr := ActiveCatalog(st); currentErr == nil && current.Commit == commit {
		state.ETag, state.LastModified = nextETag, nextLastModified
		state.CatalogCommit = commit
		state.LastSuccessAt, state.LastError, state.LastResult = time.Now().UTC().Format(time.RFC3339), "", "规则目录没有变化"
		saveRefreshState(st, state)
		return current, state, nil
	}

	var tree treeResponse
	_, treeHeaders, err := githubJSON(ctx, client, strings.TrimRight(repository, "/")+"/git/trees/"+commit+"?recursive=1", "", "", &tree)
	updateRateState(&state, treeHeaders)
	if err != nil {
		return refreshFailed(st, state, err.Error())
	}
	if tree.Truncated {
		return refreshFailed(st, state, "GitHub rule catalog tree is truncated")
	}
	names := map[string][]string{"geosite": {}, "geoip": {}}
	seen := map[string]bool{}
	for _, item := range tree.Tree {
		if item.Type != "blob" || !strings.HasSuffix(item.Path, ".mrs") {
			continue
		}
		for _, kind := range []string{"geosite", "geoip"} {
			prefix := "geo/" + kind + "/"
			if !strings.HasPrefix(item.Path, prefix) {
				continue
			}
			name := strings.TrimSuffix(strings.TrimPrefix(item.Path, prefix), ".mrs")
			if name == "" || strings.Contains(name, "/") {
				continue
			}
			key := kind + "/" + name
			if seen[key] {
				return refreshFailed(st, state, "GitHub rule catalog contains duplicate entries")
			}
			seen[key] = true
			names[kind] = append(names[kind], name)
		}
	}
	if len(seen) < 100 || len(names["geosite"]) == 0 || len(names["geoip"]) == 0 {
		return refreshFailed(st, state, "GitHub rule catalog is incomplete")
	}
	sort.Strings(names["geosite"])
	sort.Strings(names["geoip"])
	now := time.Now().UTC().Format(time.RFC3339)
	result := NewSnapshot(commit, names, now)
	for _, key := range defaultOrder {
		if _, ok := LookupIn(result, key); !ok {
			return refreshFailed(st, state, "GitHub rule catalog is missing required entry "+key)
		}
	}
	if err := SaveActiveCatalog(st, result); err != nil {
		return refreshFailed(st, state, "save refreshed rule catalog failed")
	}
	state.ETag, state.LastModified = nextETag, nextLastModified
	state.CatalogCommit = commit
	state.LastSuccessAt, state.LastError = now, ""
	state.LastResult = fmt.Sprintf("已更新到 %s，共 %d 条规则", commit[:12], len(result.Entries))
	saveRefreshState(st, state)
	return result, state, nil
}

func githubJSON(ctx context.Context, client *http.Client, endpoint, etag, lastModified string, target any) (int, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, nil, errors.New("create GitHub request failed")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "submux-rule-catalog")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastModified != "" {
		req.Header.Set("If-Modified-Since", lastModified)
	}
	response, err := client.Do(req)
	if err != nil {
		return 0, nil, errors.New("GitHub request failed")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified {
		return response.StatusCode, response.Header, nil
	}
	if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests {
		return response.StatusCode, response.Header, fmt.Errorf("GitHub API rate limit reached (status %d)", response.StatusCode)
	}
	if response.StatusCode != http.StatusOK {
		return response.StatusCode, response.Header, fmt.Errorf("GitHub API returned status %d", response.StatusCode)
	}
	limited := io.LimitReader(response.Body, maxGitHubBodyBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return response.StatusCode, response.Header, errors.New("read GitHub response failed")
	}
	if len(raw) > maxGitHubBodyBytes {
		return response.StatusCode, response.Header, errors.New("GitHub response is too large")
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return response.StatusCode, response.Header, errors.New("decode GitHub response failed")
	}
	return response.StatusCode, response.Header, nil
}

func updateRateState(state *RefreshState, headers http.Header) {
	if headers == nil {
		return
	}
	if value, err := strconv.Atoi(headers.Get("X-RateLimit-Remaining")); err == nil {
		state.RateRemaining = value
	}
	if value, err := strconv.ParseInt(headers.Get("X-RateLimit-Reset"), 10, 64); err == nil && value > 0 {
		state.RateReset = time.Unix(value, 0).UTC().Format(time.RFC3339)
	}
}

func refreshFailed(st *store.Store, state RefreshState, message string) (Snapshot, RefreshState, error) {
	state.LastError, state.LastResult = message, ""
	saveRefreshState(st, state)
	current, _ := ActiveCatalog(st)
	return current, state, errors.New(message)
}

func saveRefreshState(st *store.Store, value RefreshState) {
	raw, _ := json.Marshal(value)
	_ = st.SetSetting(refreshStateSetting, string(raw))
}
