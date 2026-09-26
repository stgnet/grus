# Replication without a leader

This replaced Raft (M9). Every node applies every write it can see, in one
order that every node works out for itself, so there is no leader, no
quorum, no election and no recovery procedure. A node that can reach no
other node carries on, reads and writes, and merges with the others when
it can reach them again.

## What it guarantees

1. **Any node alone keeps working.** A node serves every group it holds
   and accepts every kind of write, whether or not it can reach any other
   node. There is no read-only mode.
2. **A write is stored before it's acknowledged.** It's in the accepting
   node's own file (in the same SQLite transaction as its effects) before
   the page says it worked, and it's sent to the other nodes at once. It
   can be lost only if that node is destroyed before any other node heard
   of it.
3. **Every node ends up the same.** Once nodes can reach each other, even
   through a chain of other nodes, every copy of a file converges to the
   same content, by itself.
4. **Growing is starting a node.** The first node starts empty. Every
   other node starts with a `join` line naming any running node, copies
   the files it should hold, and stays in step. Losing nodes is losing
   copies; as long as one copy of a file survives, starting a node pointed
   at it rebuilds the rest. A node that holds everything (the Studio) is
   the copy of last resort.
5. **The engine doesn't care what's under `data_dir`.** It's a directory
   that behaves like a local disk.

## The idea

Every write is still a command (internal/cmd), and every node still applies
commands one at a time to its own SQLite files; the rules in
internal/cmd/applier.go still hold. What changed is who decides the order.

Each command becomes an **operation**, stamped by the node that accepted
it. Operations on a file are applied in stamp order. When an operation
arrives that belongs before ones already applied (it was made during a
split, or on a node that was briefly unreachable), the node **rewinds**
that file to a saved copy from before it and applies everything again in
order. Every node applies the same operations in the same order, so every
node ends up with the same file. (This is the design of Bayou, Xerox PARC,
1995, with one log per file, which Grus already had.)

### Operations

```
origin   who accepted it: the node's id plus an incarnation (the time its
         data directory was created), so a node rebuilt from scratch
         never reuses an old origin's numbers
seq      that origin's count of operations on this file: 1, 2, 3, no gaps
stamp    a hybrid logical clock reading (below)
cause    for a follow-up sent by another operation, that operation's
         identity, so a follow-up sent twice applies once
command  the encoded command
```

A file's `ops` table holds its operations, and its `seqs` table (the
version vector) holds, for each origin, the highest seq it has. An
operation's row and its effects are written in one transaction.

### Order

Operations are ordered by `(stamp, origin, seq)`, a total order any node
computes from the operations alone.

The stamp is a hybrid logical clock: wall-clock milliseconds, but never
less than one more than the largest stamp or clock reading this node has
seen. So something done after seeing something else always sorts after
it, whatever the clocks say. Clocks only need to be roughly right.

A node's own new operation always sorts after everything it has seen, so
it's applied straight away, and the page that made it gets the real
result.

### Stable point, and rewinding

Each node keeps two copies of each file:

- **live** (site.db, groups/<id>/group.db): everything this node has,
  applied in order. Pages read this one.
- **stable** (site.stable.db, groups/<id>/stable.db): only the operations
  up to the file's **stable point**, applied in order.

The stable point is the stamp below which no operation can ever appear
again. Every node regularly tells every other node its clock and how many
of its own operations on each file it has made. A node's clock only moves
forward, so once node X has said "my clock is at T, and I've made N
operations on this file", and this node has all N, nothing more from X
can have a stamp at or below T. The stable point is the smallest such T
over **every node in the node map**, including this one.

When an operation arrives that sorts before the end of the live file,
the node rewinds. It copies the stable file to a new live file, applies
every operation after the stable point in order, and switches pages over
to the new file. Normally the stable point is a few seconds old, so a
rewind replays a handful of operations.

While a node in the map can't be heard from, the stable point waits for
it. Nothing is ever wrongly treated as final, but rewinds get longer and
operations are kept longer. That's the only cost of a node being down.
Taking a dead node out of the map (below) ends it.

Operations up to the stable point are applied to the stable file too. An
operation is deleted once every node in the map has it.

### Passing operations on

- **Push:** a new operation goes straight to every node that holds its
  file and can be reached.
- **Pull:** every few seconds each node swaps reports with a few others
  (what each has), and each sends the other what it's missing. So
  operations spread through chains of nodes, and a node back from being
  offline catches up without anyone noticing it was gone.
- **Best-effort acknowledgement:** after storing a write, the node waits
  up to half a second for one other node that holds the file to confirm
  it has it, then answers either way. It never blocks, and alone it
  answers at once.

A write for a group this node doesn't hold is passed on, as a page
request, to a node that does, as before. A follow-up for such a group
(below) is made here and pushed to its holders.

### Where nodes are

The node map lists each node's address as `<IP>:<cluster port>`. Nodes
have no DNS names of their own (a domain points at whichever nodes serve
pages, not at one node), so each node finds its own address and
registers it; nobody types it in.

1. **Its public IP.** Every report a node gets back says the address its
   request was seen coming from. A node that hasn't heard from anyone yet
   asks the node one of the site's domains leads to (`/sync/whoami` on
   the cluster port): every domain points at a live node, which is all the
   discovery needs. No outside "what's my IP" service is used.
2. **Whether it can be reached there.** It asks another node to connect
   back to that address (`/sync/dialback`); the answer counts only if the
   node that answers has this node's certificate. The first node, with
   nobody to ask, checks whether the IP is on one of its own interfaces,
   as a VPS's is.

It looks again every five minutes, so a new home IP or a port opened on
a router is picked up by itself. `advertise` in the config skips all of
this (a private network).

**A node nobody can reach** (the Studio behind a home router, with no port
forwarded) registers with no address. The others never dial it; it does
the talking. It posts its report to each of them, which gets theirs in
return, pushes what they're missing, and pulls what it's missing, so
operations still flow both ways and it still counts towards the stable
point. The one thing it can't do is answer a request another node starts:
searches and model measurements are sent to a node with a model, so a
Studio that runs the model needs the cluster port forwarded to it for
quick answers (everything else the model does waits in the job queue,
which the Studio pulls from itself).

If no node in the map answers at all (say, every address changed while
this node was off), it tries the site's domains instead, and picks the
map up again from whichever node answers.

### New nodes, and nodes that fell behind

A node copies a file by fetching another node's stable file and the
operations after it. It does this when a group is placed on it, when it
first joins, and when it's missing operations that have been deleted
everywhere else.

### Follow-ups between files

Some commands change another file too (a new group's settings, a sister
pairing ending in both groups). They write the follow-up into their
file's outbox, as before. A follow-up is made into an operation on its
target file once the command that sent it is stable, since a tentative
command could still be reordered. The node on duty for the source file
(below) does this. The operation carries the sending operation's identity
as its `cause`, and a cause that has been applied once is skipped after
that, so a follow-up sent twice still applies once.

### Duties: email, the nightly batch, the purge

The things one node should do for the whole site are done by the **node
on duty**: the node with the lowest id among those it has heard from in
the last minute. For a group's email, it's the node on duty among that
group's holders. During a split each side has one on duty, so an email
could go out twice. A duplicate is better than a lost one, and the
records of what was sent are operations that merge.

### Node numbers

Ids (internal/ids) include a node number, and two nodes with the same
number could make the same id. The node map records each node's number
(`node_num` in its grus.conf). A node that sees its number held by another
node refuses to accept writes and says so in its log and on the admin
page, until one of them is given another number.

### Removing a node that's gone for good

"Remove node" on the admin page records the node's version vector as it
stood then. Its operations beyond that are ignored by everyone, and the
stable point stops waiting for it. If the node was not in fact gone and
comes back, it finds itself removed, sends back the operations that were
ignored (as a new origin, which applies them now, after everything else),
replaces its files with fresh copies, and carries on as a new node.
Nothing it accepted is lost.

## When a write fails after it succeeded

A command that succeeds where it's made can fail when it's applied in its
final place. For example, a comment on a post that was locked on the
other side of a split. A command that fails in its final place fails on
every node, is recorded in the file's `conflicts` table with the reason,
and is shown to operators and the group's mods on their pages, so nothing
anyone wrote disappears without trace.

Most commands can't fail that way. Anything that only adds rows with new
ids (posts, comments, photos, votes, reports, sessions, follows) applies
in any order, and counters and version numbers are the same on every node
because every node applies the same order.

## Per-command rules

Commands keep their Apply code. These are the changes, from reading every
one of them:

| What | Commands | Rule |
|---|---|---|
| Rows referenced by a SQLite row number, which can differ between nodes until a rewind settles the order | jobs (ClaimJob, FailJob and every job result), faq_history (RollbackFAQEntry), nudges (ReverseNudge) | these rows get ids computed from what they are (a job's kind and subject; a nudge's post, kind, target and time; a history row's entry, time and text), so every node gives a row the same id in any order |
| "Everything up to id N" | MarkRead, MarkEmailed | by the notification's time instead of its row number |
| The outbox's "done up to N" | OutboxDone | gone: follow-ups are made from the stable file, and the cause rule makes them apply once |
| Names that must be unique | CreateGroup (slug), SetHandle | the second to claim a name in the final order gets it with a number added (`travato-2`, `alice-2`); the page they made it on already showed the name as free |
| One email, two first sign-ins on two sides of a split | RedeemLogin | the second finds the account already made and uses it; the id the second side made is recorded as another name for the same account, so what was written under it stays theirs |
| Validation that can come out differently in the final order | CreateComment (locked post), JoinGroup (used-up invite), SetRole and LeaveGroup (last owner), UnplaceGroup (last host), Vote, AnswerSister, and the rest of the "rejects on state" checks | fails the same way on every node and is recorded in `conflicts` |
| Leases and version gates | ClaimJob, every job result | unchanged: they're already decided by the file's state, which is the same everywhere in the final order; a job run twice during a split keeps the result that fits |
| Deciding who acts | the daily purge, the nightly FAQ batch, seeding the global settings, email and the digest, copying settings up | the node on duty, not a leader |

## What went away

- hashicorp/raft and raft-boltdb, leaders, elections, quorum, `voter` and
  `bootstrap` in grus.conf, `grus recover` and the recovery runbook.
- `grus backup`, deploy/grus-backup.sh and restoring from backups. Every
  full node is a live, complete copy, and losing all but one node is
  repaired by starting new nodes pointed at it.

## Upgrading from Raft

All nodes are upgraded together. On its first start, each node makes its
current files the starting point: live and stable are the same, with no
operations yet. A node whose copy was behind (a non-voter that hadn't
received the last few Raft entries) would then differ forever, so each
file also records the Raft index it was at. Nodes compare it the first
time they talk, and one that's behind takes the other's copy.

## Testing

- The existing command and web tests, unchanged in what they check.
- Named scenarios: one node alone; two nodes split, both writing, then
  rejoined; a node offline while the others write; a follow-up sent
  twice; a name claimed on both sides of a split; every node but one
  destroyed and rebuilt from it.
- A convergence test: several nodes in one process, random writes on
  random nodes, random splits and rejoins, nodes stopped and restarted.
  Afterwards every copy of every file must hold the same rows, and every
  write accepted anywhere must be present, or recorded in `conflicts`.
