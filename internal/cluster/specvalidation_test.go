package cluster

import (
	"context"
	"strings"
	"testing"

	argoapp "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/util/argo"
	argodb "github.com/argoproj/argo-cd/v3/util/db"
)

// specCase is one Application shape and the verdict both validations must reach.
//
// wantErr is the substring cluster.ValidateSourceSpec's error must carry ("" for a
// spec it must accept), and upstreamMessage is the InvalidSpecError message
// ArgoCD's own validation produces for the same shape - the drift pin below runs
// every shape, accepted and rejected, past upstream and compares. It is empty for
// the accepted shapes, where the expectation is that upstream produces NO
// condition at all.
type specCase struct {
	name            string
	spec            ApplicationSpec
	wantErr         string
	upstreamMessage string
}

func specCases() []specCase {
	return []specCase{
		{
			name: "single git source with a path",
			spec: ApplicationSpec{Source: &ApplicationSource{
				RepoURL: "https://github.com/org/repo.git", Path: "charts/web", TargetRevision: "main",
			}},
		},
		{
			name: "single helm chart source with a target revision",
			spec: ApplicationSpec{Source: &ApplicationSource{
				RepoURL: "https://charts.example.com", Chart: "web", TargetRevision: "1.2.3",
			}},
		},
		{
			name: "helm chart source without a target revision",
			spec: ApplicationSpec{Source: &ApplicationSource{
				RepoURL: "https://charts.example.com", Chart: "web",
			}},
			wantErr:         "spec.source.targetRevision is required if the manifest source is a helm chart",
			upstreamMessage: "spec.source.targetRevision is required if the manifest source is a helm chart",
		},
		{
			// The artifact spelling as it must be written: `path` selects a
			// directory INSIDE the pulled artifact, and `.` is its root.
			name: "oci artifact source with a path",
			spec: ApplicationSpec{Source: &ApplicationSource{
				RepoURL: "oci://registry.example.com/artifacts/web", TargetRevision: "1.2.3", Path: ".",
			}},
		},
		{
			// The shape that motivated this validation: a live ArgoCD 3.3.11
			// controller stamped exactly upstreamMessage on such an application
			// and never rendered it, while argocdf reported a clean diff.
			// Upstream's rule has no oci:// carve-out.
			name: "oci artifact source without a path",
			spec: ApplicationSpec{Source: &ApplicationSource{
				RepoURL: "oci://registry.example.com/artifacts/web", TargetRevision: "1.2.3",
			}},
			wantErr:         "spec.source.repoURL and either spec.source.path or spec.source.chart are required",
			upstreamMessage: "spec.source.repoURL and either spec.source.path or spec.source.chart are required",
		},
		{
			name: "single source without a repo url",
			spec: ApplicationSpec{Source: &ApplicationSource{
				Path: "charts/web", TargetRevision: "main",
			}},
			wantErr:         "spec.source.repoURL and either spec.source.path or spec.source.chart are required",
			upstreamMessage: "spec.source.repoURL and either spec.source.path or spec.source.chart are required",
		},
		{
			name:            "no source at all",
			spec:            ApplicationSpec{},
			wantErr:         "spec.source.repoURL and either spec.source.path or spec.source.chart are required",
			upstreamMessage: "spec.source.repoURL and either spec.source.path or spec.source.chart are required",
		},
		{
			// `ref` earns a source nothing but a name its siblings can use, so it
			// is not a substitute for path/chart when there ARE no siblings.
			name: "single source carrying only a ref",
			spec: ApplicationSpec{Source: &ApplicationSource{
				RepoURL: "https://github.com/org/values.git", TargetRevision: "main", Ref: "values",
			}},
			wantErr:         "spec.source.repoURL and either spec.source.path or spec.source.chart are required",
			upstreamMessage: "spec.source.repoURL and either spec.source.path or spec.source.chart are required",
		},
		{
			// The mirror of the case above: among siblings, a ref-only source is
			// exactly how ArgoCD spells "values live over here".
			name: "multi source with a ref-only entry",
			spec: ApplicationSpec{Sources: []ApplicationSource{
				{RepoURL: "https://charts.example.com", Chart: "web", TargetRevision: "1.2.3"},
				{RepoURL: "https://github.com/org/values.git", TargetRevision: "main", Ref: "values"},
			}},
		},
		{
			name: "multi source entry with neither path, chart nor ref",
			spec: ApplicationSpec{Sources: []ApplicationSource{
				{RepoURL: "https://charts.example.com", Chart: "web", TargetRevision: "1.2.3"},
				{RepoURL: "https://github.com/org/values.git", TargetRevision: "main"},
			}},
			wantErr: "spec.sources[1]: repoURL and either path, chart or ref are required",
			// Upstream interpolates the whole ApplicationSource here, so only the
			// fixed head of its message can be compared.
			upstreamMessage: "spec.source.repoURL and either source.path, source.chart, or source.ref are required for source ",
		},
		{
			name: "multi source chart entry without a target revision",
			spec: ApplicationSpec{Sources: []ApplicationSource{
				{RepoURL: "https://charts.example.com", Chart: "web"},
				{RepoURL: "https://github.com/org/values.git", TargetRevision: "main", Ref: "values"},
			}},
			wantErr:         "spec.sources[0]: targetRevision is required if the manifest source is a helm chart",
			upstreamMessage: "spec.source.targetRevision is required if the manifest source is a helm chart",
		},
		{
			// A source-hydrator app has no source to validate - GetSource()
			// SYNTHESIZES one whose Path is syncSource.path - so an empty
			// syncSource.path must not be read as a missing path. Upstream
			// validates the hydrator's own fields instead, and this is the shape
			// that fails if the switch in ValidateSourceSpec loses its order.
			name: "source hydrator without a sync path",
			spec: ApplicationSpec{SourceHydrator: &SourceHydrator{
				DrySource:  DrySource{RepoURL: "https://github.com/org/repo.git", TargetRevision: "main", Path: "manifests"},
				SyncSource: SyncSource{TargetBranch: "env/prod"},
			}},
		},
		{
			name: "source hydrator without a dry repo url",
			spec: ApplicationSpec{SourceHydrator: &SourceHydrator{
				DrySource:  DrySource{TargetRevision: "main", Path: "manifests"},
				SyncSource: SyncSource{TargetBranch: "env/prod", Path: "."},
			}},
			wantErr:         "spec.sourceHydrator.drySource.repoURL is required",
			upstreamMessage: "spec.sourceHydrator.drySource.repoURL is required",
		},
		{
			name: "source hydrator without a sync target branch",
			spec: ApplicationSpec{SourceHydrator: &SourceHydrator{
				DrySource:  DrySource{RepoURL: "https://github.com/org/repo.git", TargetRevision: "main", Path: "manifests"},
				SyncSource: SyncSource{Path: "."},
			}},
			wantErr:         "spec.sourceHydrator.syncSource.targetBranch is required",
			upstreamMessage: "spec.sourceHydrator.syncSource.targetBranch is required",
		},
		{
			name: "source hydrator with a hydrateTo missing its target branch",
			spec: ApplicationSpec{SourceHydrator: &SourceHydrator{
				DrySource:  DrySource{RepoURL: "https://github.com/org/repo.git", TargetRevision: "main", Path: "manifests"},
				SyncSource: SyncSource{TargetBranch: "env/prod", Path: "."},
				HydrateTo:  &argoapp.HydrateTo{},
			}},
			wantErr:         "when spec.sourceHydrator.hydrateTo is set, spec.sourceHydrator.hydrateTo.path is required",
			upstreamMessage: "when spec.sourceHydrator.hydrateTo is set, spec.sourceHydrator.hydrateTo.path is required",
		},
	}
}

// TestValidateSourceSpec pins the mirror of ArgoCD's source-spec rules, shape by
// shape. The motivating one is "oci artifact source without a path": upstream
// applies the path-or-chart rule to oci:// sources like any other, so such an
// application is an InvalidSpecError the controller never renders - and argocdf
// used to report a clean diff for it.
func TestValidateSourceSpec(t *testing.T) {
	for _, tt := range specCases() {
		t.Run(tt.name, func(t *testing.T) {
			app := &Application{Spec: tt.spec}
			app.Name = "test-app"
			app.Namespace = "argocd"

			err := ValidateSourceSpec(app)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateSourceSpec() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateSourceSpec() = nil, want an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("ValidateSourceSpec() = %q, want it to contain %q", err, tt.wantErr)
			}
			// The verdict is ArgoCD's, and the message says so - a user reading a
			// report line has to know the fix belongs in the Application.
			if !strings.Contains(err.Error(), "ArgoCD would refuse it") {
				t.Errorf("ValidateSourceSpec() = %q, want it to attribute the verdict to ArgoCD", err)
			}
		})
	}
}

// TestValidateSourceSpecMatchesUpstream is the drift pin that makes mirroring
// ArgoCD's rules safe instead of a guess. Every shape in the table - accepted and
// rejected alike - is handed to ArgoCD's OWN exported validation, and the two
// verdicts must agree:
//
//   - a shape argocdf REJECTS must still produce upstream's InvalidSpecError,
//     carrying the message this package quotes. This guards upstream RELAXING a
//     rule (adding the oci:// carve-out this whole change exists for, say) while
//     the mirror keeps refusing an application ArgoCD renders happily.
//   - a shape argocdf ACCEPTS must produce NO condition. This guards the other
//     direction, upstream ADDING a rule the mirror does not know about, which
//     would otherwise be invisible: no test can enumerate rules that do not exist
//     yet, but upstream itself can be asked.
//
// The second direction is why this runs against a project and a database rather
// than the nil-argument trick that only reaches the rejection path. See
// upstreamConditions.
func TestValidateSourceSpecMatchesUpstream(t *testing.T) {
	for _, tt := range specCases() {
		t.Run(tt.name, func(t *testing.T) {
			conditions, err := upstreamConditions(t, tt.spec)
			if err != nil {
				t.Fatalf("argo.ValidatePermissions() error = %v, want nil (a rejection is a CONDITION, not an error)", err)
			}

			if tt.wantErr == "" {
				if len(conditions) != 0 {
					t.Fatalf("argo.ValidatePermissions() = %v, want no conditions: upstream now rejects a shape "+
						"cluster.ValidateSourceSpec accepts, so the mirror is missing a rule", conditions)
				}
				return
			}

			if len(conditions) == 0 {
				t.Fatalf("argo.ValidatePermissions() returned no conditions: upstream no longer rejects this shape, "+
					"so ValidateSourceSpec now refuses an application ArgoCD accepts (argocdf says: %q)", tt.wantErr)
			}
			if conditions[0].Type != argoapp.ApplicationConditionInvalidSpecError {
				t.Errorf("condition type = %q, want %q", conditions[0].Type, argoapp.ApplicationConditionInvalidSpecError)
			}
			// The MESSAGE is what is asserted, and the type alone would make this
			// weaker than it looks: several unrelated refusals upstream can make
			// on this path are also InvalidSpecError. Only the text ties the
			// verdict to the source rules this package quotes.
			//
			// Compared by PREFIX for one reason: upstream's multi-source message
			// ends in the whole offending ApplicationSource. For every other shape
			// the recorded prefix is the entire message.
			if !strings.HasPrefix(conditions[0].Message, tt.upstreamMessage) {
				t.Errorf("upstream message = %q, want it to start with %q (the text this package quotes)",
					conditions[0].Message, tt.upstreamMessage)
			}
		})
	}
}

// permissiveDB answers the two questions ValidatePermissions asks a database once
// source validation has PASSED, and nothing else: every other method is the
// embedded nil interface and panics if upstream's path ever reaches it, which
// upstreamConditions reports as the drift it would be.
type permissiveDB struct{ argodb.ArgoDB }

func (permissiveDB) GetCluster(_ context.Context, server string) (*argoapp.Cluster, error) {
	return &argoapp.Cluster{Server: server}, nil
}

func (permissiveDB) GetProjectClusters(_ context.Context, _ string) ([]*argoapp.Cluster, error) {
	return nil, nil
}

// upstreamConditions runs ArgoCD's own ValidatePermissions over a spec and returns
// the conditions it produces, with everything that is NOT a source rule neutralized
// so the verdict is about source shape alone:
//
//   - the project permits every repository and every destination. That is not
//     decoration in either half. Upstream's MULTI-source loop returns early on a
//     source condition but merely APPENDS on a project denial (argo.go:632-645), so
//     a restrictive project would prepend "not permitted in project ”" and the
//     source verdict would not be the first condition; and an ACCEPTED shape would
//     collect a project condition and look rejected. `*` also happens to be the
//     policy argocdf renders under anyway (DIFFERENCES.md §14).
//   - the destination resolves. An accepted shape runs on to GetDestinationCluster,
//     and a spec with no reachable cluster fails there with an InvalidSpecError of
//     its own - indistinguishable, by type, from the source verdict this asserts.
//
// What is deliberately NOT neutralized is the source rules, which is the whole
// point: whatever conditions come back describe those.
func upstreamConditions(t *testing.T, spec ApplicationSpec) (conditions []argoapp.ApplicationCondition, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("argo.ValidatePermissions() panicked (%v): its path now reaches a database method "+
				"permissiveDB does not implement, so this pin no longer compares what it claims to", r)
		}
	}()

	spec.Destination = ApplicationDestination{
		Server:    "https://kubernetes.default.svc",
		Namespace: "default",
	}
	permissive := &argoapp.AppProject{Spec: argoapp.AppProjectSpec{
		SourceRepos:  []string{"*"},
		Destinations: []argoapp.ApplicationDestination{{Server: "*", Namespace: "*"}},
	}}

	return argo.ValidatePermissions(context.Background(), &spec, permissive, permissiveDB{})
}
