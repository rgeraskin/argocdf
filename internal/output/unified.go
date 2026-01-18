// Package output provides unified diff generation utilities.
package output

import (
	"fmt"
	"strings"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/rgeraskin/argocdf/internal/diff"
)

// GenerateUnifiedDiff creates a unified diff string from two YAML contents.
// The filename is used in the diff header for context.
func GenerateUnifiedDiff(oldYAML, newYAML, filename string) (string, error) {
	unifiedDiff := difflib.UnifiedDiff{
		A:        difflib.SplitLines(oldYAML),
		B:        difflib.SplitLines(newYAML),
		FromFile: "base/" + filename,
		ToFile:   "target/" + filename,
		Context:  3,
	}
	return difflib.GetUnifiedDiffString(unifiedDiff)
}

// GenerateManifestUnifiedDiffs generates unified diffs for all modified manifests.
// Returns a map of manifest key to unified diff string.
func GenerateManifestUnifiedDiffs(result *diff.ManifestSetDiff) (map[string]string, error) {
	diffs := make(map[string]string)

	// Generate diffs for modified manifests
	for _, md := range result.Modified {
		if md.Old != nil && md.New != nil {
			diffStr, err := GenerateUnifiedDiff(md.Old.Raw, md.New.Raw, md.Key)
			if err != nil {
				return nil, fmt.Errorf("failed to generate diff for %s: %w", md.Key, err)
			}
			if diffStr != "" {
				diffs[md.Key] = diffStr
			}
		}
	}

	// Generate diffs for added manifests (entire content is new)
	for _, m := range result.Added {
		diffStr, err := GenerateUnifiedDiff("", m.Raw, m.Key())
		if err != nil {
			return nil, fmt.Errorf("failed to generate diff for added %s: %w", m.Key(), err)
		}
		if diffStr != "" {
			diffs[m.Key()] = diffStr
		}
	}

	// Generate diffs for removed manifests (entire content is removed)
	for _, m := range result.Removed {
		diffStr, err := GenerateUnifiedDiff(m.Raw, "", m.Key())
		if err != nil {
			return nil, fmt.Errorf("failed to generate diff for removed %s: %w", m.Key(), err)
		}
		if diffStr != "" {
			diffs[m.Key()] = diffStr
		}
	}

	return diffs, nil
}

// CombineUnifiedDiffs combines multiple unified diffs into a single string.
// This is useful for feeding to diff2html or external diff tools.
func CombineUnifiedDiffs(diffs map[string]string, keys []string) string {
	var parts []string
	for _, key := range keys {
		if d, ok := diffs[key]; ok && d != "" {
			parts = append(parts, d)
		}
	}
	return strings.Join(parts, "\n")
}

// GetSortedKeys returns the keys from the diff map in sorted order.
func GetSortedKeys(result *diff.ManifestSetDiff) []string {
	var keys []string

	// Added first
	for _, m := range result.Added {
		keys = append(keys, m.Key())
	}

	// Then removed
	for _, m := range result.Removed {
		keys = append(keys, m.Key())
	}

	// Then modified
	for _, md := range result.Modified {
		keys = append(keys, md.Key)
	}

	return keys
}
