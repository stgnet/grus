package cluster

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/stgnet/grus/internal/blob"
	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// The internal HTTP API on the cluster port (see mux.go). Only cluster
// members can reach it: the TLS layer already required a certificate from
// the cluster CA.
//
//	GET  /blob/{hash}[?thumb=1]   a photo, for a node that's missing it
//	PUT  /blob/{hash}[?thumb=1]   store a photo pushed by the node that received it
//	POST /apply                    submit an encoded command (leader only)
//	GET  /group/{slug}             a group's id, for operator tools

// RPCHandler serves the internal API for this node.
func RPCHandler(n *Node, st *store.Store, blobs *blob.Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /group/{slug}", func(w http.ResponseWriter, r *http.Request) {
		g, err := st.GroupBySlug(r.PathValue("slug"))
		if err != nil || g == nil {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, g.ID)
	})
	mux.HandleFunc("GET /blob/{hash}", func(w http.ResponseWriter, r *http.Request) {
		f, err := blobs.Open(r.PathValue("hash"), r.URL.Query().Get("thumb") == "1")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "image/jpeg")
		io.Copy(w, f)
	})
	mux.HandleFunc("PUT /blob/{hash}", func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
		if err == nil {
			err = blobs.Write(r.PathValue("hash"), r.URL.Query().Get("thumb") == "1", data)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
	})
	mux.HandleFunc("POST /apply", func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c, err := cmd.Decode(data)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		v, err := n.Apply(c)
		w.Header().Set("Content-Type", "application/json")
		if errors.Is(err, ErrNotLeader) {
			// Tell the caller where to go instead.
			w.WriteHeader(http.StatusMisdirectedRequest)
			json.NewEncoder(w).Encode(applyReply{Error: err.Error(), Leader: n.LeaderAddr()})
			return
		}
		if err != nil {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(applyReply{Error: err.Error()})
			return
		}
		json.NewEncoder(w).Encode(applyReply{Value: v})
	})
	return mux
}

type applyReply struct {
	Value  any    `json:"value,omitempty"`
	Error  string `json:"error,omitempty"`
	Leader string `json:"leader,omitempty"`
}

// Client calls other nodes' internal API.
type Client struct {
	hc *http.Client
}

// NewClient makes a client that presents this node's certificate and only
// talks to cluster members.
func NewClient(conf *tls.Config) *Client {
	c := conf.Clone()
	c.NextProtos = []string{rpcProto}
	c.ServerName = clusterName
	return &Client{hc: &http.Client{
		Timeout:   60 * time.Second,
		Transport: &http.Transport{TLSClientConfig: c, MaxIdleConnsPerHost: 4},
	}}
}

func blobURL(addr, hash string, thumb bool) string {
	u := "https://" + addr + "/blob/" + hash
	if thumb {
		u += "?thumb=1"
	}
	return u
}

// GetBlob fetches one size of a photo from another node.
func (c *Client) GetBlob(addr, hash string, thumb bool) ([]byte, error) {
	resp, err := c.hc.Get(blobURL(addr, hash, thumb))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get blob %s from %s: %s", hash, addr, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

// PutBlob pushes one size of a photo to another node.
func (c *Client) PutBlob(addr, hash string, thumb bool, data []byte) error {
	req, err := http.NewRequest(http.MethodPut, blobURL(addr, hash, thumb), bytes.NewReader(data))
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("put blob %s to %s: %s %s", hash, addr, resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}

// GroupID looks a group's id up by its slug.
func (c *Client) GroupID(addr, slug string) (int64, error) {
	resp, err := c.hc.Get("https://" + addr + "/group/" + url.PathEscape(slug))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("no group %q", slug)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(string(b), 10, 64)
}

// Apply submits a command to the node at addr, following one redirect to
// the leader if addr isn't it. It returns the command's result as JSON.
func (c *Client) Apply(addr string, cm cmd.Command) (json.RawMessage, error) {
	data, err := cmd.Encode(cm)
	if err != nil {
		return nil, err
	}
	for tries := 0; tries < 2; tries++ {
		resp, err := c.hc.Post("https://"+addr+"/apply", "application/octet-stream", bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		var rep struct {
			Value  json.RawMessage `json:"value"`
			Error  string          `json:"error"`
			Leader string          `json:"leader"`
		}
		err = json.NewDecoder(resp.Body).Decode(&rep)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("apply via %s: %s: %v", addr, resp.Status, err)
		}
		if resp.StatusCode == http.StatusMisdirectedRequest && rep.Leader != "" {
			addr = rep.Leader
			continue
		}
		if rep.Error != "" {
			return nil, errors.New(rep.Error)
		}
		return rep.Value, nil
	}
	return nil, ErrNotLeader
}
