package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

type blacklistRule struct {
	Pattern     string
	Category    string
	Description string
}

var blacklistRules []blacklistRule

// defaultExcluded is the set of category prefixes excluded by default.
var defaultExcluded = []string{"System"}

// loadBlacklist reads the YAML blacklist file with a simple line-by-line parser
// (avoids a YAML dependency). The format is well-known and consistent.
func loadBlacklist(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var rules []blacklistRule
	var cur blacklistRule

	flush := func() {
		if cur.Pattern != "" && cur.Category != "" {
			rules = append(rules, cur)
		}
		cur = blacklistRule{}
	}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "- pattern:") {
			flush()
			cur.Pattern = unquote(strings.TrimPrefix(trimmed, "- pattern:"))
		} else if strings.HasPrefix(trimmed, "category:") {
			cur.Category = unquote(strings.TrimPrefix(trimmed, "category:"))
		} else if strings.HasPrefix(trimmed, "description:") {
			cur.Description = unquote(strings.TrimPrefix(trimmed, "description:"))
		}
	}
	flush()

	blacklistRules = rules
	return scanner.Err()
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// isExcluded reports whether the given path's base name matches any blacklist rule
// whose category starts with one of the prefixes in excluded.
// If excluded is nil, defaultExcluded is used.
func isExcluded(path string, excluded []string) bool {
	if excluded == nil {
		excluded = defaultExcluded
	}
	if len(excluded) == 0 {
		return false
	}
	name := filepath.Base(path)
	for _, rule := range blacklistRules {
		if !matchGlob(rule.Pattern, name) {
			continue
		}
		for _, prefix := range excluded {
			if rule.Category == prefix || strings.HasPrefix(rule.Category, prefix+":") {
				return true
			}
		}
	}
	return false
}

// matchGlob is a simple fnmatch-style pattern matcher (supports * and ? only).
func matchGlob(pattern, name string) bool {
	matched, _ := filepath.Match(pattern, name)
	return matched
}

// getAllBlacklistCategories returns sorted unique category names from loaded rules.
func getAllBlacklistCategories() []string {
	seen := map[string]bool{}
	var cats []string
	for _, r := range blacklistRules {
		if !seen[r.Category] {
			seen[r.Category] = true
			cats = append(cats, r.Category)
		}
	}
	// sort inline
	for i := 1; i < len(cats); i++ {
		for j := i; j > 0 && cats[j] < cats[j-1]; j-- {
			cats[j], cats[j-1] = cats[j-1], cats[j]
		}
	}
	return cats
}

// getAllBlacklistRules returns all loaded blacklist rules as plain maps.
func getAllBlacklistRules() []map[string]string {
	rules := make([]map[string]string, len(blacklistRules))
	for i, r := range blacklistRules {
		rules[i] = map[string]string{
			"pattern":     r.Pattern,
			"category":    r.Category,
			"description": r.Description,
		}
	}
	return rules
}

// parseExcludeParam converts a comma-separated excludeCategories query param.
// "" → no exclusions; missing → default (System)
func parseExcludeParam(param string, present bool) []string {
	if !present {
		return defaultExcluded
	}
	if strings.TrimSpace(param) == "" {
		return nil
	}
	var cats []string
	for _, c := range strings.Split(param, ",") {
		if t := strings.TrimSpace(c); t != "" {
			cats = append(cats, t)
		}
	}
	return cats
}
