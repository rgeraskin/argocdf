package argocd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-git/go-git"
	"github.com/go-git/go-git/plumbing"
	"gopkg.in/yaml.v2"
)

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
