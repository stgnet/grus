package web

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// Email for notifications and the daily digest (plan section 5). Both are
// off until someone turns them on at /profile.
//
// Sending is a side effect outside the replicated log, so for each group
// only its log's leader does it, and it records what it sent with a
// command on that group's log (MarkEmailed, DigestSent), so a new leader
// carries on where the old one stopped. If the leader dies between sending
// and recording, the next one sends that batch again: a rare duplicate
// email is better than a lost one.
//
// One node usually leads every group, and then a person gets one email for
// all their groups. When groups are led from different nodes, each node
// sends its own groups' part.

const (
	emailEvery = 5 * time.Minute
	// emailQuiet: a notification waits this long before it's emailed, so a
	// burst of replies arrives as one email, and anything read on the site
	// in the meantime isn't emailed at all.
	emailQuiet = 10 * 60
	// digestGap keeps a digest to one a day even if the clock hour is
	// seen twice (a restart, or leadership moving mid-hour).
	digestGap = 20 * 3600
)

// RunMail sends notification emails every few minutes, and the daily
// digest at digestHour UTC, for the groups this node leads.
func (s *Server) RunMail(ctx context.Context, digestHour int) {
	tick := time.NewTicker(emailEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if err := s.SendNotices(); err != nil {
			log.Printf("notification email: %v", err)
		}
		if s.Now().UTC().Hour() == digestHour {
			if err := s.SendDigests(); err != nil {
				log.Printf("digest email: %v", err)
			}
		}
	}
}

// SendNotices emails each person who wants it their waiting notifications,
// all groups in one email.
func (s *Server) SendNotices() error {
	users, err := s.Store.EmailUsers()
	if err != nil {
		return err
	}
	var want []int64
	for id, u := range users {
		if u.NotifyEmail {
			want = append(want, id)
		}
	}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if len(want) == 0 {
		return nil
	}
	primary, err := s.Store.PrimaryDomain()
	if err != nil {
		return err
	}
	groups, err := s.Store.Groups()
	if err != nil {
		return err
	}
	type upTo struct{ group, id int64 }
	lines := map[int64][]noticeView{}
	marks := map[int64][]upTo{}
	now := s.Now().Unix()
	for i := range groups {
		g := &groups[i]
		if !s.leads(g.ID) {
			continue
		}
		list, err := s.Store.PendingEmail(g.ID, now-emailQuiet, want)
		if err != nil || len(list) == 0 {
			continue
		}
		views, err := s.noticeViews(g, primary, list)
		if err != nil {
			return err
		}
		newest := map[int64]int64{}
		for _, v := range views {
			lines[v.UserID] = append(lines[v.UserID], v)
			newest[v.UserID] = max(newest[v.UserID], v.ID)
		}
		for u, id := range newest {
			marks[u] = append(marks[u], upTo{g.ID, id})
		}
	}
	for _, id := range want {
		list := lines[id]
		if len(list) == 0 {
			continue
		}
		subject := fmt.Sprintf("%d new notifications", len(list))
		if len(list) == 1 {
			subject = excerpt(list[0].Text, 120)
		}
		var b strings.Builder
		for _, v := range list {
			fmt.Fprintf(&b, "%s (%s)\n%s\n\n", v.Text, v.Group, v.URL)
		}
		b.WriteString(s.emailFooter(primary))
		if err := s.Mail.Send(users[id].Email, subject, b.String()); err != nil {
			log.Printf("notification email to user %d: %v", id, err)
			continue // left unmarked, so the next pass tries again
		}
		for _, m := range marks[id] {
			if _, err := s.Log.Apply(&cmd.MarkEmailed{GroupID: m.group, UserID: id, UpTo: m.id, At: now}); err != nil {
				return err
			}
		}
	}
	return nil
}

// SendDigests sends the daily digest to everyone who asked for it and
// hasn't had one today: each of their groups' top new posts of the last
// day, and how many notifications are waiting. Nothing new, no email.
// "Today" is kept per group (on the membership), since a group's digest
// is sent by whichever node leads it.
func (s *Server) SendDigests() error {
	users, err := s.Store.EmailUsers()
	if err != nil {
		return err
	}
	primary, err := s.Store.PrimaryDomain()
	if err != nil {
		return err
	}
	groups, err := s.Store.Groups()
	if err != nil {
		return err
	}
	now := s.Now().Unix()
	var ids []int64
	for id := range users {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		u := users[id]
		if u.Digest != cmd.DigestDaily {
			continue
		}
		body, due, err := s.digestBody(u, groups, primary, now)
		if err != nil {
			return err
		}
		if body != "" {
			if err := s.Mail.Send(u.Email, "Today in your groups", body+s.emailFooter(primary)); err != nil {
				log.Printf("digest to user %d: %v", id, err)
				continue
			}
		}
		for _, g := range due {
			if _, err := s.Log.Apply(&cmd.DigestSent{GroupID: g, UserID: id, At: now}); err != nil {
				return err
			}
		}
	}
	return nil
}

// digestBody is one person's digest text, or "" when there's nothing new,
// and the groups it covered (this node leads them, and they haven't had
// today's digest yet), to be marked as sent.
func (s *Server) digestBody(u *store.User, groups []store.Group, primary string, now int64) (string, []int64, error) {
	var b strings.Builder
	var due []int64
	unread := 0
	for i := range groups {
		g := &groups[i]
		if !s.leads(g.ID) {
			continue
		}
		m, err := s.Store.Membership(g.ID, u.ID)
		if err != nil || m == nil || m.Status != "active" || now-m.DigestSentAt < digestGap {
			continue // not a member, the group's file isn't here, or sent today
		}
		due = append(due, g.ID)
		n, _ := s.Store.UnreadCount(g.ID, u.ID)
		unread += n
		posts, err := s.Store.NewTopPosts(g.ID, now-86400, 5)
		if err != nil {
			return "", nil, err
		}
		if len(posts) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s\n", g.Name)
		for _, p := range posts {
			fmt.Fprintf(&b, "  %s (%s)\n  %s\n", p.Title, plural(p.CommentCount, "comment", "comments"),
				s.groupURL(g, primary, fmt.Sprintf("/p/%d", p.ID)))
		}
		b.WriteString("\n")
	}
	if b.Len() == 0 && unread == 0 {
		return "", due, nil
	}
	if unread > 0 {
		fmt.Fprintf(&b, "You have %s: %s\n\n", plural(unread, "unread notification", "unread notifications"),
			s.primaryURL(primary, "/notifications"))
	}
	return b.String(), due, nil
}

func (s *Server) emailFooter(primary string) string {
	return "--\nYou asked for these emails. To change or stop them: " + s.primaryURL(primary, "/profile") + "\n"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
