package helm

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"helm.sh/helm/v3/pkg/chartutil"
	"k8s.io/helm/pkg/repo"
)

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