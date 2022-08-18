package controller

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	api "github.com/akuityio/k8sta/api/v1alpha1"
	argocd "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (t *ticketReconciler) promoteImages(
	ctx context.Context,
	ticket *api.Ticket,
	app *argocd.Application,
) (string, error) {
	// Create a temporary home directory for everything we're about to do
	homeDir, err := ioutil.TempDir("", "")
	if err != nil {
		return "", errors.Wrapf(
			err,
			"error creating temporary workspace for cloning repo %q",
			app.Spec.Source.RepoURL,
		)
	}
	// defer os.RemoveAll(homeDir)
	t.logger.WithFields(log.Fields{
		"path": homeDir,
	}).Debug("created temporary home directory")

	if err =
		t.setupGitAuth(ctx, app.Spec.Source.RepoURL, homeDir); err != nil {
		return "", errors.Wrapf(
			err,
			"error setting up authentication for repo %q",
			app.Spec.Source.RepoURL,
		)
	}

	// Clone the repo
	repoDir := filepath.Join(homeDir, "repo")
	cmd := exec.Command( // nolint: gosec
		"git",
		"clone",
		"--no-tags",
		app.Spec.Source.RepoURL,
		repoDir,
	)
	if _, err = t.execGitCommand(cmd, homeDir); err != nil {
		return "", errors.Wrapf(
			err,
			"error cloning repo %q into %q",
			app.Spec.Source.RepoURL,
			repoDir,
		)
	}
	t.logger.WithFields(log.Fields{
		"path": repoDir,
		"repo": app.Spec.Source.RepoURL,
	}).Debug("cloned git repository")

	// TODO: This is hard-coded for now, but there's a possibility here of later
	// supporting other tools and patterns.
	sha, err := t.promotionStrategyRenderedYAMLBranchesWithKustomize(
		ctx,
		ticket,
		app,
		homeDir,
		repoDir,
	)
	if err != nil {
		return "", err
	}

	// Force the Argo CD Application to refresh and sync?
	patch := client.MergeFrom(app.DeepCopy())
	app.ObjectMeta.Annotations[argocd.AnnotationKeyRefresh] =
		string(argocd.RefreshTypeHard)
	app.Operation = &argocd.Operation{
		Sync: &argocd.SyncOperation{
			Revision: app.Spec.Source.TargetRevision,
		},
	}
	if err = t.client.Patch(ctx, app, patch, &client.PatchOptions{}); err != nil {
		t.logger.Debugf("----> %s", err)
		return "", errors.Wrapf(
			err,
			"error patching Argo CD Application %q to coerce refresh and sync",
			app.Name,
		)
	}
	t.logger.WithFields(log.Fields{
		"app": app.Name,
	}).Debug("triggered refresh of Argo CD Application")

	return sha, nil
}

// setupGitAuth, if necessary, configures the git CLI for authentication using
// either SSH or the "store" (username/password-based) credential helper.
func (t *ticketReconciler) setupGitAuth(
	ctx context.Context,
	repoURL string,
	homeDir string,
) error {
	// Configure the git client
	cmd := exec.Command("git", "config", "--global", "user.name", "k8sta")
	if _, err := t.execGitCommand(cmd, homeDir); err != nil {
		return errors.Wrapf(err, "error configuring git username")
	}
	cmd = exec.Command(
		"git",
		"config",
		"--global",
		"user.email",
		"k8sta@akuity.io",
	)
	if _, err := t.execGitCommand(cmd, homeDir); err != nil {
		return errors.Wrapf(err, "error configuring git user email address")
	}

	const repoTypeGit = "git"
	var sshKey, username, password string
	// NB: This next call returns an empty Repository if no such Repository is
	// found, so instead of continuing to look for credentials if no Repository is
	// found, what we'll do is continue looking for credentials if the Repository
	// we get back doesn't have anything we can use, i.e. no SSH private key or
	// password.
	repo, err := t.argoDB.GetRepository(ctx, repoURL)
	if err != nil {
		return errors.Wrapf(
			err,
			"error getting Repository (Secret) for repo %q",
			repoURL,
		)
	}
	if repo.Type == repoTypeGit || repo.Type == "" {
		sshKey = repo.SSHPrivateKey
		username = repo.Username
		password = repo.Password
	}
	if sshKey == "" && password == "" {
		// We didn't find any creds yet, so keep looking
		var repoCreds *argocd.RepoCreds
		repoCreds, err = t.argoDB.GetRepositoryCredentials(ctx, repoURL)
		if err != nil {
			return errors.Wrapf(
				err,
				"error getting Repository Credentials (Secret) for repo %q",
				repoURL,
			)
		}
		if repoCreds.Type == repoTypeGit || repoCreds.Type == "" {
			sshKey = repo.SSHPrivateKey
			username = repo.Username
			password = repo.Password
		}
	}

	// We didn't find any creds, so we're done. We can't promote without creds.
	if sshKey == "" && password == "" {
		return errors.Errorf("could not find any credentials for repo %q", repoURL)
	}

	// If an SSH key was provided, use that.
	if sshKey != "" {
		sshConfigPath := filepath.Join(homeDir, ".ssh", "config")
		// nolint: lll
		const sshConfig = "Host *\n  StrictHostKeyChecking no\n  UserKnownHostsFile=/dev/null"
		if err =
			ioutil.WriteFile(sshConfigPath, []byte(sshConfig), 0600); err != nil {
			return errors.Wrapf(err, "error writing SSH config to %q", sshConfigPath)
		}

		rsaKeyPath := filepath.Join(homeDir, ".ssh", "id_rsa")
		if err = ioutil.WriteFile(rsaKeyPath, []byte(sshKey), 0600); err != nil {
			return errors.Wrapf(err, "error writing SSH key to %q", rsaKeyPath)
		}
		return nil // We're done
	}

	// If we get to here, we're authenticating using a password

	// Set up the credential helper
	cmd = exec.Command("git", "config", "--global", "credential.helper", "store")
	if _, err = t.execGitCommand(cmd, homeDir); err != nil {
		return errors.Wrapf(err, "error configuring git credential helper")
	}

	credentialURL, err := url.Parse(repoURL)
	if err != nil {
		return errors.Wrapf(err, "error parsing URL %q", repoURL)
	}
	// Remove path and query string components from the URL
	credentialURL.Path = ""
	credentialURL.RawQuery = ""
	// If the username is the empty string, we assume we're working with a git
	// provider like GitHub that only requires the username to be non-empty. We
	// arbitrarily set it to "git".
	if username == "" {
		username = "git"
	}
	// Augment the URL with user/pass information.
	credentialURL.User = url.UserPassword(username, password)
	// Write the augmented URL to the location used by the "stored" credential
	// helper.
	credentialsPath := filepath.Join(homeDir, ".git-credentials")
	if err := ioutil.WriteFile(
		credentialsPath,
		[]byte(credentialURL.String()),
		0600,
	); err != nil {
		return errors.Wrapf(
			err,
			"error writing credentials to %q",
			credentialsPath,
		)
	}
	return nil
}

// nolint: gocyclo
func (t *ticketReconciler) promotionStrategyRenderedYAMLBranchesWithKustomize(
	ctx context.Context,
	ticket *api.Ticket,
	app *argocd.Application,
	homeDir string,
	repoDir string,
) (string, error) {
	loggerFields := log.Fields{
		"repo":   app.Spec.Source.RepoURL,
		"branch": app.Spec.Source.TargetRevision,
	}

	// We assume the Application-specific overlay path within the source branch ==
	// the name of the Application-specific branch that the final rendered YAML
	// will live in.
	// TODO: Nothing enforced this assumption yet.
	appDir := filepath.Join(repoDir, app.Spec.Source.TargetRevision)

	// Set the image
	for _, image := range ticket.Change.NewImages.Images {
		cmd := exec.Command( // nolint: gosec
			"kustomize",
			"edit",
			"set",
			"image",
			fmt.Sprintf(
				"%s=%s:%s",
				image.Repo,
				image.Repo,
				image.Tag,
			),
		)
		cmd.Dir = appDir // We need to be in the overlay directory to do this
		if err := cmd.Run(); err != nil {
			return "", errors.Wrap(err, "error setting image")
		}
		loggerFields["imageRepo"] = image.Repo
		loggerFields["imageTag"] = image.Tag
		t.logger.WithFields(loggerFields).Debug("ran kustomize edit set image")
	}

	delete(loggerFields, "imageRepo")
	delete(loggerFields, "imageTag")

	// Render Application-specific YAML
	// TODO: We may need to buffer this or use a file instead because the rendered
	// YAML could be quite large.
	cmd := exec.Command("kustomize", "build")
	cmd.Dir = appDir // We need to be in the overlay directory to do this
	yamlBytes, err := cmd.Output()
	if err != nil {
		return "",
			errors.Wrapf(
				err,
				"error rendering YAML for branch %q",
				app.Spec.Source.TargetRevision,
			)
	}
	t.logger.WithFields(loggerFields).Debug("rendered Application-specific YAML")

	// Commit the changes to the source branch
	var commitMsg string
	if len(ticket.Change.NewImages.Images) == 1 {
		commitMsg = fmt.Sprintf(
			"k8sta: updating %s to use image %s:%s",
			app.Spec.Source.TargetRevision,
			ticket.Change.NewImages.Images[0].Repo,
			ticket.Change.NewImages.Images[0].Tag,
		)
	} else {
		commitMsg = "k8sta: updating %s to use new images"
		for _, image := range ticket.Change.NewImages.Images {
			commitMsg = fmt.Sprintf(
				"%s\n * %s:%s",
				commitMsg,
				image.Repo,
				image.Tag,
			)
		}
	}
	cmd = exec.Command("git", "commit", "-am", commitMsg)
	cmd.Dir = repoDir // We need to be in the root of the repo for this
	if _, err = t.execGitCommand(cmd, homeDir); err != nil {
		return "", errors.Wrap(err, "error committing changes to source branch")
	}
	t.logger.WithFields(loggerFields).Debug(
		"committed changes to the source branch",
	)

	// Push the changes to the source branch
	cmd = exec.Command("git", "push", "origin", "HEAD")
	cmd.Dir = repoDir // We need to be anywhere in the root of the repo for this
	if _, err = t.execGitCommand(cmd, homeDir); err != nil {
		return "", errors.Wrap(err, "error pushing changes to source branch")
	}
	t.logger.WithFields(loggerFields).Debug("pushed changes to the source branch")

	// Check if the Application-specific branch exists on the remote
	appBranchExists := true
	cmd = exec.Command( // nolint: gosec
		"git",
		"ls-remote",
		"--heads",
		"--exit-code", // Return 2 if not found
		app.Spec.Source.RepoURL,
		app.Spec.Source.TargetRevision,
	)
	// We need to be anywhere in the root of the repo for this
	cmd.Dir = repoDir
	if _, err = t.execGitCommand(cmd, homeDir); err != nil { // nolint: gosec
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 2 {
			return "", errors.Wrapf(
				err,
				"error checking for existence of Application-specific branch %q "+
					"from repo %q",
				app.Spec.Source.TargetRevision,
				app.Spec.Source.RepoURL,
			)
		}
		// If we get to here, exit code was 2 and that means the branch doesn't
		// exist
		appBranchExists = false
	}

	if appBranchExists {
		// Switch to the Application-specific branch
		cmd = exec.Command( // nolint: gosec
			"git",
			"checkout",
			app.Spec.Source.TargetRevision,
			// The next line makes it crystal clear to git that we're checking out
			// a branch. We need to do this since we operate under an assumption that
			// the path to the overlay within the repo == the branch name.
			"--",
		)
		cmd.Dir = repoDir // We need to be anywhere in the root of the repo for this
		if _, err = t.execGitCommand(cmd, homeDir); err != nil {
			return "", errors.Wrapf(
				err,
				"error checking out Application-specific branch %q from repo %q",
				app.Spec.Source.TargetRevision,
				app.Spec.Source.RepoURL,
			)
		}
		t.logger.WithFields(loggerFields).Debug(
			"checked out Application-specific branch",
		)
	} else {
		// Create the Application-specific branch
		cmd = exec.Command( // nolint: gosec
			"git",
			"checkout",
			"--orphan",
			app.Spec.Source.TargetRevision,
			// The next line makes it crystal clear to git that we're checking out
			// a branch. We need to do this since we operate under an assumption that
			// the path to the overlay within the repo == the branch name.
			"--",
		)
		cmd.Dir = repoDir // We need to be anywhere in the root of the repo for this
		if _, err = t.execGitCommand(cmd, homeDir); err != nil {
			return "", errors.Wrapf(
				err,
				"error creating orphaned Application-specific branch %q from repo %q",
				app.Spec.Source.TargetRevision,
				app.Spec.Source.RepoURL,
			)
		}
		t.logger.WithFields(loggerFields).Debug(
			"created Application-specific branch",
		)
	}

	// Remove existing rendered YAML (or files from the source branch that were
	// left behind when the orphaned Application-specific branch was created)
	files, err := filepath.Glob(filepath.Join(repoDir, "*"))
	if err != nil {
		return "", errors.Wrapf(
			err,
			"error listing files in Application-specific branch %q",
			app.Spec.Source.TargetRevision,
		)
	}
	for _, file := range files {
		if _, fileName := filepath.Split(file); fileName == ".git" {
			continue
		}
		if err = os.RemoveAll(file); err != nil {
			return "", errors.Wrapf(
				err,
				"error deleting file %q from Application-specific branch %q",
				file,
				app.Spec.Source.TargetRevision,
			)
		}
	}
	t.logger.WithFields(loggerFields).Debug("removed existing rendered YAML")

	// Write the new rendered YAML
	if err = os.WriteFile( // nolint: gosec
		filepath.Join(repoDir, "all.yaml"),
		yamlBytes,
		0644,
	); err != nil {
		return "", errors.Wrapf(
			err,
			"error writing rendered YAML to Application-specific branch %q",
			app.Spec.Source.TargetRevision,
		)
	}
	t.logger.WithFields(loggerFields).Debug("wrote new rendered YAML")

	// Commit the changes to the Application-specific branch
	commitMsg = ""
	if len(ticket.Change.NewImages.Images) == 1 {
		commitMsg = fmt.Sprintf(
			"k8sta: updating to use new image %s:%s",
			ticket.Change.NewImages.Images[0].Repo,
			ticket.Change.NewImages.Images[0].Tag,
		)
	} else {
		commitMsg = "k8sta: updating to use new images"
		for _, image := range ticket.Change.NewImages.Images {
			commitMsg = fmt.Sprintf(
				"%s\n * %s:%s",
				commitMsg,
				image.Repo,
				image.Tag,
			)
		}
	}
	cmd = exec.Command("git", "add", ".")
	cmd.Dir = repoDir // We need to be in the root of the repo for this
	if _, err = t.execGitCommand(cmd, homeDir); err != nil {
		return "", errors.Wrapf(
			err,
			"error staging changes for commit to Application-specific branch %q",
			app.Spec.Source.TargetRevision,
		)
	}
	cmd = exec.Command("git", "commit", "-m", commitMsg)
	cmd.Dir = repoDir // We need to be in the root of the repo for this
	if _, err = t.execGitCommand(cmd, homeDir); err != nil {
		return "", errors.Wrapf(
			err,
			"error committing changes to Application-specific branch %q",
			app.Spec.Source.TargetRevision,
		)
	}
	t.logger.WithFields(loggerFields).Debug(
		"committed changes to Application-specific branch",
	)

	// Push the changes to the Application-specific branch
	cmd = exec.Command( // nolint: gosec
		"git",
		"push",
		"origin",
		app.Spec.Source.TargetRevision,
	)
	cmd.Dir = repoDir // We need to be anywhere in the root of the repo for this
	if _, err = t.execGitCommand(cmd, homeDir); err != nil {
		return "", errors.Wrapf(
			err,
			"error pushing changes to Application-specific branch %q",
			app.Spec.Source.TargetRevision,
		)
	}
	t.logger.WithFields(loggerFields).Debug(
		"pushed changes to Application-specific branch",
	)

	// Get the ID of the last commit
	cmd = exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repoDir // We need to be anywhere in the root of the repo for this
	shaBytes, err := cmd.Output()
	if err != nil {
		return "", errors.Wrapf(
			err,
			"error obtaining last commit ID for branch %q",
			app.Spec.Source.TargetRevision,
		)
	}
	sha := strings.TrimSpace(string(shaBytes))
	t.logger.WithFields(loggerFields).Debug(
		"obtained sha of commit to Application-specific branch",
	)
	return sha, nil
}

func (t *ticketReconciler) execGitCommand(
	cmd *exec.Cmd,
	homeDir string,
) ([]byte, error) {
	homeEnvVar := fmt.Sprintf("HOME=%s", homeDir)
	if cmd.Env == nil {
		cmd.Env = []string{homeEnvVar}
	} else {
		cmd.Env = append(cmd.Env, homeEnvVar)
	}
	output, err := cmd.CombinedOutput()
	t.logger.Debug(string(output))
	return output, err
}
