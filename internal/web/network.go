package web

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/stgnet/grus/internal/cluster"
	"github.com/stgnet/grus/internal/cmd"
)

// The admin page's Network page (/admin/network): every node, who hears
// from whom, and how far along each node's copy of each file is, with
// anything that needs looking at listed first. Operators only, like the
// rest of /admin. The data comes from the node's reports (see
// cluster/status.go), so any node's page shows the whole network.
//
// Everything is turned into display text here, so the template only lays
// it out. The page refreshes itself every 10 seconds (no scripts).

// networker is implemented by the cluster node (not by the local test log).
type networker interface{ Network() cluster.Network }

type networkData struct {
	Self     string
	At       string
	Warnings []string
	Nodes    []netNodeRow
	Names    []string    // the grid's column (and row) names
	Grid     [][]netCell // Grid[i][j]: how long since Names[i] heard from Names[j]
	Files    []netFileRow
}

type netNodeRow struct {
	ID, Reach, Role, Version, Up, Heard, Rewinds, State string
	Self, Bad                                           bool
	IsAddr                                              bool // Reach is an address (kept on one line), not an explanation
}

type netCell struct {
	Text string
	Bad  bool
}

type netFileRow struct {
	Name    string
	Holders []netCell
	Final   string // how old the point is up to which changes are final
	Bad     bool
}

func (s *Server) adminNetwork(w http.ResponseWriter, r *http.Request) {
	u := s.operator(w, r)
	if u == nil {
		return
	}
	nw, ok := s.Log.(networker)
	if !ok {
		s.render(w, r, http.StatusOK, "admin-network", &page{Title: "Network", User: u, Data: networkData{}})
		return
	}
	groups, err := s.Store.Groups()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	slugs := map[cmd.LogID]string{}
	for _, g := range groups {
		slugs[cmd.LogID(g.ID)] = g.Slug
	}
	w.Header().Set("Refresh", "10")
	s.render(w, r, http.StatusOK, "admin-network", &page{Title: "Network", User: u,
		Data: networkPage(nw.Network(), slugs, s.Now())})
}

// Thresholds for what the page flags. A file whose changes haven't become
// final for this long is waiting on a node that isn't answering.
const finalLate = 5 * time.Minute

func networkPage(nw cluster.Network, slugs map[cmd.LogID]string, now time.Time) networkData {
	d := networkData{Self: nw.Self, At: now.UTC().Format("2006-01-02 15:04:05 UTC")}
	versions := map[string][]string{}

	for _, nd := range nw.Nodes {
		row := netNodeRow{ID: nd.ID, Self: nd.ID == nw.Self, Version: "?", Up: "?", Rewinds: "?", State: "ok"}
		st := nd.Status

		// Where others reach it.
		switch {
		case nd.Addr != "":
			row.Reach, row.IsAddr = nd.Addr, true
		case st != nil && st.PublicIP != "":
			row.Reach = "can't be reached at " + st.PublicIP + ": it does the talking"
		default:
			row.Reach = "can't be reached: it does the talking"
		}

		switch {
		case !nd.InMap:
			row.Role = "not in the node map"
		case nd.Full:
			row.Role = "full copy"
		case nd.Voter:
			row.Role = "serves pages, takes new groups"
		default:
			row.Role = "holds what's placed on it"
		}
		if nd.AI {
			row.Role += ", runs the model"
		}

		switch {
		case row.Self:
			row.Heard = "this node"
		case nd.Never:
			row.Heard = "never"
		default:
			row.Heard = ago(nd.Age)
		}

		if st != nil {
			row.Version = st.Version
			row.Up = ago(now.Sub(time.Unix(st.Started, 0)))
			row.Rewinds = fmt.Sprint(st.Rewinds)
			versions[st.Version] = append(versions[st.Version], nd.ID)
			if st.Refusing != "" {
				row.State, row.Bad = "refusing writes: "+st.Refusing, true
				d.Warnings = append(d.Warnings, nd.ID+" is refusing writes: "+st.Refusing+".")
			}
		}

		// What needs looking at.
		switch {
		case !nd.InMap:
			row.Bad = true
			d.Warnings = append(d.Warnings, nd.ID+" answers but isn't in the node map. It was probably started as a separate site (or copied site.db before the first node had registered); the two merge by themselves within a minute or so while they can reach each other. If this stays, check its join lines.")
		case nd.Never:
			row.Bad = true
			d.Warnings = append(d.Warnings, "Nothing has been heard from "+nd.ID+". If it's gone for good, remove it on the admin page: until then changes wait for it before becoming final.")
		case !nd.Fresh:
			row.Bad = true
			d.Warnings = append(d.Warnings, fmt.Sprintf("%s hasn't been heard from for %s. Everything carries on without it; changes wait for it before becoming final.", nd.ID, ago(nd.Age)))
		}
		if nd.AI && nd.Addr == "" && nd.InMap {
			row.Bad = true
			d.Warnings = append(d.Warnings, nd.ID+" runs the model, but other nodes can't connect to it, so searches get plain results. Forward TCP port 7946 to it (the same port number outside).")
		}
		if st == nil && !nd.Never && !row.Self {
			d.Warnings = append(d.Warnings, nd.ID+" doesn't report its status: it runs an older version. Run make install on it.")
		}
		d.Nodes = append(d.Nodes, row)
	}
	if len(versions) > 1 {
		var parts []string
		for v, ids := range versions {
			sort.Strings(ids)
			parts = append(parts, v+" on "+strings.Join(ids, ", "))
		}
		sort.Strings(parts)
		d.Warnings = append(d.Warnings, "Nodes run different versions ("+strings.Join(parts, "; ")+"). That's fine while upgrading.")
	}

	// Who hears whom.
	for i, nd := range nw.Nodes {
		d.Names = append(d.Names, nd.ID)
		var cells []netCell
		var row []cluster.NetLink
		if i < len(nw.Heard) {
			row = nw.Heard[i]
		}
		for _, c := range row {
			switch {
			case c.Self:
				cells = append(cells, netCell{Text: "·"})
			case !c.Known:
				cells = append(cells, netCell{Text: "?"})
			case !c.Heard:
				cells = append(cells, netCell{Text: "never", Bad: true})
			default:
				cells = append(cells, netCell{Text: ago(c.Ago), Bad: !c.OK})
			}
		}
		d.Grid = append(d.Grid, cells)
	}

	// Files.
	for _, f := range nw.Files {
		row := netFileRow{Name: fileName(f.Log, slugs), Final: "?"}
		var oldest time.Duration
		for _, h := range f.Holders {
			c := netCell{}
			switch {
			case !h.Reported:
				c.Text, c.Bad = h.ID+": not holding it yet", true
			case h.Behind > 0:
				c.Text = fmt.Sprintf("%s: %d behind", h.ID, h.Behind)
			default:
				c.Text = h.ID + ": up to date"
			}
			if h.Waiting > 0 {
				c.Text += fmt.Sprintf(", %d not final", h.Waiting)
			}
			row.Holders = append(row.Holders, c)
			if h.StableAge > oldest {
				oldest = h.StableAge
			}
		}
		if oldest > 0 {
			row.Final = ago(oldest) + " ago"
			if oldest > finalLate {
				row.Bad = true
				d.Warnings = append(d.Warnings, fmt.Sprintf("Changes to %s have only been final up to %s ago: some node holding it isn't being heard from.", row.Name, ago(oldest)))
			}
		}
		d.Files = append(d.Files, row)
	}
	return d
}

// fileName is a file's name for people: the group's address name.
func fileName(l cmd.LogID, slugs map[cmd.LogID]string) string {
	if l == cmd.SiteLog {
		return "site.db (every node)"
	}
	if slug, ok := slugs[l]; ok {
		return slug
	}
	return fmt.Sprintf("group %d", l)
}

// ago is a duration for people: "4s", "3m", "2h", "5d".
func ago(d time.Duration) string {
	switch {
	case d < time.Second:
		return "<1s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
