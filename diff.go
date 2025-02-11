package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/charmbracelet/log"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	helmclient "github.com/mittwald/go-helm-client"
	helmclientValues "github.com/mittwald/go-helm-client/values"
	"github.com/sergi/go-diff/diffmatchpatch"
	"gopkg.in/yaml.v3"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/repo"
)

const (
	gitBranchMaster  = "main"
	gitBranchCurrent = "feature"
	gitRepoPath      = "/Users/rg/Projects/argocd/argocd_main/"
	gitRepoURL       = "https://github.com/asd/argocd-apps.git"
	kubeVersion      = "v1.23.10"
	kubeVersionMajor = "1"
	kubeVersionMinor = "23"
)

var logger *log.Logger

type KubeResource struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec interface{} `yaml:"spec"`
}

type HelmParameter struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type HelmFileParameter struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

type HelmSpec struct {
	ReleaseName    string              `yaml:"releaseName,omitempty"`
	FileParameters []HelmFileParameter `yaml:"fileParameters,omitempty"`
	Parameters     []HelmParameter     `yaml:"parameters,omitempty"`
	ValueFiles     []string            `yaml:"valueFiles,omitempty"`
	Values         string              `yaml:"values,omitempty"`
	// valueObjects is object with any structure inside
	ValueObjects []interface{} `yaml:"valueObjects,omitempty"`
}

type SourceSpec struct {
	Chart          string    `yaml:"chart,omitempty"`
	Path           string    `yaml:"path,omitempty"`
	TargetRevision string    `yaml:"targetRevision,omitempty"`
	RepoURL        string    `yaml:"repoURL"`
	Helm           *HelmSpec `yaml:"helm,omitempty"`
}

type DestinationSpec struct {
	Namespace string `yaml:"namespace"`
}

type ApplicationSpec struct {
	Destination DestinationSpec `yaml:"destination"`
	Source      SourceSpec      `yaml:"source,omitempty"`
	Sources     []SourceSpec    `yaml:"sources,omitempty"`
}

type Application struct {
	Metadata struct {
		Name   string `yaml:"name"`
		Labels struct {
			ArgoCDParent string `yaml:"argocd.argoproj.io/instance,omitempty"`
		} `yaml:"labels,omitempty"`
	} `yaml:"metadata"`
	Spec ApplicationSpec `yaml:"spec"`
}

type ApplicationList struct {
	Items []Application `yaml:"items"`
}

type App struct {
	Name              string
	ParentAppName     string
	ChildAppNames     []string
	ApplicationOld    *Application
	ApplicationNew    *Application
	Diff              []diffmatchpatch.Diff
	AffectedResources string
	RenderedOld       string
	RenderedNew       string
}

func execCommand(name string, args ...string) (string, error) {
	var stdout bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Dir = gitRepoPath
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	return stdout.String(), err
}

func hasChangesInPath(
	ApplicationSpec ApplicationSpec,
	repo *git.Repository,
	head *plumbing.Reference,
	mainRef *plumbing.Reference,
) (bool, error) {
	// Get commit objects
	headCommit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return false, fmt.Errorf("failed to get HEAD commit: %w", err)
	}

	mainCommit, err := repo.CommitObject(mainRef.Hash())
	if err != nil {
		return false, fmt.Errorf("failed to get main commit: %w", err)
	}

	// Get the patch between main and HEAD
	patch, err := mainCommit.Patch(headCommit)
	if err != nil {
		return false, fmt.Errorf("failed to get patch: %w", err)
	}

	// Helper function to check if a path contains changes
	contains := func(path string) bool {
		//
		if !strings.HasSuffix(path, "/") {
			path = path + "/"
		}

		for _, filePatch := range patch.FilePatches() {
			if filePatch.IsBinary() {
				continue
			}
			from, to := filePatch.Files()
			// Check both old and new paths
			if from != nil && strings.HasPrefix(from.Path(), path) {
				logger.Debug("changes detected in 'from' path", "path", path)
				return true
			}
			if to != nil && strings.HasPrefix(to.Path(), path) {
				logger.Debug("changes detected in 'to' path", "path", path)
				return true
			}
		}
		return false
	}

	if len(ApplicationSpec.Sources) > 0 {
		for _, source := range ApplicationSpec.Sources {
			if contains(source.Path) {
				return true, nil
			}
		}
	} else {
		return contains(ApplicationSpec.Source.Path), nil
	}

	return false, nil
}

func templateHelmChart(application *Application) ([]byte, error) {
	logger := logger.With("appName", application.Metadata.Name)

	optionsClient := &helmclient.Options{
		RepositoryCache:  "/tmp/.helmcache",
		RepositoryConfig: "/tmp/.helmrepo",
		Namespace:        application.Spec.Destination.Namespace,
		Output:           io.Discard,
	}
	hc, err := helmclient.New(optionsClient)
	if err != nil {
		return nil, fmt.Errorf("failed to create helm client: %w", err)
	}

	optionsTemplate := &helmclient.HelmTemplateOptions{
		KubeVersion: &chartutil.KubeVersion{
			Version: kubeVersion,
			Major:   kubeVersionMajor,
			Minor:   kubeVersionMinor,
		},
	}

	// check if release name is overridden
	var releaseName string
	if application.Spec.Source.Helm != nil && application.Spec.Source.Helm.ReleaseName != "" {
		logger.Info(
			"release name overridden",
			"releaseName",
			application.Spec.Source.Helm.ReleaseName,
		)
		releaseName = application.Spec.Source.Helm.ReleaseName
	} else {
		logger.Info("using default release name", "releaseName", application.Metadata.Name)
		releaseName = application.Metadata.Name
	}

	var chart, chartVersion string
	var valueFiles, fileValues []string
	if application.Spec.Source.Chart != "" {
		// use chart from remote repo
		logger.Info("using chart from remote repo", "chart", application.Spec.Source.Chart)
		repoURL := strings.TrimSuffix(application.Spec.Source.RepoURL, "/")
		if strings.HasPrefix(repoURL, "https://") {
			logger.Info("https repo", "repoURL", repoURL)
			chart = application.Spec.Source.Chart + "/" + application.Spec.Source.Chart
			repoEntry := repo.Entry{
				Name:               application.Spec.Source.Chart,
				URL:                repoURL,
				PassCredentialsAll: true,
			}
			logger.Info("adding chart repo", "repoURL", repoURL)
			if err := hc.AddOrUpdateChartRepo(repoEntry); err != nil {
				return nil, fmt.Errorf("failed to add chart repo: %w", err)
			}
		} else {
			logger.Info("oci repo", "repoURL", repoURL)
			chart = "oci://" + repoURL + "/" + chart
		}
		if application.Spec.Source.Helm != nil {
			// add value files from remote repo
			valueFiles = application.Spec.Source.Helm.ValueFiles
			// add file parameters from remote repo
			for _, fileParam := range application.Spec.Source.Helm.FileParameters {
				fileValues = append(
					fileValues,
					fmt.Sprintf("%s=%s", fileParam.Name, fileParam.Path),
				)
			}
		}
		if application.Spec.Source.TargetRevision != "HEAD" &&
			application.Spec.Source.TargetRevision != gitBranchMaster &&
			application.Spec.Source.TargetRevision != "" {
			chartVersion = application.Spec.Source.TargetRevision
		}
	} else if application.Spec.Source.Path != "" {
		// use chart from local path
		chart = filepath.Join(gitRepoPath, application.Spec.Source.Path)
		logger.Info("using chart from local path", "path", chart)
		if application.Spec.Source.Helm != nil {
			// add value files from local path
			for _, path := range application.Spec.Source.Helm.ValueFiles {
				valueFiles = append(valueFiles, filepath.Join(gitRepoPath, path))
			}
			// add file parameters from local path
			for _, fileParam := range application.Spec.Source.Helm.FileParameters {
				fileValues = append(fileValues, fmt.Sprintf("%s=%s", fileParam.Name, filepath.Join(gitRepoPath, fileParam.Path)))
			}
		}
	} else {
		return nil, fmt.Errorf("no chart specified")
	}

	if len(valueFiles) > 0 {
		logger.Info("value files", "valueFiles", valueFiles)
	}
	if len(fileValues) > 0 {
		logger.Info("file values", "fileValues", fileValues)
	}

	// prepare set values
	setValues := []string{}
	if application.Spec.Source.Helm != nil {
		for _, param := range application.Spec.Source.Helm.Parameters {
			setValues = append(setValues, fmt.Sprintf("%s=%s", param.Name, param.Value))
		}
	}

	if len(setValues) > 0 {
		logger.Info("set values", "setValues", setValues)
	}

	valuesOptions := helmclientValues.Options{
		Values:     setValues,
		ValueFiles: valueFiles,
		FileValues: fileValues,
	}

	chartSpec := helmclient.ChartSpec{
		ReleaseName:      releaseName,
		ChartName:        chart,
		Namespace:        application.Spec.Destination.Namespace,
		DependencyUpdate: true,
		ValuesOptions:    valuesOptions,
	}

	if chartVersion != "" {
		chartSpec.Version = chartVersion
	}

	logger.Info("template chart", "chart", chart)
	output, err := hc.TemplateChart(&chartSpec, optionsTemplate)
	if err != nil {
		return nil, fmt.Errorf("failed to template chart: %w", err)
	}
	return output, nil
}

func getAppType(ApplicationSpec ApplicationSpec) string {
	switch {
	case isHelmChart(ApplicationSpec):
		return "helm"
	case isKustomization(ApplicationSpec):
		return "kustomize"
	default:
		return "unknown"
	}
}

func isKustomization(ApplicationSpec ApplicationSpec) bool {
	logger.Debug("Checking if app is kustomization", "ApplicationSpec", ApplicationSpec)
	panic("unimplemented")
}

func isHelmChart(ApplicationSpec ApplicationSpec) bool {
	check := func(source SourceSpec) bool {
		logger.Debug("Checking if source is helm chart", "source", source)

		// check Chart.yaml exists
		switch {
		case source.Path != "":
			logger.Debug("Checking if Chart.yaml exists in path", "path", source.Path)
			chartPath := filepath.Join(gitRepoPath, source.Path, "Chart.yaml")
			if _, err := os.Stat(chartPath); err == nil {
				logger.Debug("Chart.yaml exists in path", "chartPath", chartPath)
				return true
			}
		case source.Chart != "":
			logger.Debug("source.Chart is set", "source.Chart", source.Chart)
			return true
		case source.Helm != nil:
			logger.Debug("source.Helm is set", "source.Helm", source.Helm)
			return true
		}

		return false
	}

	// if there are several sources, check if any of them is a helm chart
	if len(ApplicationSpec.Sources) > 0 {
		for _, source := range ApplicationSpec.Sources {
			if check(source) {
				return true
			}
		}
	}

	// if there is only one source, check if it is a helm chart
	return check(ApplicationSpec.Source)
}

func getGit(
	gitRepoPath string,
	gitBranchMaster string,
) (*git.Repository, *plumbing.Reference, *plumbing.Reference, error) {
	// Open the git repository
	repo, err := git.PlainOpen(gitRepoPath)
	if err != nil {
		logger.Fatal("failed to open repository", "error", err)
	}

	// Get the HEAD reference
	head, err := repo.Head()
	if err != nil {
		logger.Fatal("failed to get HEAD", "error", err)
	}

	// Get the main branch reference
	mainRefName := plumbing.NewBranchReferenceName(gitBranchMaster)
	mainRef, err := repo.Reference(mainRefName, true)
	if err != nil {
		logger.Fatal("failed to get main branch reference", "error", err)
	}

	return repo, head, mainRef, nil
}

func appsGetList() (ApplicationList, error) {
	// Get ArgoCD applications
	output, err := execCommand("kubectl", "get", "applications", "-n", "argocd", "-o", "yaml")
	if err != nil {
		return ApplicationList{}, fmt.Errorf("error getting ArgoCD applications: %w", err)
	}

	var appList ApplicationList
	if err := yaml.Unmarshal([]byte(output), &appList); err != nil {
		return ApplicationList{}, fmt.Errorf("error unmarshalling application list: %w", err)
	}

	return appList, nil
}

func appsGetChanged(
	appList ApplicationList,
	repo *git.Repository,
	head *plumbing.Reference,
	mainRef *plumbing.Reference,
) ([]Application, error) {
	// Process each application
	appsChanged := []Application{}
	for _, app := range appList.Items {
		// Skip if the application is not in the argocd repo
		// or if the target revision is not HEAD or master/main
		if app.Spec.Source.RepoURL != gitRepoURL ||
			(app.Spec.Source.TargetRevision != "HEAD" && app.Spec.Source.TargetRevision != gitBranchMaster) {
			continue
		}

		// Check if there are changes in the application
		hasChanges, err := hasChangesInPath(app.Spec, repo, head, mainRef)
		if err != nil {
			return nil, fmt.Errorf("error checking changes for app %s: %w", app.Metadata.Name, err)
		}

		if !hasChanges {
			continue
		}
		appsChanged = append(appsChanged, app)
	}

	appsChangedNames := []string{}
	for _, app := range appsChanged {
		appsChangedNames = append(appsChangedNames, app.Metadata.Name)
	}
	logger.Info("appsChanged", "appsChanged", appsChangedNames)
	return appsChanged, nil
}

func buildAppsMap(applicationsChanged []Application, appsMap *map[string]*App) error {
	logger.Info("building apps map")
	// Заполняем отображение name -> *App
	for _, application := range applicationsChanged {
		logger.Debug("adding app to map", "app", application.Metadata.Name)
		(*appsMap)[application.Metadata.Name] = &App{
			Name:           application.Metadata.Name,
			ApplicationOld: &application,
			// deep copy app
			ApplicationNew: &Application{
				Metadata: application.Metadata,
				Spec:     application.Spec,
			},
		}
	}

	// Устанавливаем связи родитель -> дети и дети -> родитель
	for _, application := range applicationsChanged {
		parentName := application.Metadata.Labels.ArgoCDParent
		if parentName != "" {
			parent, ok := (*appsMap)[parentName]
			if !ok {
				logger.Debug(
					"triggered parent not found",
					"parentName",
					parentName,
					"app",
					application.Metadata.Name,
				)
				continue
			}
			logger.Debug(
				"adding child app to parent",
				"parent",
				parent.ApplicationOld.Metadata.Name,
				"child",
				application.Metadata.Name,
			)
			parent.ChildAppNames = append(parent.ChildAppNames, application.Metadata.Name)

			child, ok := (*appsMap)[application.Metadata.Name]
			if !ok {
				return fmt.Errorf("child app not found, wtf??: %s", application.Metadata.Name)
			}
			child.ParentAppName = parent.ApplicationOld.Metadata.Name
		}
	}

	return nil
}

func printDiffs(appsMap *map[string]*App, parentAppName string) {
	dmp := diffmatchpatch.New()
	for _, app := range *appsMap {
		if app.ParentAppName == parentAppName {
			fmt.Println("# " + app.Name)
			fmt.Println(dmp.DiffPrettyText(app.Diff))
			if len(app.ChildAppNames) > 0 {
				fmt.Printf("# Children: %v\n", app.ChildAppNames)
				printDiffs(appsMap, app.Name)
			}
		}
	}
}

func findNewChildren(app *App, appsMap *map[string]*App) error {
	// decode yaml from app.AffectedResources
	logger.Debug("unmarshalling affected resources")

	parseYaml := func(yamlString string) ([]KubeResource, error) {
		decoder := yaml.NewDecoder(strings.NewReader(yamlString))
		var resources []KubeResource
		for {
			var resource KubeResource
			err := decoder.Decode(&resource)
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("failed to parse YAML: %w", err)
			}
			resources = append(resources, resource)
		}
		return resources, nil
	}

	kubeResourceToApplication := func(resource *KubeResource) (*Application, error) {
		// Serialize KubeResource
		yamlData, err := yaml.Marshal(resource)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal YAML: %w", err)
		}

		// Unmarshal KubeResource into Application
		var resourceApplication *Application
		if err := yaml.Unmarshal(yamlData, &resourceApplication); err != nil {
			return nil, fmt.Errorf("failed to unmarshal YAML: %w", err)
		}
		return resourceApplication, nil
	}

	affectedResources, err := parseYaml(app.AffectedResources)
	if err != nil {
		return fmt.Errorf("failed to parse affected resources: %w", err)
	}

	// find new children apps in affectedResources
	for _, resourceKube := range affectedResources {
		logger.Info(
			"affected resource",
			"name",
			resourceKube.Metadata.Name,
			"kind",
			resourceKube.Kind,
		)
		if resourceKube.Kind == "Application" {
			resourceApplication, err := kubeResourceToApplication(&resourceKube)
			if err != nil {
				return fmt.Errorf("failed to convert kube resource to application: %w", err)
			}

			if !slices.Contains(app.ChildAppNames, resourceApplication.Metadata.Name) {
				app.ChildAppNames = append(app.ChildAppNames, resourceApplication.Metadata.Name)
				(*appsMap)[resourceApplication.Metadata.Name] = &App{
					Name: resourceApplication.Metadata.Name,
				}
			}

			(*appsMap)[resourceApplication.Metadata.Name].ParentAppName = app.Name
			(*appsMap)[resourceApplication.Metadata.Name].ApplicationNew = resourceApplication
			if (*appsMap)[resourceApplication.Metadata.Name].ApplicationOld == nil {
				resourcesOld, err := parseYaml(app.RenderedOld)
				if err != nil {
					return fmt.Errorf("failed to parse rendered old: %w", err)
				}
				for _, resource := range resourcesOld {
					if resource.Metadata.Name == resourceApplication.Metadata.Name &&
						resource.Kind == "Application" {

						applicationOld, err := kubeResourceToApplication(&resource)
						if err != nil {
							return fmt.Errorf(
								"failed to convert kube resource to application: %w",
								err,
							)
						}
						(*appsMap)[resourceApplication.Metadata.Name].ApplicationOld = applicationOld
					}
				}
			}
		}
	}

	return nil
}

func getDiff(app *App) error {
	dmp := diffmatchpatch.New()
	diffs := dmp.DiffMain(
		app.RenderedOld,
		app.RenderedNew,
		true,
	)

	// make diff smaller
	for i, diff := range diffs {
		if diff.Type == diffmatchpatch.DiffEqual {
			// delete all text after the first '---' and before the last '---' in diff.Text
			firstHyphen := strings.Index(diff.Text, "---")
			lastHyphen := strings.LastIndex(diff.Text, "---")
			if firstHyphen != -1 && lastHyphen != -1 {
				diffs[i].Text = diff.Text[:firstHyphen] + diff.Text[lastHyphen:]
			}
		}
	}
	// delete last equal diff
	lastDiff := &diffs[len(diffs)-1]
	if lastDiff.Type == diffmatchpatch.DiffEqual {
		lastHyphen := strings.LastIndex(lastDiff.Text, "---")
		if lastHyphen != -1 {
			lastDiff.Text = lastDiff.Text[:lastHyphen]
		}
	}

	app.Diff = diffs
	app.AffectedResources = string(dmp.DiffText2(diffs))
	return nil
}

func renderAppsMap(
	appsMap *map[string]*App,
	parentAppName string,
	repo *git.Repository,
) error {
	for _, app := range *appsMap {
		if app.ParentAppName == parentAppName {
			// render app
			logger.Info("rendering app branches", "app", app.Name)
			err := renderBranches(
				repo,
				gitBranchCurrent,
				gitBranchMaster,
				app,
			)
			if err != nil {
				return fmt.Errorf("error rendering app: %w", err)
			}
			// get diff
			logger.Info("get diff", "app", app.Name)
			err = getDiff(app)
			if err != nil {
				return fmt.Errorf("error getting diff: %w", err)
			}
			// if diff affects resources with kind Application, find new children
			logger.Info("find new children", "app", app.Name)
			err = findNewChildren(app, appsMap)
			if err != nil {
				return fmt.Errorf("error finding new children: %w", err)
			}
			// render children
			if len(app.ChildAppNames) > 0 {
				logger.Info(
					"render children",
					"app",
					app.Name,
					"children",
					app.ChildAppNames,
				)
				for range app.ChildAppNames {
					err = renderAppsMap(appsMap, app.Name, repo)
					if err != nil {
						return fmt.Errorf("error rendering child: %w", err)
					}
				}
			}
		}
	}

	return nil
}

func renderApp(application *Application) (string, error) {
	appType := getAppType(application.Spec)
	logger.Info("Rendering app", "name", application.Metadata.Name, "type", appType)

	var renderedApp []byte
	var err error
	switch appType {
	case "helm":
		renderedApp, err = templateHelmChart(application)
		if err != nil {
			return "", fmt.Errorf("error templating helm chart: %w", err)
		}
	default:
		logger.Error("Unknown app type", "type", appType)
		return "", fmt.Errorf("unknown app type: %s", appType)
	}

	return string(renderedApp), nil
}

func renderBranches(
	repo *git.Repository,
	gitBranchCurrent, gitBranchMaster string,
	app *App,
) error {
	type branch struct {
		Application *Application
		Rendered    *string
	}
	// branches is a dictionary with branch name as key and branch as value
	branches := map[string]branch{
		gitBranchCurrent: {Application: app.ApplicationNew, Rendered: &app.RenderedNew},
		gitBranchMaster:  {Application: app.ApplicationOld, Rendered: &app.RenderedOld},
	}

	worktree, err := repo.Worktree()
	if err != nil {
		logger.Fatal("failed to get worktree", "error", err)
	}

	for branchName, opts := range branches {
		application := opts.Application
		rendered := opts.Rendered

		logger.Info("Processing branch", "branch", branchName)

		branchRefName := plumbing.NewBranchReferenceName(branchName)
		branchCoOpts := git.CheckoutOptions{
			Branch: branchRefName,
		}
		if err := worktree.Checkout(&branchCoOpts); err != nil {
			return fmt.Errorf("failed to checkout branch: %w", err)
		}

		renderedApp, err := renderApp(application)
		if err != nil {
			return fmt.Errorf("error rendering app: %w", err)
		}
		*rendered = renderedApp
	}
	// checkout to the original branch
	logger.Info("checkout to the original branch", "branch", gitBranchCurrent)
	branchRefName := plumbing.NewBranchReferenceName(gitBranchCurrent)
	branchCoOpts := git.CheckoutOptions{
		Branch: branchRefName,
	}
	if err := worktree.Checkout(&branchCoOpts); err != nil {
		logger.Fatal("failed to checkout branch", "error", err)
	}

	return nil
}

func main() {
	logger = log.NewWithOptions(os.Stderr, log.Options{
		// ReportCaller:    true,
		ReportTimestamp: true,
		Level:           log.DebugLevel,
		// Formatter:       log.LogfmtFormatter,
	})

	repo, head, mainRef, err := getGit(gitRepoPath, gitBranchMaster)
	if err != nil {
		logger.Fatal("failed to get git", "error", err)
	}

	appList, err := appsGetList()
	if err != nil {
		logger.Fatal("failed to get argo cd apps", "error", err)
	}

	applicationsChanged, err := appsGetChanged(appList, repo, head, mainRef)
	if err != nil {
		logger.Fatal("failed to get changed apps", "error", err)
	}

	// build apps dependency map
	appsMap := map[string]*App{}
	err = buildAppsMap(applicationsChanged, &appsMap)
	if err != nil {
		logger.Fatal("failed to build apps map", "error", err)
	}

	err = renderAppsMap(&appsMap, "", repo)
	if err != nil {
		logger.Fatal("failed to render apps map", "error", err)
	}

	printDiffs(&appsMap, "")
}
