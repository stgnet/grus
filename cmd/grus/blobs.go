package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/stgnet/grus/internal/blob"
	"github.com/stgnet/grus/internal/cluster"
	"github.com/stgnet/grus/internal/store"
)

// Photos travel between nodes outside the Raft logs (they'd make them
// huge). Four pieces keep each node's blob directory complete for the
// groups it holds:
//
//   - pushBlob: the node that receives an upload copies it to the other
//     nodes holding the group (and full-copy nodes) before the post is
//     written, so a photo has more than one copy from the start.
//   - syncBlobs: every node regularly looks for photos its own group files
//     refer to but its disk doesn't have (it was offline, or the group was
//     just placed on it) and fetches them from the other nodes.
//   - fetchNow: a page asks for a photo that syncBlobs hasn't fetched yet;
//     it's fetched on the spot, from a node holding the group.
//   - gcBlobs: once a day, each node deletes photos none of its group
//     files refer to any more (their posts were purged, or the group was
//     taken off the node).

func pushBlob(node *cluster.Node, client *cluster.Client, blobs *blob.Store) func(int64, string) {
	return func(groupID int64, hash string) {
		for _, addr := range node.BlobPeers(groupID) {
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

func fetchNow(node *cluster.Node, client *cluster.Client, blobs *blob.Store) func(int64, string) error {
	return func(groupID int64, hash string) error {
		err := errors.New("no other node has it")
		for _, addr := range node.BlobPeers(groupID) {
			if err = fetchBlob(client, blobs, addr, hash); err == nil {
				return nil
			}
		}
		return err
	}
}

func syncBlobs(ctx context.Context, node *cluster.Node, st *store.Store, blobs *blob.Store, client *cluster.Client) {
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
		sources := node.BlobPeers(0)
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
