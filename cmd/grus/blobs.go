package main

import (
	"context"
	"log"
	"slices"
	"time"

	"github.com/stgnet/grus/internal/blob"
	"github.com/stgnet/grus/internal/cluster"
	"github.com/stgnet/grus/internal/config"
	"github.com/stgnet/grus/internal/store"
)

// Photos travel between nodes outside the Raft log (they'd make it huge).
// Three pieces keep every node's blob directory complete:
//
//   - pushBlob: the node that receives an upload copies it to the leader
//     and its configured peers before the post is written, so a photo has
//     more than one copy from the start.
//   - syncBlobs: every node regularly looks for photos its databases refer
//     to but its disk doesn't have (it was offline, or is new) and fetches
//     them from the leader, or from a peer if it is the leader.
//   - gcBlobs: once a day, each node deletes photos nothing refers to any
//     more (their posts were purged).

func pushBlob(node *cluster.Node, client *cluster.Client, blobs *blob.Store, peers []config.Peer) func(string) {
	return func(hash string) {
		for _, addr := range blobSources(node, peers) {
			for _, thumb := range []bool{false, true} {
				data, err := blobs.Read(hash, thumb)
				if err == nil {
					err = client.PutBlob(addr, hash, thumb, data)
				}
				if err != nil {
					// That node catches up later through syncBlobs.
					log.Printf("blob push to %s: %v", addr, err)
					break
				}
			}
		}
	}
}

// blobSources is the other nodes this one knows about: the leader (unless
// it's us) and the peers in our config. Only the leader's config lists
// peers, so a follower talks to the leader, and the leader to everyone.
func blobSources(node *cluster.Node, peers []config.Peer) []string {
	var out []string
	if !node.IsLeader() {
		if a := node.LeaderAddr(); a != "" {
			out = append(out, a)
		}
	}
	for _, p := range peers {
		if !slices.Contains(out, p.Addr) {
			out = append(out, p.Addr)
		}
	}
	return out
}

func syncBlobs(ctx context.Context, node *cluster.Node, st *store.Store, blobs *blob.Store, client *cluster.Client, peers []config.Peer) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		want, err := st.BlobHashes()
		if err != nil {
			log.Printf("blob sync: %v", err)
			continue
		}
		sources := blobSources(node, peers)
		for hash := range want {
			if blobs.Has(hash) || ctx.Err() != nil {
				continue
			}
			for _, src := range sources {
				if fetchBlob(client, blobs, src, hash) == nil {
					break
				}
			}
		}
	}
}

func fetchBlob(client *cluster.Client, blobs *blob.Store, addr, hash string) error {
	for _, thumb := range []bool{false, true} {
		data, err := client.GetBlob(addr, hash, thumb)
		if err != nil {
			return err
		}
		if err := blobs.Write(hash, thumb, data); err != nil {
			return err
		}
	}
	return nil
}

func gcBlobs(ctx context.Context, st *store.Store, blobs *blob.Store) {
	tick := time.NewTicker(24 * time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		keep, err := st.BlobHashes()
		if err != nil {
			log.Printf("blob gc: %v", err)
			continue
		}
		// A day's grace: a photo is stored a moment before the post that
		// uses it, and on other nodes the post may arrive after the photo.
		n, err := blobs.GC(func(h string) bool { return keep[h] }, 24*time.Hour)
		if err != nil {
			log.Printf("blob gc: %v", err)
		}
		if n > 0 {
			log.Printf("blob gc: removed %d unused photos", n)
		}
	}
}
