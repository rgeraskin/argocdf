package app

import (
	"io"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/log"

	"github.com/rgeraskin/argocdf/internal/config"
)

// TestCreateRenderFactoryErrorReturnsUntypedNil pins the interface-nil
// contract: on a construction failure the returned applicationRenderer must
// be a true nil interface, not a typed-nil *ArgoCDRenderer wrapped in a
// non-nil interface — Run's deferred cleanup type-asserts the stored value
// and would otherwise call Cleanup on a nil receiver and panic during error
// unwinding.
func TestCreateRenderFactoryErrorReturnsUntypedNil(t *testing.T) {
	// The argocd engine's first construction step creates its registry auth
	// dir under os.TempDir(); pointing TMPDIR at a missing path forces the
	// failure without any stubbing.
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))

	f := NewFactory(&config.Config{Renderer: config.RendererArgoCD}, log.New(io.Discard))
	r, err := f.CreateRenderFactory("v1.30.0", nil, nil)
	if err == nil {
		t.Fatal("CreateRenderFactory() succeeded; the TMPDIR trick no longer forces a construction failure")
	}
	if r != nil {
		t.Fatalf("CreateRenderFactory() returned a non-nil interface (%T) alongside the error; Run's deferred Cleanup would panic on the typed nil", r)
	}
}
