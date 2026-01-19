// Package testutil provides test utilities including mock implementations.
package testutil

import (
	"github.com/rgeraskin/argocdf/internal/diff"
	"github.com/rgeraskin/argocdf/internal/output"
	"github.com/rgeraskin/argocdf/internal/types"
)

// MockWriter implements output.Writer for testing.
type MockWriter struct {
	HeaderCalls     []string
	AppDiffCalls    []*types.AppDiff
	TreeCalls       []*diff.AppTree
	SummaryCalls    []output.Summary
	FooterCalled    bool
	FlushCalled     bool
	WriteHeaderErr  error
	WriteAppDiffErr error
	WriteTreeErr    error
	WriteSummaryErr error
	WriteFooterErr  error
	FlushErr        error
}

// NewMockWriter creates a new MockWriter.
func NewMockWriter() *MockWriter {
	return &MockWriter{}
}

// WriteHeader records the call.
func (m *MockWriter) WriteHeader(title string) error {
	m.HeaderCalls = append(m.HeaderCalls, title)
	return m.WriteHeaderErr
}

// WriteAppDiff records the call.
func (m *MockWriter) WriteAppDiff(appDiff *types.AppDiff, depth int) error {
	m.AppDiffCalls = append(m.AppDiffCalls, appDiff)
	return m.WriteAppDiffErr
}

// WriteTree records the call.
func (m *MockWriter) WriteTree(tree *diff.AppTree) error {
	m.TreeCalls = append(m.TreeCalls, tree)
	return m.WriteTreeErr
}

// WriteSummary records the call.
func (m *MockWriter) WriteSummary(summary output.Summary) error {
	m.SummaryCalls = append(m.SummaryCalls, summary)
	return m.WriteSummaryErr
}

// WriteFooter records the call.
func (m *MockWriter) WriteFooter() error {
	m.FooterCalled = true
	return m.WriteFooterErr
}

// Flush records the call.
func (m *MockWriter) Flush() error {
	m.FlushCalled = true
	return m.FlushErr
}

// MockAppDiscoverer implements app discovery for testing.
type MockAppDiscoverer struct {
	NewApps         []diff.DiscoveredApplication
	ModifiedApps    []diff.ModifiedApplication
	FindNewErr      error
	FindModifiedErr error
}

// NewMockAppDiscoverer creates a new MockAppDiscoverer.
func NewMockAppDiscoverer() *MockAppDiscoverer {
	return &MockAppDiscoverer{}
}

// DiscoverApplications returns configured new apps.
func (m *MockAppDiscoverer) DiscoverApplications(content string) ([]diff.DiscoveredApplication, error) {
	return m.NewApps, nil
}

// FindNewApplications returns configured new apps.
func (m *MockAppDiscoverer) FindNewApplications(oldContent, newContent string) ([]diff.DiscoveredApplication, error) {
	return m.NewApps, m.FindNewErr
}

// FindModifiedApplications returns configured modified apps.
func (m *MockAppDiscoverer) FindModifiedApplications(oldContent, newContent string) ([]diff.ModifiedApplication, error) {
	return m.ModifiedApps, m.FindModifiedErr
}
