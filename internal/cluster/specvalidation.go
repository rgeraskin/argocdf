package cluster

import "fmt"

// ValidateSourceSpec reports why ArgoCD would REFUSE to render this application,
// or nil when its sources satisfy ArgoCD's own spec rules. It mirrors
// util/argo.validateSourcePermissions (argo.go:562) - the check the controller
// runs on a spec BEFORE it ever asks the repo-server for manifests.
//
// argocdf renders by calling reposerver/repository.GenerateManifests directly,
// which sits BELOW that check, so an application ArgoCD has already refused used
// to render here and report a clean diff. The shape that surfaced it was an
// OCI-ARTIFACT source carrying only repoURL + targetRevision: upstream's rule has
// no oci:// carve-out, so the controller stamped
//
//	InvalidSpecError: spec.source.repoURL and either spec.source.path or spec.source.chart are required
//
// on the resource and never rendered it, while argocdf reported "No changes" - the
// worst possible answer, since it describes an application that does not exist.
//
// Only the SOURCE rules are mirrored. Upstream's exported caller
// (ValidatePermissions, argo.go:616) cannot be used for them: it demands an
// *AppProject and a db.ArgoDB, and it additionally enforces IsSourcePermitted and
// IsDestinationPermitted - project and destination policy argocdf DELIBERATELY
// does not apply, having no AppProject context (DIFFERENCES.md §14, the
// "AppProject scoping" row). Mirroring is therefore duplication, and
// TestValidateSourceSpecMatchesUpstream is what keeps the copy honest: it feeds
// every invalid shape here to the real ValidatePermissions and asserts upstream
// still rejects it, which is the guard against upstream RELAXING a rule (adding
// the oci:// carve-out, say) while this mirror keeps refusing.
//
// Messages are upstream's verbatim wherever upstream's are usable, so a line in
// an argocdf report and the InvalidSpecError condition on the live resource read
// the same and grep the same. The multi-source ones are not usable: upstream
// interpolates the offending ApplicationSource through its generated String(),
// which dumps every nested helm parameter and value into the message - noise in a
// PR comment, and a route for a --set value to reach one. The source's POSITION
// identifies it here instead.
//
// The application is not named in the error. Every consumer already knows which
// application it is holding: processWave logs it as name=, and each writer prints
// it under the application's own heading.
func ValidateSourceSpec(app *Application) error {
	spec := &app.Spec

	// Upstream's switch, in upstream's order (argo.go:620-660). The order is
	// load-bearing for the first case: a source-hydrator app has no spec.source
	// and no spec.sources at all - GetSource() SYNTHESIZES one from drySource +
	// syncSource - so validating it as a single source would reject a hydrator
	// whose syncSource.path is empty, which ArgoCD accepts. Upstream validates
	// the hydrator's own fields instead, and so does this.
	switch {
	case spec.SourceHydrator != nil:
		return validateSourceHydrator(spec.SourceHydrator)
	case spec.HasMultipleSources():
		for i, source := range spec.Sources {
			if err := validateMultiSource(source, i); err != nil {
				return err
			}
		}
		return nil
	default:
		return validateSingleSource(spec.GetSource())
	}
}

// validateSingleSource mirrors validateSourcePermissions with
// hasMultipleSources=false. `ref` is not accepted here: a $ref source only means
// anything to the sibling sources of a multi-source app.
func validateSingleSource(source ApplicationSource) error {
	if source.RepoURL == "" || (source.Path == "" && source.Chart == "") {
		return invalidSpec("spec.source.repoURL and either spec.source.path or spec.source.chart are required")
	}
	if source.Chart != "" && source.TargetRevision == "" {
		return invalidSpec("spec.source.targetRevision is required if the manifest source is a helm chart")
	}

	return nil
}

// validateMultiSource mirrors validateSourcePermissions with
// hasMultipleSources=true, for the source at index i of spec.sources. A source
// that only carries `ref` is valid: it exists to be referenced as $name by a
// sibling's value files, and generates no manifests of its own.
func validateMultiSource(source ApplicationSource, i int) error {
	if source.RepoURL == "" || (source.Path == "" && source.Chart == "" && source.Ref == "") {
		return invalidSpec(fmt.Sprintf(
			"spec.sources[%d]: repoURL and either path, chart or ref are required", i))
	}
	if source.Chart != "" && source.TargetRevision == "" {
		return invalidSpec(fmt.Sprintf(
			"spec.sources[%d]: targetRevision is required if the manifest source is a helm chart", i))
	}

	return nil
}

// validateSourceHydrator mirrors util/argo.validateSourceHydrator (argo.go:590),
// the rules that replace the source rules for an application whose manifests come
// from a dry source hydrated into a sync branch. Upstream collects all three
// conditions; one error is enough here, since a report shows one error per
// application.
//
// The third message says `hydrateTo.path` while the field it guards is
// `hydrateTo.targetBranch`. That is upstream's wording, kept verbatim for the same
// reason as the others: this line is what a user finds on the live resource.
func validateSourceHydrator(hydrator *SourceHydrator) error {
	if hydrator.DrySource.RepoURL == "" {
		return invalidSpec("spec.sourceHydrator.drySource.repoURL is required")
	}
	if hydrator.SyncSource.TargetBranch == "" {
		return invalidSpec("spec.sourceHydrator.syncSource.targetBranch is required")
	}
	if hydrator.HydrateTo != nil && hydrator.HydrateTo.TargetBranch == "" {
		return invalidSpec("when spec.sourceHydrator.hydrateTo is set, spec.sourceHydrator.hydrateTo.path is required")
	}

	return nil
}

// invalidSpec wraps one upstream message into the error a report shows. The
// prefix says whose verdict it is: nothing in argocdf failed, and the fix is in
// the Application, not in the invocation.
func invalidSpec(reason string) error {
	return fmt.Errorf("invalid Application spec, ArgoCD would refuse it: %s", reason)
}
