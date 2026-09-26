# Replication without a leader (design)

Status: proposed, not built. This replaces Raft (internal/cluster) with
replication that keeps working at any number of nodes, down to one, with no
leader, no quorum, no recovery procedure and no backups.

## What it has to do

1. **Any node alone keeps working.** A node that can reach no other node
   still serves every group it holds and accepts every kind of write. No
   read-only mode, ever.
2. **Nothing is lost while a node is alive.** A write is stored on the node
   that accepted it before the person is told it worked, and it's copied to
   the other nodes as soon as they can be reached. The only loss possible
   is a node destroyed before it could pass on its latest writes; with other
   nodes reachable that's a fraction of a second (see "Passing writes on").
3. **Every node ends up the same.** Once nodes can reach each other again,
   directly or through any chain of other nodes, every copy of a file
   converges to the same content, without anyone doing anything.
4. **Growth is just starting a node.** The first node starts empty. Every
   other node starts with a `join` line naming any running node, copies the
   files it should hold, and stays in step. Losing nodes is just losing
   copies: as long as one copy of each file survives anywhere, starting new
   nodes pointed at the survivors rebuilds everything. A node that holds
   everything (the Studio) is the copy of last resort.
5. **The engine doesn't care what's under `data_dir`.** It's a directory.

## The idea in one paragraph

Every write is still a command (internal/cmd), and every node still applies
commands one at a time to its own SQLite files. What changes is who decides
the order. Today a Raft leader does, so nothing can be written without one.
Instead, each command gets a timestamp from the node that accepted it, and
the order is simply "by timestamp". Each node applies commands as they
arrive; when one arrives late (it was made on the far side of a network
split, or on a node that was offline), the node rewinds that file to just
before where the late command belongs and applies everything again in the
right order. Since every node ends up applying the same commands in the
same order, every node ends up with the same files. This is the design of
Bayou (Xerox PARC, 1995), adapted to one log per file, which Grus already
has.

What this keeps: every command's Apply works as it does now. They're
deterministic, carry their own values, may read the file, and may fail.
The rules in internal/cmd/applier.go stay true, with "in the same order"
meaning the timestamp order rather than a leader's.

## The pieces

### Operations

An operation is a command plus where it came from:

```
origin   the node that accepted it (node id)
seq      that node's count of operations on this file: 1, 2, 3, ... no gaps
stamp    a hybrid logical clock reading (below)
file     site, or a group id (cmd.LogOf, as now)
command  the encoded command (cmd.Encode, as now)
```

Every node keeps, per file, every operation it has, in an `ops` table in
that file itself. Adding an operation and applying it happen in the same
SQLite transaction, so a node can never have one without the other. The
`applied` table (today's Raft index) becomes the node's version vector for
the file: for each origin, the highest seq it has.

### The order

Operations on a file are ordered by `(stamp, origin, seq)`. That's a total
order every node computes identically from the operations alone.

The stamp is a hybrid logical clock: the node's wall clock in milliseconds,
but never less than one past the largest stamp the node has seen. So an
operation made after seeing another always sorts after it, whatever the
clocks say. A comment made on a post always sorts after the post, even when
the commenter's node has a slow clock. Clocks only need to be roughly right
(NTP); the clock keeps order correct even if they aren't.

### Applying late operations: rewind and replay

When an operation arrives that sorts after everything the node has applied
to the file (the usual case), it's simply applied.

When it sorts earlier, the node rewinds the file and replays: it restores
the file's last checkpoint (below), then applies every operation after it,
the late one included, in order. Files are per group, so a rewind touches
one group; the replay is a few thousand commands at most in practice, and
commands take well under a millisecond each. A node keeps serving reads
from the old state while it replays into a copy, then swaps the copy in.

A command that fails in the final order fails on every node, and does
nothing on any of them. See "When a write fails after it succeeded" for
what the person who made it sees.

### Checkpoints: how far back a rewind can go

A rewind needs a copy of the file from before the late operation. Keeping
every past state is impossible, so nodes agree, with an operation, on a
point before which nothing can move any more:

- A **checkpoint operation** `Checkpoint{Upto: vector}` says "these
  operations (a version vector) are final". Any node may issue one for a
  file once it has heard, recently, from every node that holds the file
  (their version vectors reach at least `Upto`). It's an ordinary
  operation, ordered and replicated like any other.
- Each node keeps a physical copy of each file as of its latest applied
  checkpoint (SQLite `VACUUM INTO`, as `CopyTo` does now), and deletes the
  operations it covers, apart from a short tail kept for nodes catching up.
- **Late after a checkpoint:** an operation not in a checkpoint's `Upto`
  that would sort before the checkpoint is ordered just after the
  checkpoint instead (its stamp is treated as the checkpoint's). Every node
  does the same, because the rule depends only on the checkpoint operation
  itself. So a node that was offline for a week comes back, and its week of
  writes is applied as if made at the moment the others last checkpointed:
  nothing is lost, nothing old is rewritten, and no rewind goes back
  further than the last checkpoint.
- A node that holds a file but hasn't been heard from for a day stops
  counting when deciding whether a checkpoint can be issued, so one dead
  node can't hold checkpoints back forever. When it comes back, the rule
  above takes care of its writes.

Two partitions may each issue a checkpoint. That's fine: both are
operations; each covers what it names; the rule is applied for each in
order.

### Passing writes on

- **Push:** a node that accepts a write sends it to every reachable node
  that holds the file (the node map says which) straight away.
- **Pull (anti-entropy):** every few seconds, each node swaps version
  vectors with a few other holders of each file it holds, and each sends
  the other what it's missing. So writes spread even through nodes that
  aren't directly connected, and a node that was offline catches up
  without anyone noticing it was gone.
- **Forwarding:** a node that accepts a write for a file it doesn't hold
  (a small VPS answering for a group it doesn't have, when the request
  can't be passed on) keeps the operation in an outbox until some holder
  confirms it has it.
- **Best-effort acknowledgement:** after storing a write locally, the node
  waits up to about a second for one other holder to confirm it has it,
  then answers the person either way. With other nodes reachable, that
  shrinks the "destroyed before passing it on" window to nothing in
  practice. Alone, it answers at once. It never blocks.

### New nodes and snapshots

A node joining, or taking on a group, copies the file's checkpoint and the
operations after it from any holder, then keeps in step by push and pull.
This is today's snapshot install, minus Raft. A node too far behind (its
vector is older than a holder's retained tail) gets a fresh copy the same
way, with its own unshared operations kept and applied on top.

### Node identity

Ids (internal/ids) include a node number, and two nodes with the same
number could make the same id, which multi-writer replication would turn
into lost data. So node numbers are no longer typed into grus.conf: a
joining node is given a free one by the node it joins through, recorded in
the node map. A node that sees its number used by a different node refuses
to accept writes and says so, rather than risk it.

### Side effects: email, the job queue, nightly work

Today "the leader does it" guarantees once. Without a leader, each such
duty goes to one node chosen from what the node map and recent contact
say: the lowest-id holder that has been heard from recently. When nodes
are split, each side may choose its own, so an email could be sent twice.
That's the rule already chosen for email ("a rare duplicate is better than
a lost one"); the records of what was sent (MarkEmailed, DigestSent) are
operations and merge. AI jobs are claimed by operations, so a job may run
twice during a split; results are written the same way either time.

## When a write fails after it succeeded

Most writes can't conflict: posts, comments, photos, votes, reports,
sign-ins and follows all make new rows with new ids, and apply in any
order. The cases that can (a command that succeeds where it was made but
fails in the final order) get a rule each, so that nothing a person wrote
is silently dropped:

- **Names that must be unique** (handles, group addresses): the command
  itself resolves it deterministically: the later one gets the name with a
  suffix (`alice-2`), and the person is told next time they visit.
- **Content added to something that changed meanwhile** (a comment on a
  post that was locked or deleted on the other side): the content is kept,
  held for the mods rather than shown, as a held first post is now.
- **Anything else that fails** is recorded with the reason in a `conflicts`
  table on the file, shown to operators and the group's mods, instead of
  vanishing.

The table below lists every command and its rule.

## Per-command rules

(Filled in from the command inventory.)

## What goes away

- hashicorp/raft and raft-boltdb, `bootstrap`, `voter`, leaders and
  elections, quorum, `grus recover`, the recovery runbook.
- `grus backup`, deploy/grus-backup.sh and the restore steps. Every full
  node is a live, complete copy, and losing all but one node is repaired by
  starting new nodes pointed at it.

## Testing

- The existing command tests, unchanged.
- A convergence test: several nodes in one process, random writes on
  random nodes, random splits and rejoins, nodes stopped and started, then
  everything reconnected. Every copy of every file must end up
  byte-for-byte identical, and every write accepted anywhere must be
  present or recorded in `conflicts`.
- The scenarios by name: one node alone; two nodes split and rejoined; a
  node offline for a simulated week; every node but the Studio destroyed
  and rebuilt from it.

## Upgrading from Raft

Each node's current files become its first checkpoint. The first start of
the new version on each node converts its files; all nodes are upgraded
together (the new version doesn't talk to the old).
