package web

import (
	"fmt"

	"github.com/stgnet/grus/internal/store"
)

// Arranging a long thread (plan section 2, "Keeping it organized"). The
// comments themselves never change; this only decides the order they're
// shown in and what's folded:
//
//  1. The threads whose comments carry the answer (rank nudges) come first,
//     under the summary.
//  2. The rest of the comments the summary covers fold under "Show the N
//     replies".
//  3. Off-topic stretches (tangent nudges) fold to one line, "8 replies
//     about tire pressure".
//  4. A comment whose advice a later one replaced is marked "Newer
//     information below".
//
// "Show in order" (?order=time) skips all of it: plain chronological
// order, nothing folded, which is always one tap away.

// block is one thing in the comment list: a comment thread, or a folded
// off-topic stretch of them.
type block struct {
	Thread  *thread
	Tangent *tangentBlock
}

type tangentBlock struct {
	About   string
	NudgeID int64
	Threads []thread
}

type summaryView struct {
	store.Note
	Count int // comments folded under it
}

// arrangement is how the comments section is laid out.
type arrangement struct {
	Arranged bool // false = plain chronological order
	Summary  *summaryView
	Answers  []block // threads that carry the answer, shown first
	Folded   []block // covered by the summary, folded under it
	Rest     []block // everything after
}

// nudgeView is a nudge as the mods' "how this thread is arranged" panel
// lists it.
type nudgeView struct {
	store.Nudge
	What string
}

// arrange lays out a post's comment threads from its summary note and
// nudges. plain = the reader asked for chronological order.
func arrange(threads []thread, summary *store.Note, covers map[int64]bool, nudges []store.Nudge, plain bool) arrangement {
	var a arrangement
	if plain || (summary == nil && len(nudges) == 0) {
		a.Rest = blocks(threads, nil)
		return a
	}
	a.Arranged = true
	rank := map[int64]int{}
	superseded := map[int64]int64{}
	var tangents []store.Nudge
	for _, n := range nudges {
		if n.Reversed {
			continue
		}
		switch n.Kind {
		case "rank":
			rank[n.TargetID] = int(n.Value) + 1
		case "superseded":
			superseded[n.TargetID] = n.Value
		case "tangent":
			tangents = append(tangents, n)
		}
	}
	// Mark superseded comments (top-level and replies).
	for i := range threads {
		t := &threads[i]
		t.Comment.NewerID = superseded[t.Comment.ID]
		for j := range t.Replies {
			t.Replies[j].NewerID = superseded[t.Replies[j].ID]
		}
	}
	// A thread carries the answer if its comment or any reply does; it
	// moves up in the order of its best-ranked comment.
	best := func(t thread) int {
		b := 0
		for _, id := range append([]int64{t.Comment.ID}, replyIDs(t)...) {
			if r := rank[id]; r != 0 && (b == 0 || r < b) {
				b = r
			}
		}
		return b
	}
	answers := make([]thread, 0)
	var folded, rest []thread
	for _, t := range threads {
		switch {
		case best(t) != 0:
			answers = append(answers, t)
		case summary != nil && covers[t.Comment.ID]:
			folded = append(folded, t)
		default:
			rest = append(rest, t)
		}
	}
	// Answers in rank order (a stable sort by best rank).
	for i := 1; i < len(answers); i++ {
		for j := i; j > 0 && best(answers[j]) < best(answers[j-1]); j-- {
			answers[j], answers[j-1] = answers[j-1], answers[j]
		}
	}
	a.Answers = blocks(answers, nil)
	a.Folded = blocks(folded, tangents)
	a.Rest = blocks(rest, tangents)
	if summary != nil {
		n := 0
		for _, t := range folded {
			n += 1 + len(t.Replies)
		}
		a.Summary = &summaryView{Note: *summary, Count: n}
	}
	return a
}

func replyIDs(t thread) []int64 {
	var ids []int64
	for _, r := range t.Replies {
		ids = append(ids, r.ID)
	}
	return ids
}

// blocks groups runs of threads that fall inside a tangent's range (by
// comment id, which grows with time) into one folded block.
func blocks(threads []thread, tangents []store.Nudge) []block {
	var out []block
	for i := 0; i < len(threads); i++ {
		t := threads[i]
		var tg *store.Nudge
		for k := range tangents {
			if t.Comment.ID >= tangents[k].TargetID && t.Comment.ID <= tangents[k].Value {
				tg = &tangents[k]
				break
			}
		}
		if tg == nil {
			out = append(out, block{Thread: &threads[i]})
			continue
		}
		tb := &tangentBlock{About: tg.Reason, NudgeID: tg.ID}
		for ; i < len(threads) && threads[i].Comment.ID >= tg.TargetID && threads[i].Comment.ID <= tg.Value; i++ {
			tb.Threads = append(tb.Threads, threads[i])
		}
		i--
		if len(tb.Threads) == 1 {
			// One thread isn't a stretch worth folding.
			out = append(out, block{Thread: &tb.Threads[0]})
			continue
		}
		out = append(out, block{Tangent: tb})
	}
	return out
}

// describeNudges words each nudge for the mods' panel.
func describeNudges(nudges []store.Nudge) []nudgeView {
	var out []nudgeView
	for _, n := range nudges {
		v := nudgeView{Nudge: n}
		switch n.Kind {
		case "rank":
			v.What = "Shown first as carrying the answer"
		case "tangent":
			v.What = fmt.Sprintf("Folded as off-topic (%s)", n.Reason)
		case "superseded":
			v.What = "Marked \"newer information below\""
		case "feed_weight":
			v.What = "Shown lower in the Active feed (it repeats an answered thread)"
		}
		out = append(out, v)
	}
	return out
}

// Count is how many comments a folded stretch holds.
func (tb *tangentBlock) Count() int {
	n := 0
	for _, t := range tb.Threads {
		n += 1 + len(t.Replies)
	}
	return n
}
