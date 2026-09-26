package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// The operator's page, on any of the domains: the global level. Groups,
// domains and their mail, the global settings, the cluster. Anyone else
// gets a plain 404, so the page's existence isn't advertised.

type adminData struct {
	Groups  []adminGroup
	Domains []store.Domain
	Global  []globalField // the global settings, in store.GlobalKeys order
	Nodes   []adminNode   // the node map (M7); empty on a single node with none registered
	Cluster map[string]string
	Form    map[string]string // values to refill after an error

	AI         []*aiDay
	QueueDue   int  // AI jobs waiting to run now
	QueueLater int  // scheduled for later (waiting for a thread to go quiet)
	SearchUp   bool // a worker is answering searches
}

type adminGroup struct {
	store.Group
	URL string
}

// adminNode is one row of the node map: a node and the groups on it.
type adminNode struct {
	store.Node
	Groups []string // slugs, "(main)" marked
}

// statser is implemented by the cluster node (not by the local test log).
type statser interface{ Stats() map[string]string }

func (s *Server) operator(w http.ResponseWriter, r *http.Request) *store.User {
	u := s.user(r)
	if u == nil || !u.IsOperator {
		s.notFound(w, r)
		return nil
	}
	return u
}

func (s *Server) admin(w http.ResponseWriter, r *http.Request) {
	u := s.operator(w, r)
	if u == nil {
		return
	}
	s.renderAdmin(w, r, u, http.StatusOK, "", nil)
}

func (s *Server) renderAdmin(w http.ResponseWriter, r *http.Request, u *store.User, status int, msg string, form map[string]string) {
	rt := routeOf(r)
	groups, err := s.Store.Groups()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	domains, err := s.Store.Domains()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	raw, err := s.Store.GlobalRaw()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := adminData{Domains: domains, Form: form}
	for _, k := range store.GlobalKeys {
		d.Global = append(d.Global, globalField{Key: k, Value: raw[k]})
	}
	for _, g := range groups {
		d.Groups = append(d.Groups, adminGroup{Group: g, URL: s.groupURL(&g, rt.domain, "/")})
	}
	if st, ok := s.Log.(statser); ok {
		d.Cluster = st.Stats()
	}
	if err := s.adminNodes(&d, groups); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.adminAI(&d); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, status, "admin", &page{Title: "Admin", User: u, Error: msg, Data: d})
}

// aiDay is one day of AI activity, for the admin page: how much searching,
// how often search had to fall back to plain results and why (the signal
// that it's time for more capacity), and how much background work ran.
type aiDay struct {
	Day                       string
	Searches                  int64
	NoWorker, Timeout, Limit  int64
	Errors                    int64
	Jobs                      int64
	JobFailures               int64
	ModelSeconds              float64
	InputTokens, OutputTokens int64
}

func (s *Server) adminAI(d *adminData) error {
	since := s.Now().UTC().AddDate(0, 0, -13).Format("2006-01-02")
	rows, err := s.Store.AIUsage(since)
	if err != nil {
		return err
	}
	byDay := map[string]*aiDay{}
	for _, r := range rows {
		a := byDay[r.Day]
		if a == nil {
			a = &aiDay{Day: r.Day}
			byDay[r.Day] = a
			d.AI = append(d.AI, a)
		}
		switch r.Purpose {
		case "search":
			a.Searches += r.Calls
		case "ask_soft_fail_no_worker":
			a.NoWorker += r.Calls
		case "ask_soft_fail_timeout":
			a.Timeout += r.Calls
		case "ask_soft_fail_limit":
			a.Limit += r.Calls
		case "ask_soft_fail_error":
			a.Errors += r.Calls
		case "ask":
			// the two model calls behind each search
		default:
			a.Jobs += r.Calls
			a.JobFailures += r.Failures
		}
		a.ModelSeconds += r.Seconds
		a.InputTokens += r.InputTokens
		a.OutputTokens += r.OutputTokens
	}
	d.QueueDue, d.QueueLater, err = s.Store.QueueDepth(s.Now().Unix())
	d.SearchUp = s.AI != nil && s.AI.Available()
	return err
}

// adminFail shows a command's error on the admin page. The operator sees
// the error as it is, which is what you want when something's wrong.
func (s *Server) adminFail(w http.ResponseWriter, r *http.Request, u *store.User, err error, form map[string]string) {
	s.renderAdmin(w, r, u, http.StatusBadRequest, capitalize(cmdMessage(err))+".", form)
}

// cmdMessage strips the command name Run puts in front of an error.
func cmdMessage(err error) string {
	msg := err.Error()
	if _, rest, ok := strings.Cut(msg, ": "); ok {
		return rest
	}
	return msg
}

// adminCreateGroup creates a group; the operator becomes its first owner.
// (Self-serve group creation is on the "later" list.)
func (s *Server) adminCreateGroup(w http.ResponseWriter, r *http.Request) {
	u := s.operator(w, r)
	if u == nil {
		return
	}
	form := map[string]string{
		"slug":        strings.ToLower(strings.TrimSpace(r.FormValue("slug"))),
		"name":        strings.TrimSpace(r.FormValue("name")),
		"description": strings.TrimSpace(r.FormValue("description")),
		"visibility":  r.FormValue("visibility"),
	}
	_, err := s.Log.Apply(&cmd.CreateGroup{
		GroupID:     s.IDs.Next(),
		Slug:        form["slug"],
		Name:        form["name"],
		Description: form["description"],
		Visibility:  form["visibility"],
		OwnerID:     u.ID,
		At:          s.Now().Unix(),
	})
	if err != nil {
		s.adminFail(w, r, u, err, form)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// adminDomain lists a domain, or takes one off the list.
func (s *Server) adminDomain(w http.ResponseWriter, r *http.Request) {
	u := s.operator(w, r)
	if u == nil {
		return
	}
	domain := strings.ToLower(strings.TrimSpace(r.FormValue("domain")))
	var c cmd.Command
	switch r.FormValue("action") {
	case "add":
		c = &cmd.AddDomain{Domain: domain, At: s.Now().Unix()}
	case "remove":
		c = &cmd.RemoveDomain{Domain: domain}
	default:
		s.adminFail(w, r, u, errors.New("unknown action"), nil)
		return
	}
	if _, err := s.Log.Apply(c); err != nil {
		s.adminFail(w, r, u, err, map[string]string{"domain": domain})
		return
	}
	// Removing the domain this page is on leaves this redirect with
	// nowhere to go, which is what removing it means.
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// adminDomainMail sets one domain's own mail settings. A blank password
// keeps the one already set (the page never shows it); a blank relay
// clears them all, and the domain uses the global relay.
func (s *Server) adminDomainMail(w http.ResponseWriter, r *http.Request) {
	u := s.operator(w, r)
	if u == nil {
		return
	}
	name := strings.ToLower(strings.TrimSpace(r.FormValue("domain")))
	d, err := s.Store.DomainNamed(name)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if d == nil {
		s.adminFail(w, r, u, errors.New("no domain "+name), nil)
		return
	}
	c := &cmd.SetDomainMail{Domain: name, SMTPHost: r.FormValue("smtp_host"), SMTPUser: r.FormValue("smtp_user"),
		SMTPPass: r.FormValue("smtp_pass"), MailFrom: r.FormValue("mail_from")}
	if p := strings.TrimSpace(r.FormValue("smtp_port")); p != "" {
		if c.SMTPPort, err = strconv.Atoi(p); err != nil {
			s.adminFail(w, r, u, errors.New("the port must be a number"), nil)
			return
		}
	}
	if c.SMTPPass == "" {
		c.SMTPPass = d.SMTPPass
	}
	if strings.TrimSpace(c.SMTPHost) == "" {
		c.SMTPPort, c.SMTPUser, c.SMTPPass = 0, "", ""
	}
	if _, err := s.Log.Apply(c); err != nil {
		s.adminFail(w, r, u, err, nil)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// globalField is one global setting on the admin page.
type globalField struct {
	Key   string
	Value string
}

// adminGlobal saves the global settings form: every key on it. An empty
// field puts the key back to its default, except the SMTP password, which
// the page never shows: empty keeps it, and it's cleared with the relay.
func (s *Server) adminGlobal(w http.ResponseWriter, r *http.Request) {
	u := s.operator(w, r)
	if u == nil {
		return
	}
	raw, err := s.Store.GlobalRaw()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	values := map[string]string{}
	for _, k := range store.GlobalKeys {
		values[k] = r.FormValue(k)
	}
	if values["smtp_pass"] == "" {
		values["smtp_pass"] = raw["smtp_pass"]
	}
	if strings.TrimSpace(values["smtp_host"]) == "" {
		values["smtp_user"], values["smtp_pass"] = "", ""
	}
	if _, err := s.Log.Apply(&cmd.SetGlobal{Values: values}); err != nil {
		s.adminFail(w, r, u, err, nil)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// adminNodes fills in the node map: each node, and the groups placed on it.
func (s *Server) adminNodes(d *adminData, groups []store.Group) error {
	nodes, err := s.Store.Nodes()
	if err != nil {
		return err
	}
	hosts, err := s.Store.GroupHosts(0)
	if err != nil {
		return err
	}
	slugs := map[int64]string{}
	for _, g := range groups {
		slugs[g.ID] = g.Slug
	}
	on := map[string][]string{}
	for _, h := range hosts {
		name := slugs[h.GroupID]
		if name == "" {
			continue
		}
		if h.Voter {
			name += " (main)"
		}
		on[h.NodeID] = append(on[h.NodeID], name)
	}
	for _, n := range nodes {
		d.Nodes = append(d.Nodes, adminNode{Node: n, Groups: on[n.ID]})
	}
	return nil
}

// adminPlace puts a group on a node, or takes it off (plan section 8,
// "Who holds what"). The node copies the group from a node that has it, or deletes
// its copy, by itself.
func (s *Server) adminPlace(w http.ResponseWriter, r *http.Request) {
	u := s.operator(w, r)
	if u == nil {
		return
	}
	slug := strings.ToLower(strings.TrimSpace(r.FormValue("group")))
	node := strings.TrimSpace(r.FormValue("node"))
	form := map[string]string{"place_group": slug, "place_node": node}
	g, err := s.Store.GroupBySlug(slug)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if g == nil {
		s.adminFail(w, r, u, errors.New("no group "+slug), form)
		return
	}
	var c cmd.Command = &cmd.PlaceGroup{GroupID: g.ID, NodeID: node, Voter: r.FormValue("voter") == "on", At: s.Now().Unix()}
	if r.FormValue("action") == "remove" {
		c = &cmd.UnplaceGroup{GroupID: g.ID, NodeID: node, At: s.Now().Unix()}
	}
	if _, err := s.Log.Apply(c); err != nil {
		s.adminFail(w, r, u, err, form)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// adminRemoveNode takes a node that's gone for good out of the map. Any
// group it was the only main host of goes to the replacement.
func (s *Server) adminRemoveNode(w http.ResponseWriter, r *http.Request) {
	u := s.operator(w, r)
	if u == nil {
		return
	}
	node := strings.TrimSpace(r.FormValue("node"))
	repl := strings.TrimSpace(r.FormValue("replacement"))
	if _, err := s.Log.Apply(&cmd.RemoveNode{ID: node, Replacement: repl, At: s.Now().Unix()}); err != nil {
		s.adminFail(w, r, u, err, map[string]string{"remove_node": node, "replacement": repl})
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}
