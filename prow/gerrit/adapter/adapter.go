/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package adapter implements a controller that interacts with gerrit instances
package adapter

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/andygrunwald/go-gerrit"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	prowapi "k8s.io/test-infra/prow/apis/prowjobs/v1"
	prowv1 "k8s.io/test-infra/prow/client/clientset/versioned/typed/prowjobs/v1"
	"k8s.io/test-infra/prow/config"
	reporter "k8s.io/test-infra/prow/crier/reporters/gerrit"
	"k8s.io/test-infra/prow/gerrit/client"
	"k8s.io/test-infra/prow/pjutil"
)

type prowJobClient interface {
	Create(context.Context, *prowapi.ProwJob, metav1.CreateOptions) (*prowapi.ProwJob, error)
}

type gerritClient interface {
	QueryChanges(lastState client.LastSyncState, rateLimit int) map[string][]client.ChangeInfo
	GetBranchRevision(instance, project, branch string) (string, error)
	SetReview(instance, id, revision, message string, labels map[string]string) error
	Account(instance string) (*gerrit.AccountInfo, error)
}

// Controller manages gerrit changes.
type Controller struct {
	config        config.Getter
	prowJobClient prowJobClient
	gc            gerritClient
	tracker       LastSyncTracker
}

type LastSyncTracker interface {
	Current() client.LastSyncState
	Update(client.LastSyncState) error
}

// NewController returns a new gerrit controller client
func NewController(lastSyncTracker LastSyncTracker, gc gerritClient, prowJobClient prowv1.ProwJobInterface, cfg config.Getter) *Controller {
	return &Controller{
		prowJobClient: prowJobClient,
		config:        cfg,
		gc:            gc,
		tracker:       lastSyncTracker,
	}
}

// Sync looks for newly made gerrit changes
// and creates prowjobs according to specs
func (c *Controller) Sync() error {
	syncTime := c.tracker.Current()
	latest := syncTime.DeepCopy()

	for instance, changes := range c.gc.QueryChanges(syncTime, c.config().Gerrit.RateLimit) {
		log := logrus.WithField("host", instance)
		for _, change := range changes {
			log := log.WithFields(logrus.Fields{
				"branch":   change.Branch,
				"change":   change.Number,
				"repo":     change.Project,
				"revision": change.CurrentRevision,
			})
			if err := c.processChange(log, instance, change); err != nil {
				log.WithError(err).Errorf("Failed to process change")
			}
			lastTime, ok := latest[instance][change.Project]
			if !ok || lastTime.Before(change.Updated.Time) {
				lastTime = change.Updated.Time
				latest[instance][change.Project] = lastTime
			}
		}

		log.Infof("Processed %d changes", len(changes))
	}

	return c.tracker.Update(latest)
}

func makeCloneURI(instance, project string) (*url.URL, error) {
	u, err := url.Parse(instance)
	if err != nil {
		return nil, fmt.Errorf("instance %s is not a url: %v", instance, err)
	}
	if u.Host == "" {
		return nil, errors.New("instance does not set host")
	}
	if u.Path != "" {
		return nil, errors.New("instance cannot set path (this is set by project)")
	}
	u.Path = project
	return u, nil
}

// listChangedFiles lists (in lexicographic order) the files changed as part of a Gerrit patchset
func listChangedFiles(changeInfo client.ChangeInfo) config.ChangedFilesProvider {
	return func() ([]string, error) {
		var changed []string
		revision := changeInfo.Revisions[changeInfo.CurrentRevision]
		for file := range revision.Files {
			changed = append(changed, file)
		}
		return changed, nil
	}
}

func createRefs(reviewHost string, change client.ChangeInfo, cloneURI *url.URL, baseSHA string) (prowapi.Refs, error) {
	rev, ok := change.Revisions[change.CurrentRevision]
	if !ok {
		return prowapi.Refs{}, fmt.Errorf("cannot find current revision for change %v", change.ID)
	}
	var codeHost string // Something like https://android.googlesource.com
	parts := strings.SplitN(reviewHost, ".", 2)
	codeHost = strings.TrimSuffix(parts[0], "-review")
	if len(parts) > 1 {
		codeHost += "." + parts[1]
	}
	refs := prowapi.Refs{
		Org:      cloneURI.Host,  // Something like android-review.googlesource.com
		Repo:     change.Project, // Something like platform/build
		BaseRef:  change.Branch,
		BaseSHA:  baseSHA,
		CloneURI: cloneURI.String(), // Something like https://android-review.googlesource.com/platform/build
		RepoLink: fmt.Sprintf("%s/%s", codeHost, change.Project),
		BaseLink: fmt.Sprintf("%s/%s/+/%s", codeHost, change.Project, baseSHA),
		Pulls: []prowapi.Pull{
			{
				Number:     change.Number,
				Author:     rev.Commit.Author.Name,
				SHA:        change.CurrentRevision,
				Ref:        rev.Ref,
				Link:       fmt.Sprintf("%s/c/%s/+/%d", reviewHost, change.Project, change.Number),
				CommitLink: fmt.Sprintf("%s/%s/+/%s", codeHost, change.Project, change.CurrentRevision),
				AuthorLink: fmt.Sprintf("%s/q/%s", reviewHost, rev.Commit.Author.Email),
			},
		},
	}
	return refs, nil
}

// failedJobs find jobs currently reported as failing (used for retesting).
//
// Failing means the job is complete and not passing.
// Scans messages for prow reports, which lists jobs and whether they passed.
// Job is included in the set if the latest report has it failing.
func failedJobs(account int, revision int, messages ...gerrit.ChangeMessageInfo) sets.String {
	failures := sets.String{}
	times := map[string]time.Time{}
	for _, message := range messages {
		if message.Author.AccountID != account { // Ignore reports from other accounts
			continue
		}
		if message.RevisionNumber != revision { // Ignore reports for old commits
			continue
		}
		// TODO(fejta): parse triggered job reports and remove from failure set.
		// (alternatively refactor this whole process rely less on fragile string parsing)
		report := reporter.ParseReport(message.Message)
		if report == nil {
			continue
		}
		for _, job := range report.Jobs {
			name := job.Name
			if latest, present := times[name]; present && message.Date.Before(latest) {
				continue
			}
			times[name] = message.Date.Time
			if job.State == prowapi.FailureState || job.State == prowapi.ErrorState {
				failures.Insert(name)
			} else {
				failures.Delete(name)
			}
		}
	}
	return failures
}

// processChange creates new presubmit/postsubmit prowjobs base off the gerrit changes
func (c *Controller) processChange(logger logrus.FieldLogger, instance string, change client.ChangeInfo) error {
	var refs prowapi.Refs
	cloneURI, err := makeCloneURI(instance, change.Project)
	if err != nil {
		return fmt.Errorf("makeCloneURI: %w", err)
	}

	type triggeredJob struct {
		name   string
		report bool
	}
	var triggeredJobs []triggeredJob

	type jobSpec struct {
		spec   prowapi.ProwJobSpec
		labels map[string]string
	}

	var jobSpecs []jobSpec

	changedFiles := listChangedFiles(change)

	switch change.Status {
	case client.Merged:
		// TODO: Do we want to add support for dynamic postsubmits?
		postsubmits := c.config().PostsubmitsStatic[cloneURI.String()]
		refs, err = createRefs(instance, change, cloneURI, "")
		if err != nil {
			return fmt.Errorf("createRefs from %s: %w", cloneURI, err)
		}
		postsubmits = append(postsubmits, c.config().PostsubmitsStatic[cloneURI.Host+"/"+cloneURI.Path]...)
		for _, postsubmit := range postsubmits {
			if shouldRun, err := postsubmit.ShouldRun(change.Branch, changedFiles); err != nil {
				return fmt.Errorf("failed to determine if postsubmit %q should run: %v", postsubmit.Name, err)
			} else if shouldRun {
				postsubmitSpec := pjutil.PostsubmitSpec(postsubmit, refs)
				// For postsubmit jobs, cloneref ignores `Refs.Pulls` because this is empty for github,
				// for gerrit based repos, the commit SHA of the change is the same commit SHA merged into
				// base branch, so using this for postsubmit jobs.
				// Ref: https://gerrit-review.googlesource.com/Documentation/rest-api-changes.html#create-merge-patch-set-for-change
				if count := len(postsubmitSpec.Refs.Pulls); count == 1 {
					postsubmitSpec.Refs.BaseSHA = postsubmitSpec.Refs.Pulls[0].SHA
					postsubmitSpec.Refs.Pulls = []prowapi.Pull{}
				} else {
					logger.WithField("Pulls", count).Warn("Refs.Pulls should be exactly 1 for gerrit repos")
				}
				jobSpecs = append(jobSpecs, jobSpec{
					spec:   postsubmitSpec,
					labels: postsubmit.Labels,
				})
			}
		}
	case client.New:
		// TODO: Do we want to add support for dynamic presubmits?
		presubmits := c.config().PresubmitsStatic[cloneURI.String()]
		baseSHA, err := c.gc.GetBranchRevision(instance, change.Project, change.Branch)
		if err != nil {
			return fmt.Errorf("GetBranchRevision: %w", err)
		}
		refs, err = createRefs(instance, change, cloneURI, baseSHA)
		if err != nil {
			return fmt.Errorf("createRefs from %s at %s: %w", cloneURI, baseSHA, err)
		}
		presubmits = append(presubmits, c.config().PresubmitsStatic[cloneURI.Host+"/"+cloneURI.Path]...)

		account, err := c.gc.Account(instance)
		if err != nil {
			// This would happen if authenticateOnce hasn't done register this instance yet
			return fmt.Errorf("account not found for %q: %v", instance, err)
		}

		lastUpdate, ok := c.tracker.Current()[instance][change.Project]
		if !ok {
			lastUpdate = time.Now()
			logger.WithField("lastUpdate", lastUpdate).Warnf("lastUpdate not found, falling back to now")
		}

		revision := change.Revisions[change.CurrentRevision]
		failedJobs := failedJobs(account.AccountID, revision.Number, change.Messages...)
		failed, all := presubmitContexts(failedJobs, presubmits, logger)
		messages := currentMessages(change, lastUpdate)
		filters := []pjutil.Filter{
			messageFilter(messages, failed, all, logger),
		}
		// Automatically trigger the Prow jobs if the revision is new and is not
		// draft.
		if revision.Created.Time.After(lastUpdate) && !revision.Draft {
			filters = append(filters, pjutil.TestAllFilter())
		}
		toTrigger, err := pjutil.FilterPresubmits(pjutil.AggregateFilter(filters), listChangedFiles(change), change.Branch, presubmits, logger)
		if err != nil {
			return fmt.Errorf("filter presubmits: %w", err)
		}

		// Reply with help information to run the presubmit Prow jobs if requested.
		for _, msg := range messages {
			needsHelp, note := pjutil.ShouldRespondWithHelp(msg, len(toTrigger))
			if needsHelp {
				runWithTestAllNames, runWithTriggerNames, err := pjutil.AvailablePresubmits(listChangedFiles(change), cloneURI.Host, change.Project, change.Branch, presubmits, logger.WithField("help", true))
				if err != nil {
					return err
				}
				message := pjutil.HelpMessage(cloneURI.Host, change.Project, change.Branch, note, runWithTestAllNames, runWithTriggerNames)
				if err := c.gc.SetReview(instance, change.ID, change.CurrentRevision, message, nil); err != nil {
					return err
				}
				// Only respond to the first message that requests help information.
				break
			}
		}

		for _, presubmit := range toTrigger {
			jobSpecs = append(jobSpecs, jobSpec{
				spec:   pjutil.PresubmitSpec(presubmit, refs),
				labels: presubmit.Labels,
			})
		}
	}

	annotations := map[string]string{
		client.GerritID:       change.ID,
		client.GerritInstance: instance,
	}

	for _, jSpec := range jobSpecs {
		labels := make(map[string]string)
		for k, v := range jSpec.labels {
			labels[k] = v
		}
		labels[client.GerritRevision] = change.CurrentRevision
		labels[client.GerritPatchset] = strconv.Itoa(change.Revisions[change.CurrentRevision].Number)

		if _, ok := labels[client.GerritReportLabel]; !ok {
			labels[client.GerritReportLabel] = client.CodeReview
		}

		pj := pjutil.NewProwJob(jSpec.spec, labels, annotations)
		logger := logger.WithField("prowJob", pj)
		if _, err := c.prowJobClient.Create(context.TODO(), &pj, metav1.CreateOptions{}); err != nil {
			logger.WithError(err).Errorf("Failed to create ProwJob")
			continue
		}
		logger.Infof("Triggered new job")
		triggeredJobs = append(triggeredJobs, triggeredJob{
			name:   jSpec.spec.Job,
			report: jSpec.spec.Report,
		})
	}

	if len(triggeredJobs) == 0 {
		return nil
	}

	// comment back to gerrit if Report is set for any of the jobs
	var reportingJobs int
	var jobList string
	for _, job := range triggeredJobs {
		if job.report {
			jobList += fmt.Sprintf("\n  * Name: %s", job.name)
			reportingJobs++
		}
	}

	if reportingJobs > 0 {
		message := fmt.Sprintf("Triggered %d prow jobs (%d suppressed reporting): ", len(triggeredJobs), len(triggeredJobs)-reportingJobs)
		// If we have a Deck URL, link to all results for the CL, otherwise list the triggered jobs.
		link, err := deckLinkForPR(c.config().Gerrit.DeckURL, refs, change.Status)
		if err != nil {
			logger.WithError(err).Error("Failed to generate link to job results on Deck.")
		}
		if link != "" && err == nil {
			message = message + link
		} else {
			message = message + jobList
		}
		if err := c.gc.SetReview(instance, change.ID, change.CurrentRevision, message, nil); err != nil {
			return err
		}
	}

	return nil
}

func deckLinkForPR(deckURL string, refs prowapi.Refs, changeStatus string) (string, error) {
	if deckURL == "" || changeStatus == client.Merged {
		return "", nil
	}

	parsed, err := url.Parse(deckURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse gerrit.deck_url (impossible: this should have been caught at load time): %v", err)
	}
	query := parsed.Query()
	query.Set("repo", fmt.Sprintf("%s/%s", refs.Org, refs.Repo))
	if len(refs.Pulls) != 1 {
		return "", fmt.Errorf("impossible: triggered jobs for a Gerrit change, but refs.pulls was empty")
	}
	query.Set("pull", strconv.Itoa(refs.Pulls[0].Number))
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
