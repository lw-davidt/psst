package directory

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"time"

	"github.com/google/go-github/github"
	"github.com/pkg/errors"
	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"
)

const (
	// GHAllTeam containing all users in GitHub
	GHAllTeam = "all"

	ghWorkers      = 10
	contextTimeout = 2 * time.Second
)

// UsersService holds methods used in the GitHub UsersService for easier testing
type UsersService interface {
	Get(context.Context, string) (*github.User, *github.Response, error)
}

// GH hosts a client for accessing GH as well as cached Member and Team lists
type GH struct {
	*github.Client

	UsersService UsersService
	Info
}

// NewGitHub returns an initialized GitHub client to the caller and stored GH members and teams
func NewGitHub(org string) (*GH, error) {
	ctx := context.Background()
	client := &GH{}

	token, ok := os.LookupEnv("GITHUB_TOKEN")
	if !ok {
		return client, errors.New("GITHUB_TOKEN not set")
	}
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := oauth2.NewClient(ctx, ts)
	client.Client = github.NewClient(tc)
	client.UsersService = client.Client.Users
	client.Org = org

	if err := client.getMembersAndTeams(); err != nil {
		return client, err
	}
	return client, nil
}

func (g *GH) getMembersAndTeams() error {
	update := false

	if err := os.MkdirAll(cacheDir, os.ModePerm); err != nil {
		return errors.Wrap(err, "unable to create cache directory")
	}

	membersFile := path.Join(cacheDir, "members")
	mfInfo, err := os.Stat(membersFile)
	if err != nil || time.Since(mfInfo.ModTime()).Minutes() > cacheTTL {
		update = true
	}

	teamsFile := path.Join(cacheDir, "teams")
	tfInfo, err := os.Stat(teamsFile)
	if err != nil || time.Since(tfInfo.ModTime()).Minutes() > cacheTTL {
		update = true
	}

	if update {
		grp, _ := errgroup.WithContext(context.Background())
		grp.Go(func() error {
			if err := g.getMembers(); err != nil {
				return err
			}
			return nil
		})

		grp.Go(func() error {
			if err := g.getTeams(); err != nil {
				return err
			}
			return nil
		})

		if err := grp.Wait(); err != nil {
			return errors.Wrap(err, "unable to get members or teams from GitHub")
		}

		if err := saveCache(membersFile, g.Members); err != nil {
			return errors.Wrap(err, "unable to save members file")
		}
		if err := saveCache(teamsFile, g.Teams); err != nil {
			return errors.Wrap(err, "unable to save teams file")
		}
	} else {
		if err := getCached(membersFile, &g.Members); err != nil {
			return errors.Wrap(err, "unable to get cached members information")
		}
		if err := getCached(teamsFile, &g.Teams); err != nil {
			return errors.Wrap(err, "unable to get cached team information")
		}
	}

	return nil
}

func saveCache(filename string, v interface{}) error {
	buf, err := json.Marshal(v)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("unable to marshal cache file %s", filename))
	}

	if _, err := os.Stat(filename); os.IsExist(err) {
		if err := os.Remove(filename); err != nil {
			return err
		}
	}

	if err := ioutil.WriteFile(filename, buf, 0700); err != nil {
		return errors.Wrap(err, fmt.Sprintf("unable to write cache file %s", filename))
	}
	return nil
}

func getCached(filename string, v interface{}) error {
	_, err := os.Stat(filename)
	if err != nil {
		return err
	}

	buf, err := ioutil.ReadFile(filename)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("unable to read cached file %s", filename))
	}

	if err := json.Unmarshal(buf, v); err != nil {
		return errors.Wrap(err, fmt.Sprintf("unable to unmarshal cache file: %s", filename))
	}

	return nil
}

func (g *GH) getMembers() error {
	members := []Member{}

	in := make(chan string)
	out := make(chan Member)

	// This process can be slow so we speed it up by doing multiple lookups at a time.
	// Was implemented because it took about 45 seconds to get all members and teams and this
	// took it down to about 3 seconds.
	grp, _ := errgroup.WithContext(context.Background())
	for i := 0; i < ghWorkers; i++ {
		grp.Go(func() error {
			for login := range in {
				u, _, err := g.Client.Users.Get(context.Background(), login)
				if err != nil {
					return errors.Wrap(err, fmt.Sprintf("error looking up member %s", login))
				}
				out <- Member{Login: login, Name: u.GetName()}
			}
			return nil
		})
	}

	go func() {
		for mem := range out {
			members = append(members, mem)
		}
	}()

	nextPage := 1
	for nextPage > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
		defer cancel()
		mems, resp, err := g.Client.Organizations.ListMembers(ctx, g.Org, &github.ListMembersOptions{ListOptions: github.ListOptions{Page: nextPage}})
		if err != nil {
			return errors.Wrap(err, "unable to get members from GitHub")
		}

		for _, m := range mems {
			in <- m.GetLogin()
		}

		nextPage = resp.NextPage
	}

	close(in)
	if err := grp.Wait(); err != nil {
		return errors.Wrap(err, "error looking up members")
	}
	close(out)
	ByMembers(sortMemberLogins).Sort(members)
	g.Members = members

	return nil
}

func (g *GH) getTeams() error {
	teams := []Team{}

	in := make(chan *github.Team)
	out := make(chan Team)

	// This process can be slow so we speed it up by doing multiple lookups at a time.
	// Was implemented because it took about 45 seconds to get all members and teams and this
	// took it down to about 3 seconds.
	grp, _ := errgroup.WithContext(context.Background())
	for i := 0; i < ghWorkers; i++ {
		grp.Go(func() error {
			for team := range in {
				mems, err := g.getTeamMembers(team.GetID())
				if err != nil {
					return errors.Wrap(err, fmt.Sprintf("error looking up members of team %s", team.GetName()))
				}
				out <- Team{Name: team.GetName(), Members: mems}

			}
			return nil
		})
	}

	go func() {
		for {
			team, ok := <-out
			if ok {
				teams = append(teams, team)
			} else {
				break
			}
		}
	}()

	nextPage := 1
	for nextPage > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
		defer cancel()
		ts, resp, err := g.Client.Organizations.ListTeams(ctx, g.Org, &github.ListOptions{Page: nextPage})
		if err != nil {
			return errors.Wrap(err, "unable to get teams from GitHub")
		}

		for _, t := range ts {
			in <- t
		}

		nextPage = resp.NextPage
	}
	close(in)
	if err := grp.Wait(); err != nil {
		return errors.Wrap(err, "unable to lookup teams")
	}
	close(out)
	ByTeams(sortTeamNames).Sort(teams)
	g.Teams = teams

	return nil
}

func (g *GH) getTeamMembers(id int64) ([]string, error) {
	members := []string{}
	nextPage := 1

	for nextPage > 0 {
		users, resp, err := g.Client.Organizations.ListTeamMembers(context.Background(), id, &github.OrganizationListTeamMembersOptions{ListOptions: github.ListOptions{Page: nextPage}})
		if err != nil {
			return members, err
		}
		for _, u := range users {
			members = append(members, u.GetLogin())
		}

		nextPage = resp.NextPage
	}

	return members, nil
}

// GetMatches will search for a given value as part of a username or team name and return a set of
// available options for the user.
func (g *GH) GetMatches(lookup string) Matches {
	matches := Matches{}

	if lookup == "*" {
		matches.Members = g.Members
		matches.Teams = g.Teams
		return matches
	}

	for _, m := range g.Members {
		if strings.Contains(strings.ToLower(m.Login), strings.ToLower(lookup)) || strings.Contains(strings.ToLower(m.Name), strings.ToLower(lookup)) {
			matches.Members = append(matches.Members, Member{Login: m.Login, Name: m.Name})
		}
	}

	for _, t := range g.Teams {
		if strings.Contains(strings.ToLower(t.Name), strings.ToLower(lookup)) {
			matches.Teams = append(matches.Teams, Team{Name: t.Name, Members: t.Members})
		}
	}

	return matches
}

// IsMember will check an organization for a specific user
func (g *GH) IsMember(lookup string) bool {
	for _, u := range g.Members {
		if strings.ToLower(lookup) == strings.ToLower(u.Login) {
			return true
		}
	}
	return false
}

// IsTeam will check an organization for a specific team
func (g *GH) IsTeam(lookup string) bool {
	for _, t := range g.Teams {
		if strings.ToLower(lookup) == strings.ToLower(t.Name) {
			return true
		}
	}
	return false
}

// GetTeamMembers returns a list of members for the provided team name
func (g *GH) GetTeamMembers(name string) []string {
	for _, t := range g.Teams {
		if strings.ToLower(name) == strings.ToLower(t.Name) {
			return t.Members
		}
	}
	return []string{}
}

// Whoami returns the login name of the currently authenitcated user
func (g *GH) Whoami() (string, error) {
	user, _, err := g.UsersService.Get(context.Background(), "")
	if err != nil {
		return "", errors.Wrap(err, "unable to get authenticated user's login")
	}
	return *user.Login, nil
}

// GetMembers returns the list of members
func (g *GH) GetMembers() []Member {
	return g.Members
}

// GetTeams returns the list of teams
func (g *GH) GetTeams() []Team {
	return g.Teams
}
