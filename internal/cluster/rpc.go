package cluster

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
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
//	POST /apply                    make a write, on a node that holds its file
//	GET  /group/{slug}             a group's id, for operator tools
//	/sync/...                      replication (sync.go)

// serveRPC starts the node's internal API: the endpoints the cluster itself
// needs. Other packages add theirs to n.RPC(): photos (ServeBlobs), the AI
// worker's /ai/..., the web server's /web/ pass-through.
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
		// Only a node that holds the file makes the write. Any other says
		// so, and the sender tries another, rather than this node passing
		// it on again: a write can't go round in circles.
		if !n.holds(cmd.LogOf(c)) {
			w.WriteHeader(http.StatusMisdirectedRequest)
			json.NewEncoder(w).Encode(applyReply{Error: errNotHeld.Error()})
			return
		}
		v, err := n.apply(c, r.Header.Get(causeHeader))
		if err != nil {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(applyReply{Error: err.Error(), Input: cmd.IsInput(err)})
			return
		}
		json.NewEncoder(w).Encode(applyReply{Value: v})
	})
	n.serveSync(mux)
	n.rpcSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ConnContext: connPeer}
	go n.rpcSrv.Serve(n.rpc)
}

// causeHeader carries a follow-up's cause (cmd.Op.Cause) with /apply.
const causeHeader = "X-Grus-Cause"

// errNotHeld is a node's answer to /apply for a file it doesn't hold.
var errNotHeld = errors.New("that node doesn't hold the file")

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
	Value any    `json:"value,omitempty"`
	Error string `json:"error,omitempty"`
	Input bool   `json:"input,omitempty"` // the error is the person's to fix (cmd.IsInput)
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
	// blocked lists addresses this client acts as if it can't reach: how
	// tests split the network. All node-to-node traffic starts from a
	// client, so two nodes blocking each other are fully apart.
	blocked sync.Map
}

// blockingTransport fails requests to blocked addresses, as an unreachable
// node would.
type blockingTransport struct {
	c    *Client
	next http.RoundTripper
}

func (t blockingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if _, ok := t.c.blocked.Load(r.URL.Host); ok {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("blocked (a test's network split)")}
	}
	return t.next.RoundTrip(r)
}

// NewClient makes a client that presents this node's certificate and only
// talks to cluster members.
func NewClient(conf *tls.Config) *Client {
	c := conf.Clone()
	c.NextProtos = []string{rpcProto}
	c.ServerName = clusterName
	cl := &Client{}
	cl.hc = &http.Client{
		Timeout:   60 * time.Second,
		Transport: blockingTransport{cl, &http.Transport{TLSClientConfig: c, MaxIdleConnsPerHost: 4}},
	}
	return cl
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

// Apply submits a command to the node at addr, which makes the write if
// it holds the command's file. It returns the command's result as JSON.
func (c *Client) Apply(addr string, cm cmd.Command) (json.RawMessage, error) {
	return c.apply(addr, cm, "")
}

// apply is Apply with a follow-up's cause.
func (c *Client) apply(addr string, cm cmd.Command, cause string) (json.RawMessage, error) {
	data, err := cmd.Encode(cm)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, "https://"+addr+"/apply", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if cause != "" {
		req.Header.Set(causeHeader, cause)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var rep struct {
		Value json.RawMessage `json:"value"`
		Error string          `json:"error"`
		Input bool            `json:"input"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		return nil, fmt.Errorf("apply via %s: %s: %v", addr, resp.Status, err)
	}
	if resp.StatusCode == http.StatusMisdirectedRequest {
		return nil, errNotHeld
	}
	if rep.Error != "" {
		return nil, cmd.Remote(rep.Error, rep.Input)
	}
	return rep.Value, nil
}

// PostStream sends in as JSON to another node's internal API and copies
// the answer to out as it arrives, however long it takes (ctx bounds it).
func (c *Client) PostStream(ctx context.Context, addr, path string, in any, out io.Writer) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+addr+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := *c.hc
	hc.Timeout = 0
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %s %s", addr, path, resp.Status, bytes.TrimSpace(msg))
	}
	_, err = io.Copy(out, resp.Body)
	return err
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
