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
)

// The internal HTTP API on the cluster port (see mux.go). Only cluster
// members can reach it: the TLS layer already required a certificate from
// the cluster CA.
//
//	GET  /blob/{hash}[?thumb=1]   a photo, for a node that's missing it
//	PUT  /blob/{hash}[?thumb=1]   store a photo pushed by the node that received it
//	POST /apply                    submit an encoded command to its log's leader
//	GET  /applied/{log}            how far this node has applied a log
//	GET  /group/{slug}             a group's id, for operator tools

// serveRPC starts the node's internal API: the endpoints the cluster itself
// needs (applying a command, how far a log has got, a group's id). Other
// packages add theirs to n.RPC(): photos (ServeBlobs), the AI worker's
// /ai/..., the web server's /web/ pass-through.
func (n *Node) serveRPC() {
	mux := http.NewServeMux()
	n.rpcMux = mux
	st := n.st
	mux.HandleFunc("GET /group/{slug}", func(w http.ResponseWriter, r *http.Request) {
		g, err := st.GroupBySlug(r.PathValue("slug"))
		if err != nil || g == nil {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, g.ID)
	})
	mux.HandleFunc("GET /applied/{log}", func(w http.ResponseWriter, r *http.Request) {
		l, err := cmd.ParseLogID(r.PathValue("log"))
		s := n.shard(l)
		if err != nil || s == nil {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, s.raft.AppliedIndex())
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
		w.Header().Set("Content-Type", "application/json")
		// Apply here only if this node leads the command's log. Otherwise
		// say where to go, rather than forwarding again, so a request
		// can't bounce around between nodes.
		misdirected := func(leader string) {
			w.WriteHeader(http.StatusMisdirectedRequest)
			json.NewEncoder(w).Encode(applyReply{Error: ErrNotLeader.Error(), Leader: leader})
		}
		l := cmd.LogOf(c)
		s := n.shardFor(l)
		if s == nil {
			// Not a log this node holds: point at one that does.
			targets, _ := n.forwardTargets(l, nil)
			if len(targets) == 0 {
				misdirected("")
			} else {
				misdirected(targets[0])
			}
			return
		}
		v, index, err := n.applyLocal(s, c)
		if errors.Is(err, ErrNotLeader) {
			misdirected(s.leaderAddr())
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
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	// The listener closes with the node's port, which ends this Serve.
	go srv.Serve(n.rpc)
}

// RPC is the node's internal API, for other packages to add endpoints to.
func (n *Node) RPC() *http.ServeMux { return n.rpcMux }

// ServeBlobs adds the photo endpoints to a node's internal API.
func ServeBlobs(mux *http.ServeMux, blobs *blob.Store) {
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

// Applied asks a node how far it has applied one log.
func (c *Client) Applied(addr string, l cmd.LogID) (uint64, error) {
	resp, err := c.hc.Get("https://" + addr + "/applied/" + l.String())
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%s doesn't hold log %s", addr, l)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32))
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(string(b), 10, 64)
}

// Transport is the HTTP transport to other nodes' internal API, for the
// web server's pass-through to a node holding a group (web.Server.Proxy).
func (c *Client) Transport() http.RoundTripper { return c.hc.Transport }

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

// Apply submits a command to the node at addr, following it to the leader
// of the command's log if addr isn't it. It returns the command's result as JSON.
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
	for tries := 0; tries < 4; tries++ { // the node asked, and up to three redirects
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
		if resp.StatusCode == http.StatusMisdirectedRequest {
			if rep.Leader == "" || rep.Leader == addr {
				return nil, 0, ErrNotLeader
			}
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
