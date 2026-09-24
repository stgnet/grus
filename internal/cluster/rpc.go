package cluster

import (
	"bytes"
	"context"
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

// RPCHandler serves the internal API for this node. It returns the mux so
// other packages can add their endpoints (the AI worker's /ai/...).
func RPCHandler(n *Node, st *store.Store, blobs *blob.Store) *http.ServeMux {
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
		// applyHere, not Apply: a node that isn't the leader says so rather
		// than forwarding again, so a request can't bounce between nodes.
		v, index, err := n.applyHere(c)
		w.Header().Set("Content-Type", "application/json")
		if errors.Is(err, ErrNotLeader) {
			// Tell the caller where to go instead.
			w.WriteHeader(http.StatusMisdirectedRequest)
			json.NewEncoder(w).Encode(applyReply{Error: err.Error(), Leader: n.LeaderAddr()})
			return
		}
		if err != nil {
			// A command that failed was still appended to the log (it
			// fails the same way everywhere), so the index still matters.
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(applyReply{Error: err.Error(), Input: cmd.IsInput(err), Index: index})
			return
		}
		json.NewEncoder(w).Encode(applyReply{Value: v, Index: index})
	})
	return mux
}

type applyReply struct {
	Value  any    `json:"value,omitempty"`
	Error  string `json:"error,omitempty"`
	Input  bool   `json:"input,omitempty"` // the error is the person's to fix (cmd.IsInput)
	Leader string `json:"leader,omitempty"`
	Index  uint64 `json:"index,omitempty"` // the command's place in the log
}

// decodeValue turns a forwarded command's JSON result back into the Go
// value the command returned. Command results are kept simple (a string,
// an id, a bool) so this is enough: whole numbers come back as int64, not
// JSON's float64, so `v.(int64)` works the same on every node.
func decodeValue(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil {
		return nil
	}
	if n, ok := v.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			return i
		}
		f, _ := n.Float64()
		return f
	}
	return v
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
	raw, _, err := c.apply(addr, cm)
	return raw, err
}

// apply is Apply plus the command's log index, which a forwarding node
// waits for before reading its own copy.
func (c *Client) apply(addr string, cm cmd.Command) (json.RawMessage, uint64, error) {
	data, err := cmd.Encode(cm)
	if err != nil {
		return nil, 0, err
	}
	for tries := 0; tries < 2; tries++ {
		resp, err := c.hc.Post("https://"+addr+"/apply", "application/octet-stream", bytes.NewReader(data))
		if err != nil {
			return nil, 0, err
		}
		var rep struct {
			Value  json.RawMessage `json:"value"`
			Error  string          `json:"error"`
			Input  bool            `json:"input"`
			Leader string          `json:"leader"`
			Index  uint64          `json:"index"`
		}
		err = json.NewDecoder(resp.Body).Decode(&rep)
		resp.Body.Close()
		if err != nil {
			return nil, 0, fmt.Errorf("apply via %s: %s: %v", addr, resp.Status, err)
		}
		if resp.StatusCode == http.StatusMisdirectedRequest && rep.Leader != "" {
			addr = rep.Leader
			continue
		}
		if rep.Error != "" {
			return nil, rep.Index, cmd.Remote(rep.Error, rep.Input)
		}
		return rep.Value, rep.Index, nil
	}
	return nil, 0, ErrNotLeader
}

// PostJSON sends in as JSON to another node's internal API and decodes the
// JSON answer into out. The API's other endpoints (search on a worker, its
// health) are registered by their own packages on the same handler.
func (c *Client) PostJSON(ctx context.Context, addr, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+addr+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.doJSON(req, out)
}

// GetJSON fetches JSON from another node's internal API.
func (c *Client) GetJSON(ctx context.Context, addr, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+path, nil)
	if err != nil {
		return err
	}
	return c.doJSON(req, out)
}

func (c *Client) doJSON(req *http.Request, out any) error {
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %s %s", req.Method, req.URL.Path, resp.Status, bytes.TrimSpace(msg))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}
