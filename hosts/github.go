package hosts

import (
	"context"
	"strconv"
	"strings"
	"sync"

	"github.com/zricethezav/gitleaks/audit"
	"github.com/zricethezav/gitleaks/manager"
	"github.com/zricethezav/gitleaks/options"

	"github.com/google/go-github/github"
	log "github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
)

// Github wraps a github client and manager. This struct implements what the Host interface defines.
type Github struct {
	client  *github.Client
	manager manager.Manager
	wg      sync.WaitGroup
}

// NewGithubClient accepts a manager struct and returns a Github host pointer which will be used to
// perform a github audit on an organization, user, or PR.
func NewGithubClient(m manager.Manager) (*Github, error) {
	var err error
	ctx := context.Background()
	token := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: options.GetAccessToken(m.Opts)},
	)

	var githubClient *github.Client
	httpClient := oauth2.NewClient(ctx, token)

	if m.Opts.BaseURL == "" {
		githubClient = github.NewClient(httpClient)
	} else {
		githubClient, err = github.NewEnterpriseClient(m.Opts.BaseURL, m.Opts.BaseURL, httpClient)
	}

	return &Github{
		manager: m,
		client:  githubClient,
	}, err
}

// Audit will audit a github user or organization's repos.
func (g *Github) Audit() {
	ctx := context.Background()
	listOptions := github.ListOptions{
		PerPage: 100,
		Page:    1,
	}

	var githubRepos []*github.Repository

	for {
		var (
			_githubRepos []*github.Repository
			resp         *github.Response
			err          error
		)
		if g.manager.Opts.User != "" {
			_githubRepos, resp, err = g.client.Repositories.List(ctx, g.manager.Opts.User,
				&github.RepositoryListOptions{ListOptions: listOptions})
		} else if g.manager.Opts.Organization != "" {
			_githubRepos, resp, err = g.client.Repositories.ListByOrg(ctx, g.manager.Opts.Organization,
				&github.RepositoryListByOrgOptions{ListOptions: listOptions})
		}
		githubRepos = append(githubRepos, _githubRepos...)

		if resp == nil {
			break
		}

		if resp.LastPage != 0 {
			log.Infof("gathering github repos... progress: page %d of %d", listOptions.Page, resp.LastPage)
		} else {
			log.Infof("gathering github repos... progress: page %d of %d", listOptions.Page, listOptions.Page)
		}

		listOptions.Page = resp.NextPage
		if err != nil || listOptions.Page == 0 {
			break
		}
	}

	for _, repo := range githubRepos {
		r := audit.NewRepo(&g.manager)
		r.Name = *repo.Name

		auth, err := options.SSHAuth(g.manager.Opts)
		if err != nil {
			log.Warnf("unable to get ssh auth, skipping clone and audit for repo %s: %+v\n", *repo.CloneURL, err)
		}
		err = r.Clone(&git.CloneOptions{
			URL:   *repo.SSHURL,
			Auth:  auth,
			Depth: 1,
		})
		if err != nil {
			log.Warnf("err cloning %s, skipping clone and audit: %+v\n", *repo.SSHURL, err)
		}

		if err = r.Audit(); err != nil {
			log.Warn(err)
		}
	}
}

// AuditPR audits a single github PR
func (g *Github) AuditPR() {
	ctx := context.Background()
	splits := strings.Split(g.manager.Opts.PullRequest, "/")
	owner := splits[len(splits)-4]
	repoName := splits[len(splits)-3]
	prNum, err := strconv.Atoi(splits[len(splits)-1])
	repo := audit.NewRepo(&g.manager)
	repo.Name = repoName
	log.Infof("auditing pr %s\n", g.manager.Opts.PullRequest)

	if err != nil {
		return
	}
	page := 1
	for {
		commits, resp, err := g.client.PullRequests.ListCommits(ctx, owner, repoName, prNum, &github.ListOptions{
			PerPage: 100, Page: page})
		if err != nil {
			return
		}
		for _, c := range commits {
			c, _, err := g.client.Repositories.GetCommit(ctx, owner, repo.Name, *c.SHA)
			if err != nil {
				continue
			}
			commitObj := object.Commit{
				Hash: plumbing.NewHash(*c.SHA),
				Author: object.Signature{
					Name:  *c.Commit.Author.Name,
					Email: *c.Commit.Author.Email,
					When:  *c.Commit.Author.Date,
				},
			}
			for _, f := range c.Files {
				if f.Patch == nil {
					continue
				}
				audit.InspectString(*f.Patch, &commitObj, repo, *f.Filename)
			}
		}
		page = resp.NextPage
		if resp.LastPage == 0 {
			break
		}
	}
}
