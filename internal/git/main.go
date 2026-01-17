package git

import (
	"fmt"

	"github.com/go-git/go-git"
	"gopkg.in/src-d/go-git.v4/plumbing"
)

const (
	gitBranchMaster  = "main"
	gitBranchCurrent = "feature"
	gitRepoPath      = "/Users/rg/Projects/argocd/argocd_main/"
	gitRepoURL       = "https://github.com/asd/argocd-apps.git"
)

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