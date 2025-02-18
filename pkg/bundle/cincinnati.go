package bundle

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"time"

	"github.com/blang/semver/v4"
	"github.com/google/uuid"
	"k8s.io/klog/v2"
)

// Copied from https://github.com/openshift/cluster-version-operator/blob/release-4.9/pkg/cincinnati/cincinnati.go

const (
	// GraphMediaType is the media-type specified in the HTTP Accept header
	// of requests sent to the Cincinnati-v1 Graph API.
	GraphMediaType = "application/json"

	// Timeout when calling upstream Cincinnati stack.
	getUpdatesTimeout = time.Minute * 60
)

// Client is a Cincinnati client which can be used to fetch update graphs from
// an upstream Cincinnati stack.
type Client struct {
	id        uuid.UUID
	transport *http.Transport
}

// NewClient creates a new Cincinnati client with the given client identifier.
func NewClient(id uuid.UUID, transport *http.Transport) Client {
	return Client{id: id, transport: transport}
}

// Update is a single node from the update graph.
type Update node

// Error is returned when are unable to get updates.
type Error struct {
	// Reason is the reason suggested for the ClusterOperator status condition.
	Reason string

	// Message is the message suggested for the ClusterOperator status condition.
	Message string

	// cause is the upstream error, if any, being wrapped by this error.
	cause error
}

// Error serializes the error as a string, to satisfy the error interface.
func (err *Error) Error() string {
	return fmt.Sprintf("%s: %s", err.Reason, err.Message)
}

// GetUpdates fetches the current and next-applicable update payloads from the specified
// upstream Cincinnati stack given the current version and channel. The next-
// applicable updates are determined by downloading the update graph, finding
// the current version within that graph (typically the root node), and then
// finding all of the children. These children are the available updates for
// the current version and their payloads indicate from where the actual update
// image can be downloaded.
func (c Client) GetUpdates(ctx context.Context, uri *url.URL, arch string, channel string, version semver.Version) (Update, []Update, error) {
	var current Update
	// Prepare parametrized cincinnati query.
	queryParams := uri.Query()
	queryParams.Add("arch", arch)
	queryParams.Add("channel", channel)
	queryParams.Add("id", c.id.String())
	queryParams.Add("version", version.String())
	uri.RawQuery = queryParams.Encode()

	// Download the update graph.
	req, err := http.NewRequest("GET", uri.String(), nil)
	if err != nil {
		return current, nil, &Error{Reason: "InvalidRequest", Message: err.Error(), cause: err}
	}
	req.Header.Add("Accept", GraphMediaType)
	if c.transport != nil && c.transport.TLSClientConfig != nil {
		if c.transport.TLSClientConfig.ClientCAs == nil {
			klog.V(5).Infof("Using a root CA pool with 0 root CA subjects to request updates from %s", uri)
		} else {
			klog.V(5).Infof("Using a root CA pool with %n root CA subjects to request updates from %s", len(c.transport.TLSClientConfig.RootCAs.Subjects()), uri)
		}
	}

	if c.transport != nil && c.transport.Proxy != nil {
		proxy, err := c.transport.Proxy(req)
		if err == nil && proxy != nil {
			klog.V(5).Infof("Using proxy %s to request updates from %s", proxy.Host, uri)
		}
	}

	client := http.Client{}
	if c.transport != nil {
		client.Transport = c.transport
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, getUpdatesTimeout)
	defer cancel()
	resp, err := client.Do(req.WithContext(timeoutCtx))
	if err != nil {
		return current, nil, &Error{Reason: "RemoteFailed", Message: err.Error(), cause: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return current, nil, &Error{Reason: "ResponseFailed", Message: fmt.Sprintf("unexpected HTTP status: %s", resp.Status)}
	}

	// Parse the graph.
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return current, nil, &Error{Reason: "ResponseFailed", Message: err.Error(), cause: err}
	}

	var graph graph
	if err = json.Unmarshal(body, &graph); err != nil {
		return current, nil, &Error{Reason: "ResponseInvalid", Message: err.Error(), cause: err}
	}

	// Find the current version within the graph.
	var currentIdx int
	found := false
	for i, node := range graph.Nodes {
		if version.EQ(node.Version) {
			currentIdx = i
			current = Update(graph.Nodes[i])
			found = true
			break
		}
	}
	if !found {
		return current, nil, &Error{
			Reason:  "VersionNotFound",
			Message: fmt.Sprintf("currently reconciling cluster version %s not found in the %q channel", version, channel),
		}
	}

	// Find the children of the current version.
	var nextIdxs []int
	for _, edge := range graph.Edges {
		if edge.Origin == currentIdx {
			nextIdxs = append(nextIdxs, edge.Destination)
		}
	}

	var updates []Update
	for _, i := range nextIdxs {
		updates = append(updates, Update(graph.Nodes[i]))
	}

	return current, updates, nil
}

func (c Client) GetChannelLatest(ctx context.Context, uri *url.URL, arch string, channel string) (semver.Version, error) {
	// Prepare parametrized cincinnati query.
	queryParams := uri.Query()
	queryParams.Add("arch", arch)
	queryParams.Add("channel", channel)
	queryParams.Add("id", c.id.String())
	uri.RawQuery = queryParams.Encode()

	// Download the update graph.
	req, err := http.NewRequest("GET", uri.String(), nil)
	if err != nil {
		return semver.Version{}, &Error{Reason: "InvalidRequest", Message: err.Error(), cause: err}
	}
	req.Header.Add("Accept", GraphMediaType)
	if c.transport != nil && c.transport.TLSClientConfig != nil {
		if c.transport.TLSClientConfig.ClientCAs == nil {
			klog.V(5).Infof("Using a root CA pool with 0 root CA subjects to request updates from %s", uri)
		} else {
			klog.V(5).Infof("Using a root CA pool with %n root CA subjects to request updates from %s", len(c.transport.TLSClientConfig.RootCAs.Subjects()), uri)
		}
	}

	if c.transport != nil && c.transport.Proxy != nil {
		proxy, err := c.transport.Proxy(req)
		if err == nil && proxy != nil {
			klog.V(5).Infof("Using proxy %s to request updates from %s", proxy.Host, uri)
		}
	}

	client := http.Client{}
	if c.transport != nil {
		client.Transport = c.transport
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, getUpdatesTimeout)
	defer cancel()
	resp, err := client.Do(req.WithContext(timeoutCtx))
	if err != nil {
		return semver.Version{}, &Error{Reason: "RemoteFailed", Message: err.Error(), cause: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return semver.Version{}, &Error{Reason: "ResponseFailed", Message: fmt.Sprintf("unexpected HTTP status: %s", resp.Status)}
	}

	// Parse the graph.
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return semver.Version{}, &Error{Reason: "ResponseFailed", Message: err.Error(), cause: err}
	}

	var graph graph
	if err = json.Unmarshal(body, &graph); err != nil {
		return semver.Version{}, &Error{Reason: "ResponseInvalid", Message: err.Error(), cause: err}
	}

	// Find the all versions within the graph.
	Vers := []semver.Version{}
	for _, node := range graph.Nodes {

		Vers = append(Vers, node.Version)

	}

	semver.Sort(Vers)

	if len(Vers) == 0 {
		return semver.Version{}, &Error{
			Reason:  "NoVersionsFound",
			Message: fmt.Sprintf("no cluster versions found for %q in the %q channel", arch, channel),
		}
	}

	return Vers[len(Vers)-1], nil
}

type graph struct {
	Nodes []node
	Edges []edge
}

type node struct {
	Version  semver.Version    `json:"version"`
	Image    string            `json:"payload"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type edge struct {
	Origin      int
	Destination int
}

// UnmarshalJSON unmarshals an edge in the update graph. The edge's JSON
// representation is a two-element array of indices, but Go's representation is
// a struct with two elements so this custom unmarshal method is required.
func (e *edge) UnmarshalJSON(data []byte) error {
	var fields []int
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}

	if len(fields) != 2 {
		return fmt.Errorf("expected 2 fields, found %d", len(fields))
	}

	e.Origin = fields[0]
	e.Destination = fields[1]

	return nil
}
