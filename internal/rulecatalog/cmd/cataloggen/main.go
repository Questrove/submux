package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const repositoryAPI = "https://api.github.com/repos/MetaCubeX/meta-rules-dat"

type branchResponse struct {
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

type treeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

type treeResponse struct {
	Tree []treeEntry `json:"tree"`
}

func main() {
	output := flag.String("output", "catalog.tsv", "output file")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 30 * time.Second}

	var branch branchResponse
	getJSON(ctx, client, repositoryAPI+"/branches/meta", &branch)
	var root treeResponse
	getJSON(ctx, client, repositoryAPI+"/git/trees/"+branch.Commit.SHA, &root)
	geoSHA := findTree(root.Tree, "geo")
	var geo treeResponse
	getJSON(ctx, client, repositoryAPI+"/git/trees/"+geoSHA, &geo)

	lines := []string{"# commit=" + branch.Commit.SHA}
	for _, kind := range []string{"geosite", "geoip"} {
		var rules treeResponse
		getJSON(ctx, client, repositoryAPI+"/git/trees/"+findTree(geo.Tree, kind), &rules)
		for _, entry := range rules.Tree {
			if entry.Type == "blob" && strings.HasSuffix(entry.Path, ".mrs") {
				lines = append(lines, kind+"\t"+strings.TrimSuffix(entry.Path, ".mrs"))
			}
		}
	}
	sort.Strings(lines[1:])
	if err := os.MkdirAll(filepath.Dir(*output), 0o755); err != nil {
		fatal(err)
	}
	if err := os.WriteFile(*output, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("wrote %d rules at %s to %s\n", len(lines)-1, branch.Commit.SHA, *output)
}

func findTree(entries []treeEntry, name string) string {
	for _, entry := range entries {
		if entry.Path == name && entry.Type == "tree" {
			return entry.SHA
		}
	}
	fatal(fmt.Errorf("tree %q not found", name))
	return ""
}

func getJSON(ctx context.Context, client *http.Client, url string, target any) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fatal(err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "submux-rule-catalog-generator")
	response, err := client.Do(req)
	if err != nil {
		fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		fatal(fmt.Errorf("GET %s: %s", url, response.Status))
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
