package auth

// This file is the one place that decides who may read what. Every read
// path (group pages, posts, comments, images, search, Ask, the FAQ, exports)
// asks CanSeeGroup and CanRead rather than checking visibility itself, so
// there's exactly one rule to get right and to test.

// Viewer is who's asking. The zero Viewer is a signed-out reader.
type Viewer struct {
	UserID   int64
	Operator bool   // site operator: can read everything, everywhere
	Role     string // in this group: owner | mod | member | "" (not a member)
	Status   string // in this group: active | pending | banned | ""
}

// Item is the part of a post or comment the rule needs.
type Item struct {
	AuthorID int64
	Status   string // visible | flagged | auto_hidden | held | removed | deleted
}

func (v Viewer) member() bool { return v.Role != "" && v.Status == "active" }
func (v Viewer) mod() bool    { return v.member() && (v.Role == "owner" || v.Role == "mod") }

// IsMember: an active member of this group.
func IsMember(v Viewer) bool { return v.member() }

// CanModerate: may act as a mod in this group (its owners and mods, and the
// site operator anywhere).
func CanModerate(v Viewer) bool { return v.Operator || v.mod() }

// CanSeeGroup: does the group exist, as far as this viewer can tell?
// Public and private groups show their name, description and rules to
// everyone; hidden groups exist only for their members (and invite links).
func CanSeeGroup(v Viewer, visibility string) bool {
	if v.Operator {
		return true
	}
	if v.Status == "banned" {
		return false
	}
	if visibility == "hidden" {
		return v.member()
	}
	return true
}

// CanRead: may this viewer read this group's content, and (if item isn't
// nil) this particular post or comment?
func CanRead(v Viewer, visibility string, item *Item) bool {
	if v.Operator {
		return true
	}
	if v.Status == "banned" {
		return false
	}
	// Group level: public groups are open to everyone, logged in or not;
	// private and hidden groups only to active members.
	if visibility != "public" && !v.member() {
		return false
	}
	if item == nil {
		return true
	}
	isAuthor := v.UserID != 0 && v.UserID == item.AuthorID
	switch item.Status {
	case "visible", "flagged":
		// Flagged items stay readable, with a label, while members vote.
		return true
	case "auto_hidden", "held":
		// Waiting on a vote or a mod: the author and mods still see it.
		return isAuthor || v.mod()
	case "removed", "deleted":
		// Kept until purge so mods can review appeals and undo mistakes.
		return v.mod()
	default:
		// An unknown status is a bug somewhere; fail closed.
		return false
	}
}

// CanManage: may change the group's settings (its owners, and the site
// operator).
func CanManage(v Viewer) bool {
	return v.Operator || (v.member() && v.Role == "owner")
}

// CanReadFAQ: may this viewer read the group's FAQ? Whoever can read the
// group can. A private group's owner can also make its FAQ a public preview
// (plan section 4, "Privacy"), which shows the entries while the threads
// they link to stay members-only. Hidden groups never preview.
func CanReadFAQ(v Viewer, visibility string, publicFAQ bool) bool {
	if CanRead(v, visibility, nil) {
		return true
	}
	return visibility == "private" && publicFAQ && v.Status != "banned"
}
