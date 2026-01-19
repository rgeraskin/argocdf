package git

import "testing"

func TestNormalizeRepoURL(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		// HTTPS URLs
		{
			name:     "https URL unchanged",
			input:    "https://github.com/owner/repo",
			expected: "https://github.com/owner/repo",
		},
		{
			name:     "https URL with .git suffix",
			input:    "https://github.com/owner/repo.git",
			expected: "https://github.com/owner/repo",
		},
		{
			name:     "https URL with trailing slash",
			input:    "https://github.com/owner/repo/",
			expected: "https://github.com/owner/repo",
		},
		{
			name:     "https URL with both .git and trailing slash",
			input:    "https://github.com/owner/repo.git/",
			expected: "https://github.com/owner/repo",
		},

		// SSH URLs (git@host:path format)
		{
			name:     "git@ SSH URL converted to https",
			input:    "git@github.com:owner/repo",
			expected: "https://github.com/owner/repo",
		},
		{
			name:     "git@ SSH URL with .git suffix",
			input:    "git@github.com:owner/repo.git",
			expected: "https://github.com/owner/repo",
		},
		{
			name:     "git@ SSH URL with nested path",
			input:    "git@gitlab.com:group/subgroup/repo.git",
			expected: "https://gitlab.com/group/subgroup/repo",
		},

		// SSH URLs (ssh:// format)
		{
			name:     "ssh:// URL converted to https",
			input:    "ssh://git@github.com/owner/repo",
			expected: "https://github.com/owner/repo",
		},
		{
			name:     "ssh:// URL with .git suffix",
			input:    "ssh://git@github.com/owner/repo.git",
			expected: "https://github.com/owner/repo",
		},
		{
			name:     "ssh:// URL without user",
			input:    "ssh://github.com/owner/repo",
			expected: "https://github.com/owner/repo",
		},

		// HTTP URLs
		{
			name:     "http URL unchanged",
			input:    "http://github.com/owner/repo",
			expected: "http://github.com/owner/repo",
		},
		{
			name:     "http URL with .git suffix",
			input:    "http://github.com/owner/repo.git",
			expected: "http://github.com/owner/repo",
		},

		// Edge cases
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "URL with port",
			input:    "https://github.com:443/owner/repo.git",
			expected: "https://github.com:443/owner/repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := NormalizeRepoURL(tt.input)
			if result != tt.expected {
				t.Errorf("NormalizeRepoURL(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}
