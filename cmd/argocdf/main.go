package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/charmbracelet/log"
	"github.com/sergi/go-diff/diffmatchpatch"
)

const (
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
