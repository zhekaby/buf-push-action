// Copyright 2020-2022 Buf Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bufpushaction

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/bufbuild/buf-push-action/internal/pkg/github"
	"github.com/bufbuild/buf/private/buf/bufcli"
	"github.com/bufbuild/buf/private/bufpkg/bufmodule"
	"github.com/bufbuild/buf/private/bufpkg/bufmodule/bufmoduleref"
	"github.com/bufbuild/buf/private/gen/proto/apiclient/buf/alpha/registry/v1alpha1/registryv1alpha1apiclient"
	"github.com/bufbuild/buf/private/pkg/app/appflag"
	"github.com/bufbuild/buf/private/pkg/command"
	"github.com/bufbuild/buf/private/pkg/rpc"
)

func push(ctx context.Context, container appflag.Container, eventName string) error {
	refType := container.Env(githubRefTypeKey)
	if refType != githubRefTypeBranch {
		writeNotice(
			container.Stdout(),
			fmt.Sprintf("Skipping because %q events are not supported with %q references", eventName, refType),
		)
		return nil
	}
	input := container.Env(inputInput)
	storageosProvider := bufcli.NewStorageosProvider(false)
	runner := command.NewRunner()
	module, moduleIdentity, err := bufcli.ReadModuleWithWorkspacesDisabled(
		ctx,
		container,
		storageosProvider,
		runner,
		input,
	)
	if err != nil {
		return err
	}
	protoModule, err := bufmodule.ModuleToProtoModule(ctx, module)
	if err != nil {
		return err
	}
	track := container.Env(trackInput)
	if track == "" {
		return fmt.Errorf("track not provided")
	}
	defaultBranch := container.Env(defaultBranchInput)
	if defaultBranch == "" {
		return fmt.Errorf("default_branch not provided")
	}
	refName := container.Env(githubRefNameKey)
	// Error when track is main and not overridden but the default branch is not main.
	// This is for situations where the default branch is something like master and there
	// is also a main branch. It prevents the main track from having commits from multiple git branches.
	if defaultBranch != "main" && track == "main" && track == refName {
		return errors.New("cannot push to main track from a non-default branch")
	}
	track = resolveTrack(track, defaultBranch, refName)
	registryProvider, err := getRegistryProvider(ctx, container)
	if err != nil {
		return err
	}
	tags, err := getTags(ctx, registryProvider, moduleIdentity, track)
	if err != nil {
		return err
	}
	ghClient, err := getGithubClient(ctx, container)
	if err != nil {
		return err
	}
	currentGitCommit := container.Env(githubSHAKey)
	if currentGitCommit == "" {
		return errors.New("current git commit not found in environment")
	}
	for _, tag := range tags {
		var status github.CompareCommitsStatus
		status, err = ghClient.CompareCommits(ctx, tag, currentGitCommit)
		if err != nil {
			if github.IsNotFoundError(err) {
				continue
			}
			return err
		}
		switch status {
		case github.CompareCommitsStatusIdentical:
			writeNotice(
				container.Stdout(),
				fmt.Sprintf("Skipping because the current git commit is already the head of track %s", track),
			)
			return nil
		case github.CompareCommitsStatusBehind:
			writeNotice(
				container.Stdout(),
				fmt.Sprintf("Skipping because the current git commit is behind the head of track %s", track),
			)
			return nil
		case github.CompareCommitsStatusDiverged:
			writeNotice(
				container.Stdout(),
				fmt.Sprintf("The current git commit is diverged from the head of track %s", track),
			)
		case github.CompareCommitsStatusAhead:
		default:
			return fmt.Errorf("unexpected status: %s", status)
		}
	}
	pushService, err := registryProvider.NewPushService(ctx, moduleIdentity.Remote())
	if err != nil {
		return err
	}
	localModulePin, err := pushService.Push(
		ctx,
		moduleIdentity.Owner(),
		moduleIdentity.Repository(),
		"",
		protoModule,
		[]string{currentGitCommit},
		[]string{track},
	)
	if err != nil {
		if rpc.GetErrorCode(err) != rpc.ErrorCodeAlreadyExists {
			return err
		}
		if err := tagExistingCommit(ctx, registryProvider, moduleIdentity, currentGitCommit, track); err != nil {
			return err
		}
	}

	setOutput(container.Stdout(), commitOutputID, localModulePin.Commit)
	setOutput(container.Stdout(), commitURLOutputID, fmt.Sprintf(
		"https://%s/tree/%s",
		moduleIdentity.IdentityString(),
		localModulePin.Commit,
	))

	return nil
}

func tagExistingCommit(
	ctx context.Context,
	registryProvider registryv1alpha1apiclient.Provider,
	moduleIdentity bufmoduleref.ModuleIdentity,
	tagName string,
	reference string,
) error {
	repositoryService, err := registryProvider.NewRepositoryService(ctx, moduleIdentity.Remote())
	if err != nil {
		return err
	}
	repository, _, err := repositoryService.GetRepositoryByFullName(ctx, moduleIdentity.Owner()+"/"+moduleIdentity.Repository())
	if err != nil {
		if rpc.GetErrorCode(err) == rpc.ErrorCodeNotFound {
			return fmt.Errorf("a repository named %q does not exist", moduleIdentity.IdentityString())
		}
		return err
	}
	repositoryTagService, err := registryProvider.NewRepositoryTagService(ctx, moduleIdentity.Remote())
	if err != nil {
		return err
	}
	_, err = repositoryTagService.CreateRepositoryTag(ctx, repository.Id, tagName, reference)
	if err != nil {
		if rpc.GetErrorCode(err) == rpc.ErrorCodeNotFound {
			return fmt.Errorf("%s:%s does not exist", moduleIdentity.IdentityString(), reference)
		}
		if rpc.GetErrorCode(err) == rpc.ErrorCodeAlreadyExists {
			return fmt.Errorf("%s:%s already exists with different content", moduleIdentity.IdentityString(), tagName)
		}
		return err
	}
	return nil
}

func getTags(
	ctx context.Context,
	registryProvider registryv1alpha1apiclient.Provider,
	moduleIdentity bufmoduleref.ModuleIdentity,
	track string,
) ([]string, error) {
	repositoryCommitService, err := registryProvider.NewRepositoryCommitService(ctx, moduleIdentity.Remote())
	if err != nil {
		return nil, err
	}
	repositoryCommit, err := repositoryCommitService.GetRepositoryCommitByReference(
		ctx,
		moduleIdentity.Owner(),
		moduleIdentity.Repository(),
		track,
	)
	if err != nil {
		return nil, err
	}
	tags := make([]string, 0, len(repositoryCommit.Tags))
	for _, tag := range repositoryCommit.Tags {
		tagName := tag.Name
		if len(tagName) != 40 {
			continue
		}
		if _, err := hex.DecodeString(tagName); err != nil {
			continue
		}
		tags = append(tags, tagName)
	}
	return tags, nil
}

// getGithubClient returns the github client from the context if one is present or creates a client.
func getGithubClient(ctx context.Context, container appflag.Container) (githubClient, error) {
	githubToken := container.Env(githubTokenInput)
	if githubToken == "" {
		return nil, errors.New("a github authentication token was not provided")
	}
	githubRepository := container.Env(githubRepositoryKey)
	if githubRepository == "" {
		return nil, errors.New("a github repository was not provided")
	}
	repoParts := strings.Split(githubRepository, "/")
	if len(repoParts) != 2 {
		return nil, errors.New("a github repository was not provided in the format owner/repo")
	}
	var err error
	var client githubClient
	client, err = github.NewClient(ctx, githubToken, "buf-push-action", "", githubRepository)
	if err != nil {
		return nil, err
	}
	if value, ok := ctx.Value(githubClientContextKey).(githubClient); ok {
		client = value
	}
	return client, nil
}
